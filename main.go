// voicebox serves a browser voice interface and proxies it to whichever LLM
// backend you point it at.
//
// It exists because three browser rules collide with a LAN of local model
// servers, and only a same-origin proxy satisfies all three at once:
//
//   - getUserMedia (the microphone) requires a SECURE CONTEXT: https, or
//     localhost. A bare LAN IP is neither.
//   - An https page may not call an http backend (mixed content), so serving
//     the page over TLS while llama-server speaks plain http is a dead end
//     unless the backend is proxied through the same origin.
//   - Cross-origin calls need CORS on every backend. llama-server happens to
//     send it; ollama cloud on a different host still costs a preflight.
//
// Serving the page and the API from ONE origin removes all three problems, and
// it keeps the ollama cloud API key on this side of the wire instead of in a
// browser tab where any extension can read it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

type Backend struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	URL   string `json:"url"`
	// APIKeyEnv names an environment variable holding a bearer token. The token
	// itself is never written to config and never reaches the browser.
	APIKeyEnv string `json:"api_key_env,omitempty"`

	target *url.URL
	proxy  *httputil.ReverseProxy
}

type Config struct {
	Listen   string     `json:"listen"`
	Default  string     `json:"default"`
	Backends []*Backend `json:"backends"`
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	webDir := flag.String("web", "web", "directory holding index.html")
	listen := flag.String("listen", "", "override the configured listen address")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	mux := http.NewServeMux()

	// The browser asks which backends exist. Keys are deliberately absent from
	// this payload -- it reports only whether one is CONFIGURED, so the UI can
	// warn about a missing key without ever handling it.
	mux.HandleFunc("/api/backends", func(w http.ResponseWriter, r *http.Request) {
		type view struct {
			ID       string `json:"id"`
			Label    string `json:"label"`
			NeedsKey bool   `json:"needs_key"`
			HasKey   bool   `json:"has_key"`
		}
		out := struct {
			Default  string `json:"default"`
			Backends []view `json:"backends"`
		}{Default: cfg.Default}
		for _, b := range cfg.Backends {
			out.Backends = append(out.Backends, view{
				ID: b.ID, Label: b.Label,
				NeedsKey: b.APIKeyEnv != "",
				HasKey:   b.APIKeyEnv == "" || os.Getenv(b.APIKeyEnv) != "",
			})
		}
		writeJSON(w, out)
	})

	// /v1/... proxies to the backend named by the X-Voicebox-Backend header (or
	// ?backend=), defaulting to cfg.Default.
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Voicebox-Backend")
		if id == "" {
			id = r.URL.Query().Get("backend")
		}
		if id == "" {
			id = cfg.Default
		}
		b := cfg.find(id)
		if b == nil {
			http.Error(w, fmt.Sprintf("unknown backend %q", id), http.StatusBadGateway)
			return
		}
		b.proxy.ServeHTTP(w, r)
	})

	mux.Handle("/", noCache(http.FileServer(http.Dir(*webDir))))

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: logRequests(mux),
		// No WriteTimeout: a streamed completion is one long response, and a
		// deadline here would truncate it mid-sentence.
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("voicebox on %s (web=%s)", cfg.Listen, *webDir)
	for _, b := range cfg.Backends {
		mark := ""
		if b.APIKeyEnv != "" {
			if os.Getenv(b.APIKeyEnv) == "" {
				mark = fmt.Sprintf("  [%s NOT SET]", b.APIKeyEnv)
			} else {
				mark = fmt.Sprintf("  [%s ok]", b.APIKeyEnv)
			}
		}
		def := " "
		if b.ID == cfg.Default {
			def = "*"
		}
		log.Printf(" %s %-14s %s%s", def, b.ID, b.URL, mark)
	}
	log.Printf("microphone needs a secure context: use http://localhost%s here,", portOf(cfg.Listen))
	log.Printf("or from another machine start Chrome with")
	log.Printf("  --unsafely-treat-insecure-origin-as-secure=http://<this-host>%s --user-data-dir=/tmp/vb", portOf(cfg.Listen))
	log.Fatal(srv.ListenAndServe())
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if len(cfg.Backends) == 0 {
		return nil, fmt.Errorf("no backends configured")
	}
	for _, b := range cfg.Backends {
		u, err := url.Parse(b.URL)
		if err != nil {
			return nil, fmt.Errorf("backend %s: %w", b.ID, err)
		}
		b.target = u
		b.proxy = newProxy(b)
	}
	if cfg.Default == "" {
		cfg.Default = cfg.Backends[0].ID
	}
	if cfg.find(cfg.Default) == nil {
		return nil, fmt.Errorf("default backend %q is not in the list", cfg.Default)
	}
	return &cfg, nil
}

func (c *Config) find(id string) *Backend {
	for _, b := range c.Backends {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func newProxy(b *Backend) *httputil.ReverseProxy {
	p := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(b.target)
			// SetURL keeps the inbound path appended to the target's path. Both
			// llama-server and ollama expose /v1 at the root, so the path is
			// passed through unchanged.
			r.Out.Host = b.target.Host
			if b.APIKeyEnv != "" {
				if k := os.Getenv(b.APIKeyEnv); k != "" {
					r.Out.Header.Set("Authorization", "Bearer "+k)
				}
			}
			// Our own routing header must not travel upstream.
			r.Out.Header.Del("X-Voicebox-Backend")
		},
		// FlushInterval -1 forces an immediate flush per write, which is what
		// makes server-sent events actually stream instead of buffering until
		// the turn ends.
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[proxy] %s -> %s: %v", b.ID, b.target, err)
			http.Error(w, fmt.Sprintf("backend %s unreachable: %v", b.ID, err), http.StatusBadGateway)
		},
	}
	return p
}

// noCache keeps the browser from serving a stale index.html while it is being
// edited. The model weights are cached by transformers.js in the Cache API,
// which this does not touch.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			log.Printf("%s %s [%s]", r.Method, r.URL.Path, r.Header.Get("X-Voicebox-Backend"))
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func portOf(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return listen[i:]
	}
	return ":" + listen
}
