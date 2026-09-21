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
	"path/filepath"
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
	// Context is the model's window in tokens. Configured rather than probed because the
	// three backend families expose it three different ways (llama.cpp /props, ollama
	// /api/show, cloud APIs not at all) and a wrong guess is worse than none: too low and
	// conversations are compacted that did not need it, too high and the request is silently
	// truncated by the backend, which is the failure nobody notices. Zero disables
	// compaction for this backend.
	Context int `json:"context,omitempty"`
	// CompactAt is the fraction of Context at which compaction triggers. Default 0.6, which
	// leaves room for the reply plus several more turns before it fires again.
	CompactAt float64 `json:"compact_at,omitempty"`

	target *url.URL
	proxy  *httputil.ReverseProxy
}

// voicebox is the server: configuration, the tool client, and the HTTP client
// used for the agentic loop's own upstream calls (the reverse proxies handle
// the pass-through path).
type voicebox struct {
	cfg      *Config
	mcp      *MCPClient
	upstream *http.Client
	// store is nil when sessions are disabled, and every session route stays unregistered in
	// that case -- so the feature is genuinely absent rather than present and erroring.
	store *Store
}

type Config struct {
	Listen   string     `json:"listen"`
	Default  string     `json:"default"`
	Backends []*Backend `json:"backends"`
	// TTSURL is the local Piper service (tts_server.py). Proxied through this
	// origin like everything else, so the page needs no second host and no CORS.
	TTSURL string `json:"tts_url"`
	// MCP, when set, enables tool calling. Empty means the proxy behaves exactly
	// as it did before: no tools, no request parsing.
	MCP MCPConfig `json:"mcp"`

	ttsProxy *httputil.ReverseProxy
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

	mcpClient, err := NewMCPClient(cfg.MCP)
	if err != nil {
		log.Fatalf("mcp: %v", err)
	}
	vb := &voicebox{
		cfg: cfg,
		mcp: mcpClient,
		// No timeout: a tool round plus generation can legitimately run for
		// minutes, and a deadline here would truncate a reply mid-sentence.
		upstream: &http.Client{},
	}

	// Sessions live outside the repo, under the user's data dir, so conversations survive a
	// reinstall and are not accidentally committed. Failure to open the store is NOT fatal:
	// voicebox is useful without persistence, and refusing to start would turn a storage
	// problem into a total outage of a thing that was working yesterday.
	if dir := sessionDir(); dir != "" {
		st, err := NewStore(dir)
		if err != nil {
			log.Printf("[sessions] disabled: %v", err)
		} else {
			vb.store = st
			log.Printf("[sessions] storing conversations in %s", dir)
		}
	}

	mux := http.NewServeMux()
	vb.registerSessionRoutes(mux)

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
			Default   string `json:"default"`
			Build     string `json:"build"`
			ServerTTS bool   `json:"server_tts"`
			Tools     bool   `json:"tools"`
			Backends  []view `json:"backends"`
		}{Default: cfg.Default, Build: buildID(*webDir), ServerTTS: cfg.ttsProxy != nil,
			Tools: mcpClient != nil}
		for _, b := range cfg.Backends {
			out.Backends = append(out.Backends, view{
				ID: b.ID, Label: b.Label,
				NeedsKey: b.APIKeyEnv != "",
				HasKey:   b.APIKeyEnv == "" || os.Getenv(b.APIKeyEnv) != "",
			})
		}
		writeJSON(w, out)
	})

	// Ask a backend what reasoning control it actually supports, rather than
	// offering a fixed list that is wrong for most models.
	mux.HandleFunc("/api/effort", func(w http.ResponseWriter, r *http.Request) {
		b := cfg.find(orDefault(r.URL.Query().Get("backend"), cfg.Default))
		if b == nil {
			http.Error(w, "unknown backend", http.StatusBadGateway)
			return
		}
		writeJSON(w, detectEffort(b, r.URL.Query().Get("model")))
	})

	// Chat completions are HANDLED rather than forwarded, because tool calling is
	// a multi-round conversation the proxy has to drive. Everything else on /v1
	// still passes straight through.
	mux.HandleFunc("/v1/chat/completions", vb.handleChat)

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

	// Server-side speech synthesis. Piper runs 12-18x realtime on the CPU here,
	// which is why it needs no GPU -- the card is usually full of llama-server,
	// and the browser could not obtain a WebGPU adapter even when it was free.
	if cfg.ttsProxy != nil {
		mux.Handle("/tts", cfg.ttsProxy)
		mux.Handle("/tts/", cfg.ttsProxy)
		mux.Handle("/voices", cfg.ttsProxy)
	}

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
	if cfg.TTSURL != "" {
		u, err := url.Parse(cfg.TTSURL)
		if err != nil {
			return nil, fmt.Errorf("tts_url: %w", err)
		}
		cfg.ttsProxy = &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(u); r.Out.Host = u.Host },
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
				log.Printf("[tts] %s: %v", u, err)
				http.Error(w, "tts service unreachable: "+err.Error(), http.StatusBadGateway)
			},
		}
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
//
// It also sets the two headers that make the page CROSS-ORIGIN ISOLATED, which
// is what unlocks SharedArrayBuffer -- and therefore multi-threaded WASM in
// onnxruntime. Without them, speech recognition on the CPU path runs on one
// thread and is slow enough to look hung rather than slow.
//
// COEP is "credentialless" rather than "require-corp" deliberately: require-corp
// demands a CORP header on every cross-origin resource, which the jsdelivr CDN
// and the HuggingFace model files do not send, so it would break the very
// downloads it is meant to speed up. credentialless instead sends those requests
// without credentials, which is exactly right for public static assets.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Embedder-Policy", "credentialless")
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

// buildID identifies the page currently on disk, so the UI can show which
// version is actually loaded. A stale tab reporting an already-fixed error is
// otherwise impossible to tell apart from a fix that did not work.
func buildID(webDir string) string {
	fi, err := os.Stat(webDir + "/index.html")
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%s-%d", fi.ModTime().UTC().Format("0102-1504"), fi.Size()%10000)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// envOf exists so effort.go does not import os just for this.
func envOf(k string) string { return os.Getenv(k) }

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// sessionDir resolves where conversations are kept. XDG_DATA_HOME when set, otherwise the
// conventional ~/.local/share. Empty means no home directory could be determined, and sessions
// stay disabled rather than being written somewhere surprising.
func sessionDir() string {
	if d := os.Getenv("VOICEBOX_SESSIONS"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "voicebox", "sessions")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "voicebox", "sessions")
}

func portOf(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return listen[i:]
	}
	return ":" + listen
}
