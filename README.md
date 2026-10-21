# sutracoder

![sutracoder logo](assets/sutracoder.jpg)

An autonomous agentic CLI coding harness, written in Go. It runs a bounded
**Plan → Act → Observe → Correct** loop against an LLM provider to explore a
codebase, edit files, and run commands toward a user-defined goal — under hard
turn/token limits and explicit safety rules.

- **Fast, portable, static binary** — CGO-free (SQLite via `modernc.org/sqlite`).
- **TUI dashboard** — startup logo, live Plan checklist, streaming tool/observation log, status bar.
- **Default provider: `llamacpp`** — the local llama.cpp server
  (`llama-server`) at `http://127.0.0.1:8080/v1` (OpenAI-compatible, no key
  needed). `marga`, `openai`, `anthropic`, and `ollama` are explicit opt-in
  fallbacks via `/connect` or `--provider`.
- **Structured interaction emission** — every tool call is appended to a JSONL
  sidecar for a downstream self-improvement engine to consume.

## Install

```sh
go install sutracoder/cmd/sutracoder@latest   # from a checkout:
go build -o sutracoder ./cmd/sutracoder
```

## Usage

Launch straight into the TUI — no provider connection is required to start:

```sh
sutracoder                                    # idle REPL, connect on demand
sutracoder "find the bug in the payment logic and fix it"   # seed a goal
sutracoder --provider openai --model gpt-4o-mini "refactor the config loader"
sutracoder --mode supervised "add tests for the parser"
sutracoder --sessions           # list recent sessions
sutracoder --resume <id>        # continue a session
sutracoder --version
```

It is an interactive session, opencode-style: type a goal to run, then keep
talking — follow-ups continue the same session's context. Providers connect
lazily via `/connect`, so the tool always starts even when no proxy is up.

### Slash commands

| Command | Action |
|---------|--------|
| `/help` | show all commands |
| `/status` | provider, model, mode, workspace, session, tokens |
| `/connect [prov]` | connect / switch provider: `marga`, `openai`, `anthropic`, `ollama` |
| `/provider <name>` | alias for `/connect <name>` |
| `/model <name>` | switch the model (idle only) |
| `/mode <m>` | `autonomous` or `supervised` (idle only) |
| `/auth` | check whether the current credential resolves (never prints it) |
| `/workspace [path]` | show or set the workspace root (idle only) |
| `/sessions` | list recent sessions |
| `/resume <id>` | continue a prior session |
| `/new` | start a fresh session |
| `/stop` | stop the current run |
| `/quit` | exit |

### TUI controls

| Key | Action |
|-----|--------|
| type + `Enter` | run a goal, continue a session, or run a `/command` |
| `Ctrl+S` | stop the current run (keep the session) |
| `Ctrl+C` | quit |
| `y` / `n` | answer the confirmation modal for restricted tools |

## Providers & credentials

The default `llamacpp` provider connects to the llama.cpp server at
`http://127.0.0.1:8080/v1` on first use (or via `/connect`). It needs no API key
by default (a local server); if you started `llama-server` with `--api-key`, set
`LLAMACPP_API_KEY`. Other providers resolve credentials from the environment
first, then the central **worker-env** vault:

1. Environment: `LLAMACPP_API_KEY` (optional), `MARGA_AUTH_TOKEN`,
   `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`.
2. Vault: `GET $VAULT_URL/secrets/<context>/<key>` with `Bearer $VAULT_TOKEN`
   (per-provider context, then the shared default context). `VAULT_URL` /
   `VAULT_TOKEN` come from the environment or `~/docker/stack/.env`.

Switch providers at runtime with `/connect <name>` (or `/provider <name>`):
`llamacpp`, `marga`, `openai`, `anthropic`, `ollama`.

## Configuration

`~/.config/sutracoder/config.toml`, overridden by `SUTRACODER_*` env vars and
flags (flags > env > file > defaults):

```toml
provider = "llamacpp"
model    = "local"          # llama.cpp ignores this and uses its loaded model
mode     = "autonomous"   # or "supervised"
max_turns = 50
max_tokens_budget = 200000
max_tool_output_bytes = 10240
shell_timeout_sec = 120
context_limit = 128000
workspace = "."
```

Data (SQLite DB, diagnostics log, interaction logs) lives in
`~/.local/share/sutracoder/`.

## Safety

- **Path confinement** — every tool path is resolved inside the workspace root;
  `..` and absolute escapes are rejected.
- **Shell denylist** — `sudo`, `rm -rf /`, `mkfs`, `git push --force`, `dd`,
  fork bombs, etc. always prompt for confirmation, even in autonomous mode.
- **Supervised mode** — additionally gates `execute_shell`, `edit_file`, and
  `write_file`.
- **Bounded execution** — per-command timeout, output truncation, max turns,
  and a cumulative token budget.

## Interaction emission (for the self-improvement engine)

Sutracoder emits machine-readable interaction records for a separate
background self-improving engine (see `requirements/recursive-learn.md`). Each
tool call is appended as one JSON object per line to
`~/.local/share/sutracoder/interactions/<session>.jsonl`:

```json
{"schema":1,"session_id":"…","turn":7,"ts":"…","goal":"…","tool":"edit_file",
 "args":{…},"status":"FAILURE","stdout":"","stderr":"…","returncode":1,
 "duration_ms":12,"truncated":false,"tool_error":"search_block not found",
 "corrected":true,"tokens_prompt":1200,"tokens_completion":80,"session_status":"running"}
```

Emission is fire-and-forget and never interrupts the agent loop. The same
fields are mirrored into the SQLite `turns` table.

## Development

```sh
go test ./...      # golden-loop, tools, provider, store tests
go vet ./...
gofmt -l .
```

Headless-first: the `AgentLoop` and its golden tests are the source of truth;
the TUI is a thin consumer of agent events.

See `requirements/initial_prompt.md` for the full specification.
