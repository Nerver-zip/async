# AGENTS.md — Agent Development Guide for `async`

Read this document and `docs/PLAN.md` before writing or modifying any code.

## 1. Repository Mission

Build and maintain `async` (Agent Sync), a fast, robust, and self-contained CLI tool and single static binary for synchronizing AI agent conversation sessions (transcripts, SQLite databases, rollout files, brain artifacts, and subagent trees) between local workstations and remote development environments/sandboxes over SSH.

Supported agent providers:
- **OpenAI Codex CLI (`codex`)** (`~/.codex`)
- **Antigravity CLI (`agy`)** (`~/.gemini/antigravity-cli`)

---

## 2. Core Architectural Constraints

1. **Strict Language Rule**:
   All code, documentation, inline comments, commit messages, CLI outputs, and pull requests must be written in **English**.

2. **Single Static Binary**:
   - Written in Go (Go 1.24+).
   - Compiled with `CGO_ENABLED=0` using modern pure-Go SQLite (`modernc.org/sqlite`).
   - Zero dynamic library dependencies (`glibc` independent). The exact same compiled binary must run seamlessly on Arch Linux, Debian, Alpine, and Ubuntu containers without external runtime prerequisites (no `rsync`, no `sqlite3` CLI needed on the target).

3. **Surgical Data Extraction**:
   - **Never** transfer whole database files when synchronizing a session (e.g. laptop `thread_history_1.sqlite` is ~1 GB).
   - Only query and export records belonging directly to the specified conversation and its spawned subagents (`WHERE thread_id IN (...)`).
   - For AGY, copy the individual `conversations/<id>.db`, the `brain/<id>/` folder, and surgically upsert the metadata into `conversation_summaries.db`.

4. **Mutual Exclusion & Active Lock Safety**:
   - Before reading or writing, **always** perform non-blocking kernel lock verification (`flock` with `LOCK_EX` / `LOCK_NB`) on provider lock files:
     - Codex: `~/.codex/thread-writer-locks/<id>.lock`
     - AGY: `~/.gemini/antigravity-cli/presence/<id>.lock`
   - If an active lock is detected on either the source or destination, the sync operation must immediately abort with a descriptive error and non-zero exit code. Never corrupt active agent state.

5. **Conflict & Stale Overwrite Protection**:
   - Compare conversation state monotonically (`updated_at` timestamp, step count) between source and destination.
   - Refuse to overwrite a destination conversation that has a newer timestamp or more steps unless the user explicitly passes `--force`.

6. **Network Topology & Transport**:
   - Strictly respect the firewall boundary of sandboxed environments (e.g. isolated containers blocking outbound connections to host/LAN/tailnet).
   - All network traffic flows over a standard SSH connection initiated by the client:
     `ssh <targethost> ~/.local/bin/async _rpc <subcommand>`
   - Binary data and RPC commands stream cleanly over SSH `stdin` / `stdout`.

7. **Workspace Path Translation & Ambiguity Safety**:
   - Support path translation between environments using customizable workspace base prefixes (e.g. Host 1: `~/projects`, Host 2: `~/workspace` or `~/dev`).
   - Match by project directory name relative to the base prefix.
   - If multiple directories with the same project name are found under the remote prefix, **immediately abort** with an ambiguity error prompting the user to supply `--target-path` explicitly or provide `--id`.
   - Rewrite absolute workspace paths inside serialized metadata (`threads.cwd`, `rollout_path`, `workspace_uris`) upon import so resumed sessions work seamlessly on the target.

8. **Atomic Imports**:
   - Database inserts and updates must be wrapped in atomic transactions.
   - Filesystem updates (rollout files, brain artifacts) must be unpacked into a temporary staging directory and atomically moved into place.

---

## 3. Git & Commit Conventions

- Use Conventional Commits strictly:
  - `feat(...)`: New feature or CLI capability
  - `fix(...)`: Bug fix or edge-case handling
  - `docs(...)`: Documentation, guides, specifications
  - `refactor(...)`: Code restructuring without behavioral change
  - `test(...)`: Unit, integration, or end-to-end tests
  - `chore(...)`: Tooling, build scripts, dependency updates
- Maintain small, cohesive, and well-divided commits. Do not lump unrelated changes together.

---

## 4. Definition of Done

A task or feature in `async` is only complete when:
- It adheres strictly to the architectural constraints outlined above;
- It compiles cleanly with `CGO_ENABLED=0 go build -o bin/async ./cmd/async`;
- All unit and integration tests pass via `go test ./...`;
- Path translation correctly handles both symmetric and divergent workspace structures;
- Active lock verification prevents concurrent writes on both Codex and AGY sessions;
- Surgical SQLite extraction and insertion leave unaffected threads untouched;
- Deployed and validated end-to-end between local host and remote container via SSH.
