# `async` (Agent Sync) — Architecture, Specification & Implementation Plan

A self-contained CLI tool and static binary designed to synchronize AI agent conversation sessions (transcripts, state databases, rollout logs, brain artifacts, and subagent trees) between local workstations and remote development environments/sandboxes over SSH (e.g. Workstation $\leftrightarrow$ Remote Sandbox).

---

## 1. Overview & Workflow

The core problem `async` solves is session mobility across environments without full-machine state duplication:
1. Start an agent task on your workstation in `~/projects/my-project`.
2. Push the conversation state to an isolated sandbox (e.g. sandbox container running on a remote server).
3. Resume the conversation remotely (`codex resume` or `agy -c`), allowing the agent to run dangerous commands, build containers, or run benchmarks inside the sandbox.
4. Pull the updated conversation back to the workstation to continue work locally with full context, transcripts, and plan history.

### Core CLI Usage

```bash
# 1. Automatic workspace detection: synchronizes the latest conversation for the current project directory
async codex push sandbox
async agy push sandbox

# 2. Pull the latest conversation for the corresponding remote workspace directory
async codex pull sandbox
async agy pull sandbox

# 3. Explicit conversation ID (supports both --id <uuid> and direct --<uuid> flag syntax)
async codex push --id 01a127cf-7325-7a93-a4fc-63d244f874b5 sandbox
async codex push --01a127cf-7325-7a93-a4fc-63d244f874b5 sandbox
async agy pull --id e1e555ab-8771-4848-bb6c-5404c45a9cd6 sandbox

# 4. Explicit remote target path override (when paths diverge beyond standard prefix mapping)
async codex push --target-path /home/user/workspace/custom-project sandbox
async codex pull --target-path /home/user/workspace/custom-project sandbox

# 5. List conversations associated with current workspace (or --all) on local or remote
async codex ls [sandbox] [--all]
async agy ls [sandbox] [--all]

# 6. Status and lock inspection (shows sync drift, last modified time, step count, and active lock state)
async status [sandbox]

# 7. One-step binary deployment: cross-copies the static binary to remote ~/.local/bin/async via SSH
async deploy sandbox
```

---

## 2. Technical Findings & Storage Internals

Direct inspection of `~/.codex` and `~/.gemini/antigravity-cli` on both host and sandbox environments revealed the exact storage formats and constraints:

### 2.1. OpenAI Codex CLI (`~/.codex`)

| Component | Path in `~/.codex` | Role & Critical Details |
| :--- | :--- | :--- |
| **Rollout Log** | `sessions/YYYY/MM/DD/rollout-<timestamp>-<thread_id>.jsonl` | Raw session event log. The 1st JSON line contains `id`, `cwd`, and `runtime_workspace_roots`. |
| **State Index** | `state_5.sqlite` (`threads` table) | Primary metadata used by `codex resume`: `id`, `rollout_path` (absolute path), `cwd` (workspace directory), `title`, `updated_at`, `history_mode` (`paginated` vs `legacy`), `thread_source` (`user` vs `subagent`). |
| **Subagent Hierarchy** | `state_5.sqlite` (`thread_spawn_edges`) | Parent $\to$ child links for subagents (`parent_thread_id`, `child_thread_id`). Auto-detection MUST filter top-level threads (`thread_source = 'user'`), but sync packaging MUST recursively bundle child threads. |
| **Paginated History** | `thread_history_1.sqlite` | Tables: `thread_turns`, `thread_items`, `thread_history_projection_state`, `thread_realtime_items`. **Crucial:** On host, this database is ~1 GB. Full file transfer is prohibited; only rows matching `WHERE thread_id IN (...)` are extracted and merged. |
| **Auxiliary Data** | `goals_1.sqlite`, `memories_1.sqlite`, `attachments/<id>` | Auxiliary goal stages, memories, and attachment files referenced by thread ID. |
| **Native Writer Lock** | `thread-writer-locks/<thread_id>.lock` | OS kernel lock held via `flock(fd, LOCK_EX)`. Used for non-blocking conflict prevention. |

### 2.2. Antigravity CLI (`~/.gemini/antigravity-cli`)

