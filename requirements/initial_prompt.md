# Project Specification: Sutracoder (Go Edition)
**Goal:** Build a high-performance, agentic CLI coding harness in Go. The tool operates in an iterative "Plan-Act-Observe-Correct" loop, allowing an LLM to explore a codebase, edit files, and execute shell commands to achieve a user-defined goal — under hard resource bounds and explicit safety rules.

**Branding:** The product, binary, module, and config namespace are all `sutracoder`. Logo asset: `assets/sutracoder.jpg` (use in README; no in-TUI rendering for v1).

## 1. Technical Stack (Strict Requirements)
- **Language:** Go 1.26+ (latest stable; for performance and static binary compilation). Note: the Charm/modernc dependency tree currently requires a 1.26 toolchain, so the effective minimum is 1.26 even though 1.24 was the original target.
- **TUI Framework:** `Bubble Tea` (by Charm.sh) for the main loop, `Lip Gloss` for styling, and `Bubbles` for UI components.
- **LLM Integration:** Standard `net/http` behind a `Provider` interface with five named providers:
  - **`llamacpp` (default):** the user's local llama.cpp server (`llama-server`) on the Windows host at `http://127.0.0.1:8080/v1`, reachable from WSL via mirrored networking. OpenAI-compatible surface, streaming supported, **no API key by default** (optional `LLAMACPP_API_KEY` if the server was started with `--api-key`). This is the user's actual LLM runtime and the default.
  - **`marga`:** the homelab LLM routing proxy at `http://127.0.0.1:8010/v1` (OpenAI-compatible, auth via `MARGA_AUTH_TOKEN`). Handles multi-backend routing, failover, and cost control when explicitly selected.
  - **`openai`**, **`anthropic`**, **`ollama`:** direct provider adapters for when a direct connection is explicitly wanted.
  - No heavy frameworks. Adapters normalize provider wire formats (function-calling schemas, streaming events) into one internal `ToolCall` / `AssistantMessage` model.
- **Persistence:** `modernc.org/sqlite` (a CGO-free SQLite port) to ensure the binary is statically linked and portable.
- **File I/O:** Standard `os` and `io` packages; `bufio` for efficient reads.
- **Dependencies:** Every third-party module must justify its footprint; prefer the standard library.

## 2. Core Architecture: The Agentic Loop
An **iterative event loop** (not recursive — no unbounded stack growth). Per turn:
1. **Plan:** The LLM maintains a structured task list (Checklist) via the `update_plan` tool. The checklist is state held by the harness, rendered live in the TUI.
2. **Act:** The LLM selects a Tool to call via JSON function calling.
3. **Observe:** The harness executes the tool, captures STDOUT/STDERR (truncated per §3A), and records the observation.
4. **Correct:** The observation is fed back to the LLM. On failure, the LLM updates its plan and attempts a different approach.

**Loop bounds (hard limits, configurable):**
- `max_turns` (default 50): turns before the loop halts and control returns to the user.
- `max_tokens_budget` (default 200_000 cumulative): tracked from provider `usage` fields; halt on exceed.
- On any hard limit, the loop stops cleanly with a status message; it never silently continues.

**Error taxonomy — two distinct classes:**
- `ToolError` (recoverable): command failed, file missing, malformed arguments. Returned to the LLM as an observation so it can self-correct.
- `FatalError` (loop-stopping): provider auth failure, rate-limit exhaustion after retries, malformed tool-call protocol responses, context-overflow that summarization cannot resolve. These stop the loop and surface to the user; they are never fed back as observations.

**Provider resilience:** Exponential backoff with jitter for 429/5xx responses (bounded retries, e.g. 3 attempts). Streaming responses are mandatory — tokens stream incrementally into the TUI Log panel; a non-streaming fallback is not required for v1.

**Cancellation:** Every tool execution runs under a `context.Context`. The user can interrupt mid-turn (Stop command / Ctrl+C): the in-flight tool is cancelled, partial output is kept as an observation marked `[interrupted]`, and the loop returns to the user. Cancellation is checked between turns and inside long-running tool internals (streaming copy loops, etc.).

