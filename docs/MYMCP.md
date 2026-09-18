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

## Intended configuration for voicebox

A **second instance**, so the existing one keeps its powers:

```ini
# ~/.config/systemd/user/mymcp-voicebox.service
ExecStart=/home/bryan/.local/bin/mymcp serve \
  --addr 127.0.0.1:9445 \
  --workspace %h/.local/share/voicebox/scratch \
  --memory-dir %h/.local/share/voicebox/memory \
  --allow-exec=false
```

with its own token (`mymcp token add voicebox`), which also gives it its own
memory namespace. voicebox reaches it from the Go proxy, never from the browser:
mymcp is loopback-bound, and the token must not be in a page any LAN client can
read.

Not yet built — the tool-calling loop in voicebox is still to come. This file
records the decision and the reason so it does not have to be re-derived.