| Component | Path in `~/.gemini/antigravity-cli` | Role & Critical Details |
| :--- | :--- | :--- |
| **Conversation DB** | `conversations/<id>.db` (+ `-wal`, `-shm`) | Self-contained SQLite database per conversation. Contains `trajectory_meta`, `steps`, `gen_metadata`, `executor_metadata`, `parent_references`, `trajectory_metadata_blob`. |
| **Brain Artifacts** | `brain/<id>/` | Markdown plans, uploaded images, scratch files, and structured logs (`.system_generated/logs/transcript.jsonl`, `steps/`, `tasks/`). |
| **Summaries Index** | `conversation_summaries.db` | Table `conversation_summaries`: `conversation_id`, `title`, `preview`, `step_count`, `last_modified_time`, `workspace_uris` (JSON array: `["file:///home/user/projects/async"]`), `parent_conversation_id`. |
| **Protobuf Cache** | `jetbox_summaries_proto.pb` | Binary cache checked against `conversations/<id>.db` `mtime`. AGY revalidates automatically on launch. |
| **Annotations / Implicit** | `annotations/<id>.pbtxt`, `implicit/<id>.pb` | Per-conversation protobuf metadata when present. |
| **Native Presence Lock**| `presence/<id>.lock` | OS kernel lock held via `flock(fd, LOCK_EX)`. Held while AGY is active on that conversation. |

---

## 3. Architecture & Design Principles

### 3.1. Network Topology & Sandbox Firewall Constraints

Sandboxed environments often run a strict egress firewall:
- Connections from the sandbox outward to host, LAN, or other networks are blocked.
- Inbound connections over SSH (`workstation -> ssh <targethost>`) are permitted.
- Therefore, `async` adopts a **client-initiated SSH RPC model**:
  - The client runs `ssh <targethost> ~/.local/bin/async _rpc <subcommand>`.
  - RPC queries (finding conversations, checking locks, comparing timestamps) run over standard JSON request/response on SSH `stdio`.
  - Data transfer (surgical SQLite dumps and artifact archives) is streamed over SSH `stdout` as compressed tarballs (`tar.gz`).
  - No reverse tunnels, daemon listeners, or open ports are required inside the container.

### 3.2. Prefix-Based Workspace Path Translation

Workspace directory trees commonly differ across machines:
- **Local machine (Workstation)**: `~/projects/<project>`
- **Remote machine (`sandbox`)**: `~/workspace/<project>` or `~/dev/<project>`

#### Path Mapping Rules:
1. **Explicit Target Path (`--target-path <path>`)**:
   If provided, always takes precedence.
2. **Configurable Base Prefixes**:
   Users can configure base prefixes in `~/.config/async/config.toml` or rely on sane defaults:
   - Local prefix: `~/projects`
   - Remote target prefixes: configured per host (e.g. `sandbox = "/home/user/workspace"`, `remote-server = "/home/user/dev"`).
3. **Project Name Extraction & Search**:
   Given local workspace path `~/projects/sample-project`:
   - Extracted project name: `sample-project`.
   - On the target host under its base prefix, search for matching directories named `sample-project`.
   - **Zero matches**: Create target directory or fallback to `<remote_prefix>/sample-project`.
   - **Exactly one match**: Successfully map to that path (e.g. `/home/user/workspace/sample-project`).
   - **Multiple matches (Ambiguity)**: If multiple directories share the name `sample-project` under the prefix (e.g. `/home/user/workspace/sample-project` and `/home/user/workspace/old/sample-project`), **immediately abort** with an error:
     `ambiguous workspace match: multiple directories named 'sample-project' found under '/home/user/workspace'. Please specify --target-path explicitly.`
4. **Metadata Path Rewriting on Import**:
   - For Codex: Rewrite `cwd` in `threads` table and first line of `rollout-*.jsonl`. Rewrite `rollout_path` in `threads` to the target machine's absolute rollout file path.
   - For AGY: Rewrite `workspace_uris` in `conversation_summaries.db` and workspace strings in `conversations/<id>.db` (`trajectory_meta`).

