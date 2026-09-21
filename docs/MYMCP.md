# mymcp, and what voicebox may use of it

Recorded 2026-09-18, by reading `~/mymcp` (github.com/brywil/mymcp) rather than
its docs, because the question — can the catalog be subset per caller — is
answered in the dispatch path and not in DESIGN.md.

## What it offers

75 tools, registered at boot into one workspace-confined registry.

| group | n | notable |
|---|---|---|
| system | 15 | info tools, plus the four memory tools |
| github | 14 | issues, PRs, runs — including `gh_pr_merge` |
| filesystem | 13 | read/write/edit/delete/move, confined to `--workspace` |
| tmux/shell | 10 | **`run_command`**, `send_keys`, session control |
| browser | 8 | `navigate`, `evaluate`, `screenshot`, `websocket` |
| http/web | 7 | `http_request`, `web_fetch`, parsers, `web_search_paid` |
| agent pubsub | 5 | `agent_publish` / `subscribe` / `poll` |
| git · thinking · misc · search | 4 | `git_query`, `sequentialthinking`, `wait_for`, `web_search_free` |

**Both tools voicebox wanted already exist**, so neither needs building:

- `web_search_free`, `web_search_paid`
- `memory_save`, `memory_read`, `memory_list`, `memory_search` — already
  **namespaced per principal** at `<MemoryDir>/<principal>/`, so a distinct
  token gives voicebox its own memory without any change.

## The catalog is NOT per-principal

This is the thing worth recording, because the audit log makes it look as though
it might be. From `internal/mcp/server.go`:

```go
case "tools/list":
    return okResponse(req.ID, ToolsListResult{Tools: s.o.Tools.ListTools()}), false
case "tools/call":
    s.o.Logger.Printf("call principal=%q tool=%s", principal, p.Name)
    text, isErr := s.o.Tools.CallTool(WithPrincipal(ctx, principal), p.Name, p.Arguments)
```

`ListTools()` takes no principal. `CallTool` receives it only in the **context**,
so tools can partition their own state — that is how memory and
`sequentialthinking` namespace themselves. It is attribution, not authorisation.
Every valid token sees and can call all 75 tools.

The live unit is `--workspace /home/bryan` with `--allow-exec` defaulting to
true, so a token today carries shell execution and the whole home directory.

## The levers that do exist

1. `--allow-exec=false` — drops github, git and tmux. `run_command` is defined in
   `tmux.go`, so this removes it too. No code change.
2. `--workspace <dir>` — confines the filesystem tools.
3. **`Tool.ReadOnly` is already tagged on 41 of the 75**, commented "eligible for
   the 'ro' capability preset". `Registry.ReadOnly(name)` exists and **has no
   callers**: the preset was designed and never wired up. Filtering `ListTools`
   and `CallTool` behind a `--capabilities ro` flag is small, and the hard part
   — deciding which tools qualify — is already done.

Flags 1 and 2 do **not** cover `http_request` / `web_fetch`, which can reach
`127.0.0.1` (llama-server, mymcp itself), nor `navigate` / `evaluate`, which are
full browser control. For a page reachable unauthenticated from the LAN those
are worth removing too, which is the argument for finishing the `ro` preset
rather than relying on flags.

## Built: a whitelist, and a second instance

`--tools` was added to mymcp (`314a3c7`) and takes an explicit whitelist rather
than a preset to subtract from. `all` and `ro` are accepted as words inside the
list, so `--tools ro,memory_save` reads as "everything read-only, plus the
ability to remember".

**It is enforced in `CallTool`, not just `ListTools`.** Filtering the listing
alone would be theatre — a catalog is a hint, and a model that has seen a tool
name anywhere can simply ask for it. The test that matters asserts a
non-whitelisted tool does not *run*, and was confirmed to fail against a version
that filtered only the listing.

### The six

| tool | why |
|---|---|
| `web_search_free` | browser-backed (Brave via CDP on 9222) — **needs goclaw-chromium** |
| `web_search_paid` | Ollama API, costs per query; the fallback when the browser is down |
| `memory_save` | a WRITE, and therefore outside `ro` — the reason `--tools` takes names |
| `memory_search`, `memory_read` | recall |
| `date_now` | so the model can orient itself in time |

Verified against the running instance: unauthenticated requests get 401, the
listing shows exactly those six, and `run_command`, `read_file`, `http_request`,
`evaluate` and `write_file` are all refused **on dispatch** with "tool not
available". `memory_save` writes to `<memory-dir>/voicebox/MEMORY.md` — its own
principal namespace, isolated from `thor` and `mellum`.

## The configuration, as deployed

A second instance on **9445**, so the existing one on 9443 keeps its powers.
`systemd/mymcp-voicebox.service` in this repo is the unit as deployed; its own
token comes from `mymcp token add voicebox`.

voicebox will reach it **from the Go proxy, never from the browser**: mymcp is
loopback-bound, and the token must not be in a page that anything on the tailnet
can read.

Still to come: the tool-calling loop itself, and speaking tool invocations as
asides in the thinking voice — silence during a search is much worse in audio
than on screen.

## web_fetch added 2026-09-21, and what it costs

The catalog was six tools and the agent could search but not READ: it returned titles and
snippets and had no way to open any of them. Asked to look around, it correctly reported having
"no file-system, browser-execution, or coding harness tools" — the gap was real, not a
misconfiguration.

`web_fetch` is now in the whitelist. Verified against a live page: a Hugging Face model card
came back as 3,711 characters of text.

**IT IS AN SSRF PATH, and "it goes through Ollama's cloud" is not a defence.** The tool tries
Ollama's hosted fetch first, then falls back to a direct GET from THIS machine — a fallback
added for a good reason (HF model pages 404 on the hosted API, and an agent that got the 404
searched eight more times and then invented the document). `directFetch` is a plain
`http.Client` with no address filtering. Demonstrated from the voicebox token:

    web_fetch http://192.168.0.253:8085/v1/models
      -> === Fetched: ... (direct GET) === {"models":[{"name":"/home/bryan/models-prod/...

The label `(direct GET)` is the fallback announcing itself. Anything the Spark can reach is
reachable this way, and voicebox serves :8080 unauthenticated to the LAN and tailnet, so
whoever can open the page can direct the fetch.

Accepted deliberately for now: this is a trusted LAN, and the alternative is an agent that
searches and then cannot read what it found. The fix, if it stops being acceptable, belongs in
mymcp rather than here — `directFetch` should refuse loopback, private and link-local
destinations, which also covers goclaw's instance. Note that instance may legitimately want to
fetch localhost, so the guard wants a flag rather than a hard block.