## 3. Module Specifications

### A. The Tool Engine (The "Hands")
A `Tool` interface (`Name`, `Description`, `JSONSchema`, `Execute(ctx, args) -> (result, error)`). The LLM has access to:

- **`update_plan(items: [{text, status}])`** — Replaces the agent's checklist. `status ∈ {pending, in_progress, done, failed}`. This is the sole data source for the Plan panel.
- **`ls_recursive(path)`** — Tree view of the directory structure. Skips `node_modules`, `.git`, `bin`, `obj`, and other well-known vendor dirs by default; depth-limited (default 4 levels).
- **`grep_search(query, path?, glob?)`** — Fast search. Binary selection order: `ripgrep` → `grep` → pure-Go fallback (`filepath.WalkDir` + `regexp`). Result count capped.
- **`read_file(path, start_line?, end_line?)`** — Line-ranged reads. Defaults to the first 2000 lines. Output hard-capped (see truncation policy); large files must be paged by the LLM with subsequent calls.
- **`write_file(path, content)`** — Creates or overwrites a file. Creates parent directories as needed. Used for new files and full rewrites.
- **`edit_file(path, search_block, replace_block)`** — Precise search-replace. **Atomic and race-aware:** read file → assert `search_block` appears exactly once → write via temp file + rename. If the block is absent or ambiguous, return a `ToolError` with context; never blind-write.
- **`execute_shell(command, timeout_sec?)`** — Runs via `os/exec` in the workspace root. See §3D for safety rules.
- **`complete_goal(summary)`** — Special tool called when the objective is verified. Ends the loop; the summary is persisted with the session.

**Output truncation policy (all tools):** Each tool result is hard-capped (default 10 KB). Over the cap, keep the head and tail with a `[truncated N bytes]` marker so the agent knows more exists and can page/filter.

### B. TUI Dashboard (The "Eyes")
The tool launches straight into an interactive REPL — the opencode/claude-code
development model. No provider connection is required to start; providers
connect lazily on first use or via `/connect`.

Bubble Tea Model-Update-View, split-screen:
- **Plan Panel (Left):** Live checklist driven by `update_plan` state (`bubbles/list` or custom view).
- **Log Panel (Right):** Scrolling terminal-style view of streaming assistant tokens, tool calls → outputs, and system/command feedback.
- **Status Bar (Bottom):** live from the controller — `[provider/model/mode] [State: Thinking/Streaming/Executing/Idle/Stopped] [Tokens] [Turns] [Session: <id>]`, plus a connected/not-connected indicator.
- **Input Field:** Always available. Free text starts a goal (when idle, creating a new session) or continues the current session (follow-ups preserve context); while a run is in flight it injects a user message at the next turn boundary. Lines beginning with `/` are routed to the slash-command handler.
- **Slash commands** (an opencode-style surface, executed by the controller): `/help`, `/status`, `/connect [provider]` / `/provider <name>`, `/model <name>`, `/mode <autonomous|supervised>`, `/auth`, `/workspace [path]`, `/sessions`, `/resume <id>`, `/new`, `/stop`, `/quit`. Provider/model/mode/workspace switches apply when idle and are refused during a run. `/connect` is asynchronous and reports its result via the log so the UI never blocks on the network.
- All panes must reflow correctly at 80×24 minimum.

### C. Context & State Manager
- **Session Store:** SQLite tables for sessions and turns (prompt → assistant message → tool call → observation → usage). Schema versioned with simple migrations.
- **Session Resume:** `sutracoder --resume [session_id]` reloads history and continues. `sutracoder --sessions` lists recent sessions.
- **Context Windowing:** Sliding window of most-recent turns within the model's limit; when the window overflows, the oldest turns are replaced by a provider-generated summary. Token counting uses the provider `usage` field as ground truth, with a local estimator (chars/4 style) for pre-flight checks.
- **System Prompt:** Versioned editable template file (`prompts/system.md`), baked in as an embed.FS default but overridable via `--system-prompt` or config. Tuning this file is the primary agent-quality lever; it must not be hardcoded in source.

