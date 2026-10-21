# Sutracoder — Project Rules

## Stack exception

This project does **not** follow the codebox Python/FastAPI backend defaults.

- **Branding:** product, binary, module, and config namespace are all `sutracoder`. The splash wordmark is plain text (` -` macron over the `u`); there is no bitmap logo asset.
- **Language:** Go 1.26+ (static binary, CGO-free; `modernc.org/sqlite`). Dependency tree requires a 1.26 toolchain.
- **TUI:** Bubble Tea / Lip Gloss / Bubbles.
- **LLM access:** `net/http` behind a `Provider` interface. Default provider is **`llamacpp`** — the user's local llama.cpp server (`llama-server`) on the Windows host at `http://127.0.0.1:8080/v1`, reachable from WSL via mirrored networking, OpenAI-compatible, **no key by default** (optional `LLAMACPP_API_KEY`). `marga` (homelab routing proxy, `http://127.0.0.1:8010/v1`) and direct `openai` / `anthropic` / `ollama` adapters are fallbacks, selected via `/connect` or `--provider`.
- **Package management:** Go modules only — no `uv`, no `pyproject.toml`, no `requirements.txt`.
- **APIs:** CLI + library, not FastAPI endpoints. Each tool and provider adapter still carries full doc comments (Go style).
- **Formatting/linting:** `gofmt` + `go vet` + `golangci-lint` in place of Ruff.

## Secrets

- API keys come from environment first (`LLAMACPP_API_KEY` optional, `MARGA_AUTH_TOKEN`, `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`), then the central `worker-env` vault (`GET $VAULT_URL/secrets/<context>/<key>`, `Bearer $VAULT_TOKEN`; context per provider, fallback to default context). `VAULT_URL`/`VAULT_TOKEN` come from env or `~/docker/stack/.env`. Never hardcode or commit keys.

## Scope

- Spec of record: `requirements/initial_prompt.md`. Change the spec before the code diverges from it.
- Headless-first: the `AgentLoop` and its golden tests land before any TUI work (see roadmap in the spec).

## Unchanged from codebox defaults

- Frontend rules do not apply (no web UI).
- Code lives in `~/codebox`; Zed is the editor; commands run via `zsh`.