### 3.3. Mutual Exclusion & Lock Protection

To prevent database corruption or race conditions with running agents:
1. `async` opens the lock file on the source:
   - Codex: `~/.codex/thread-writer-locks/<id>.lock`
   - AGY: `~/.gemini/antigravity-cli/presence/<id>.lock`
2. It attempts a non-blocking `flock(fd, LOCK_EX | LOCK_NB)`.
   - If `flock` returns `EWOULDBLOCK` / `EAGAIN`, the agent is actively writing to that session. **Abort immediately** with `error: session <id> is currently locked by a running agent process`.
3. The remote endpoint performs the exact same check before reading or writing.
4. Only when both sides verify the session is quiescent does sync proceed.

### 3.4. Surgical SQLite Extraction & Merging

Instead of transferring huge database files:
1. **Codex**:
   - Query `state_5.sqlite` for the main thread and all descendant subagents via `thread_spawn_edges`.
   - Extract matching rows from `thread_history_1.sqlite` (`thread_turns`, `thread_items`, `thread_history_projection_state`, `thread_realtime_items`).
   - Package extracted rows into a lightweight SQLite transfer payload or JSON stream.
   - On target, insert rows inside a transaction using `INSERT OR REPLACE INTO ...`.
2. **Antigravity**:
   - `conversations/<id>.db` is already scoped to a single conversation (~200 KB - 10 MB). Checkpoint WAL using `PRAGMA wal_checkpoint(TRUNCATE)` before transfer.
   - Transfer `conversations/<id>.db` and the `brain/<id>/` folder directly.
   - Surgically upsert the single row into the target `conversation_summaries.db`.

---

## 4. Implementation Plan & Milestones

### Milestone 1: Documentation, Repository Foundations & AGENTS.md
- [x] Create comprehensive `AGENTS.md` adhering to user standards.
- [x] Update `PLAN.md` with complete architecture and technical specifications in English.
- [x] Create `README.md` and `docs/` documentation.
- [x] Initialize Go module (`go.mod`) with pure-Go SQLite dependency.

### Milestone 2: Core Packages & Engine Modules
- [ ] `internal/config`: Configuration file parser (`~/.config/async/config.toml`) for host prefixes and defaults.
- [ ] `internal/lock`: Non-blocking kernel `flock` verification for Codex and AGY lock files.
- [ ] `internal/pathmap`: Workspace prefix mapper, directory searcher, ambiguity detector, and path rewriting.
- [ ] `internal/archive`: Streaming tarball (`tar.gz`) packager and unpacker with atomic staging.

### Milestone 3: Provider Implementations
- [ ] `internal/providers/codex`:
  - Find latest thread by `cwd` (filtering `thread_source = 'user'`).
  - Resolve child subagents via `thread_spawn_edges`.
  - Surgically dump and import rows from `state_5.sqlite` and `thread_history_1.sqlite`.
  - Rewrite `rollout_path` and `cwd` in database and rollout JSONL header.
- [ ] `internal/providers/agy`:
  - Find latest conversation by workspace URI in `conversation_summaries.db`.
  - WAL checkpoint and package `conversations/<id>.db` + `brain/<id>/`.
  - Surgically upsert summary into target `conversation_summaries.db`.

### Milestone 4: SSH RPC Transport & CLI
- [ ] `internal/transport/ssh`: SSH command runner using native `ssh` binary and `~/.ssh/config`.
- [ ] `internal/rpc`: RPC protocol for query, lock checking, and data streaming between local and remote `async`.
- [ ] `cmd/async`: CLI entrypoint with subcommands:
  - `push`, `pull`, `ls`, `status`, `deploy`, `_rpc`.

### Milestone 5: Deployment & End-to-End Validation
- [ ] Build static binary with `CGO_ENABLED=0`.
- [ ] Run `async deploy sandbox` to install `~/.local/bin/async` inside remote sandbox.
- [ ] End-to-end test `async codex push sandbox`, `codex resume` in sandbox, `async codex pull sandbox`.
- [ ] End-to-end test `async agy push sandbox` and `async agy pull sandbox`.