### D. Safety & Control
- **Execution Modes:**
  - `Autonomous`: tools run automatically, but `execute_shell` commands matching the **denylist** (e.g. `rm -rf`, `git push --force`, `sudo`, `mkfs`, writes outside the workspace root) still require `[Y/n]` confirmation.
  - `Supervised`: every `execute_shell` and `edit_file`/`write_file` call requires `[Y/n]` confirmation.
- **Shell safety:** Per-command timeout (default 120s, killable); working directory pinned to the workspace root (path args in all tools resolved and confined there — `..` escapes rejected); output capped per §3A.
- **Graceful Exit:** Ctrl+C → Bubble Tea `tea.Quit` path cancels the root context, waits for in-flight tool teardown (bounded), then closes the SQLite DB.

### E. Interaction Emission (for the downstream self-improvement engine)
Sutracoder is the interaction source for a separate, background **self-improving execution engine** (see `recursive-learn.md`, developed independently). Sutracoder's only obligation is to *emit* its interactions in a machine-readable form that engine can ingest. Learning capabilities (vector memory, heuristic distillation, retrieval) are **out of scope here** — a separate project consumes this data.

- **Structured tool results:** Every tool returns `{status: SUCCESS|FAILURE|INTERRUPTED, stdout, stderr, returncode?, duration_ms, truncated}` — the same feedback shape the downstream engine's `ExecutionSandbox` expects. Shell tools populate `returncode`; read-only tools leave it absent.
- **JSONL sidecar (primary emission surface):** Each completed tool interaction is appended as one JSON object per line to `~/.local/share/sutracoder/interactions/<session_id>.jsonl`. Append-only; an async consumer can `tail -f` / poll it. One record per interaction:
  ```json
  {
    "schema": 1, "session_id": "…", "turn": 7, "ts": "RFC3339",
    "goal": "…", "tool": "edit_file", "args": { … },
    "status": "FAILURE", "stdout": "…", "stderr": "…", "returncode": 1,
    "duration_ms": 12, "truncated": false,
    "tool_error": "search_block not found", "corrected": true,
    "tokens_prompt": 1200, "tokens_completion": 80,
    "session_status": "running|completed|stopped"
  }
  ```
  `corrected` is set on an interaction when a subsequent turn revisits the same task after a failure (the self-correction signal). `args` and `tool_error` are the "generated action" and "precise error trace" the downstream distillation loop needs.
- **SQLite (secondary):** The same structured fields are also stored in the `turns` table so the data survives compaction and is queryable.
- **Schema stability:** The record shape above is a contract; bump `schema` on breaking changes. Emission is fire-and-forget — a failure to write the JSONL log must never interrupt the agent loop (degrade to diagnostics log only).
- **Do not** embed heuristics, embeddings, or retrieval in Sutracoder; that is the consuming engine's job.

## 4. User Stories

**Happy path — "Find the bug in the payment logic and fix it."**
1. **Plan:** `update_plan(["Search for payment", "Read payment.go", "Run tests", "Fix bug"])`.
2. **Act:** `grep_search("Payment")` → finds `internal/pay/processor.go`.
3. **Observe:** `read_file` shows a missing error check on a network call.
4. **Act:** `edit_file` adds the error handling.
5. **Observe:** `execute_shell("go test ./...")` → `FAIL: test_PaymentTimeout`.
6. **Correct:** Agent updates the plan, mocks the server, reruns tests → pass.
7. **Completion:** `complete_goal("Fixed missing error check; added timeout mock; all tests pass")`.

**Fatal path — provider rate limit mid-task.**
1. Agent is 12 turns in; provider returns 429.
2. Harness retries with exponential backoff (3 attempts).
3. Attempts exhausted → `FatalError` → loop halts, state persisted, status bar shows `Stopped: rate limited`.
4. User can `--resume` the session later; no data loss, no runaway retry loop.

## 5. Config, Secrets & Diagnostics
- **Config:** Provider, model, base URL, limits, and mode from a config file (`~/.config/sutracoder/config.toml`) with env-var overrides (`SUTRACODER_*`). Precedence: flags > env > file > defaults. Data lives in `~/.local/share/sutracoder/` (SQLite DB, diagnostics log).
- **Default provider is `llamacpp`** at `http://127.0.0.1:8080/v1` (the local llama.cpp server, no key required). Providers connect **lazily** — the TUI always starts even when no server is up; the first submit (or `/connect`) pings `/health` and reports any failure in the log panel without crashing (no silent fallback to other providers — the user opts in with `/connect openai` or `--provider`).
- **API Keys:** Read from environment only (`LLAMACPP_API_KEY` optional, `MARGA_AUTH_TOKEN`, `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`). Never hardcoded, never logged.
- **Vault integration:** Keys absent from the environment are fetched from the central `worker-env` Cloudflare Worker vault — `GET $VAULT_URL/secrets/<context>/<key>` with `Bearer $VAULT_TOKEN`, returning JSON `{"value": "..."}`. Contexts: the provider's own context first (e.g. `marga/MARGA_AUTH_TOKEN`), then the shared default context. `VAULT_URL`/`VAULT_TOKEN` are read from the environment or, as a fallback, sourced from `~/docker/stack/.env`. Vault lookups happen once at startup, with a short (5s) timeout, and only when the env var is missing; a vault outage degrades to "provider not configured", never to a hang.
- **Diagnostics:** Structured logs (slog) to `~/.local/share/sutracoder/sutracoder.log` (not the TUI log panel) capturing every tool call, raw provider request/response metadata (never keys), and loop decisions — for debugging agent behavior post-hoc.

## 6. Testing & Distribution (Exit Criteria)
- **Unit tests:** Every tool; truncation, path-confinement, and denylist logic covered explicitly.
- **Golden-loop tests:** The `AgentLoop` runs against a fake `Provider` scripted with canned tool calls; assertions on final state, plan contents, and error-taxonomy handling. This suite must pass headless — the loop is developed and tested before the TUI exists.
- **TUI smoke test:** Layout renders at 80×24; viewport scrolling and interrupt path exercised via Bubble Tea's test helpers.
- **CI:** `go vet`, `gofmt` check, `go test ./...` on every push.
- **Distribution:** `goreleaser` produces static binaries (linux/darwin/windows) — static linking is a stated goal and must be verified (`file` / no dynamic deps).

## 7. Implementation Roadmap (headless-first)
1. **Step 1:** Go module init; `Provider` interface + the OpenAI-compatible adapter (serves `llamacpp`, `marga`, `openai`, and `ollama` via base-URL override) with streaming; then the `anthropic` adapter.
2. **Step 2:** Tool engine: `Tool` interface, the 8 tools, truncation policy, path confinement, denylist.
3. **Step 3:** `AgentLoop` headless: Plan → Act → Observe cycle, loop bounds, error taxonomy, cancellation. Golden-loop tests land here.
4. **Step 4:** Provider hardening: retry/backoff, usage accounting, `/health` preflight, vault-based key resolution.
5. **Step 5:** SQLite persistence + `--resume` / `--sessions`.
6. **Step 6:** Bubble Tea TUI: panels, streaming render, interrupt wiring, status bar.
7. **Step 7:** Config/secrets wiring, diagnostics file logging, system-prompt template extraction.
8. **Step 8:** CI + goreleaser; polish Lip Gloss styling.

## 8. Out of Scope (v1)
- Multi-agent / sub-agent orchestration.
- Plugin system for custom tools.
- Non-interactive (headless flag) execution — the loop exists in code, but no `--print` mode yet.
- Windows-native shell quirks (target is WSL/Unix; the binary may compile for Windows but shell safety is validated on Unix).
