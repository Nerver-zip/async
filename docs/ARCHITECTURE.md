# System Architecture

`async` (Agent Sync) provides bi-directional session synchronization between AI agent CLI environments across network boundaries.

```
+------------------------------------+          SSH Channel (stdio RPC)         +-------------------------------------+
|        Local Workstation           | ---------------------------------------> |          Remote Host / Sandbox       |
|                                    |                                          |                                     |
|  - CLI Invocation (push/pull/ls)   |                                          |  - async _rpc helper                |
|  - Workspace Auto-Detection        |                                          |  - Remote Workspace Resolution      |
|  - Local Lock Check (flock)        |                                          |  - Remote Lock Check (flock)        |
|  - Surgical SQLite Dump / Slicing  | <======================================> |  - Surgical SQLite Import / Merge   |
|  - Path Translation Engine         |          JSON RPC + Streamed tar.gz      |  - Staged Artifact Unpack           |
+------------------------------------+                                          +-------------------------------------+
```

---

## 1. Network Topology & RPC Over SSH

### The Firewall Constraint
Hardened sandbox environments often enforce egress isolation (blocking outbound connections toward LAN, host, or other networks). Only inbound connections (e.g. `workstation -> ssh <targethost>`) are permitted.

To avoid requiring reverse tunnels, listening ports, or background daemons inside the container:
- All operations are orchestrated from the client machine over standard SSH.
- The client launches `ssh <targethost> ~/.local/bin/async _rpc <subcommand>`.
- The remote `async` process reads commands and queries from `stdin` and writes JSON responses to `stdout`.
- Large payloads (surgical SQLite databases, rollout files, brain directories) are packaged as compressed `tar.gz` streams and multiplexed over the SSH transport.

---

## 2. Provider Internals

### 2.1. OpenAI Codex CLI

Codex persists state in `~/.codex/`:

1. **`state_5.sqlite`**:
   - `threads`: Index of user sessions and subagent threads. Holds `id`, `rollout_path`, `cwd`, `title`, `updated_at`, `history_mode`, `thread_source`.
   - `thread_spawn_edges`: Tracks parent $\to$ child links between sessions and their subagents (`parent_thread_id`, `child_thread_id`).
   - `thread_dynamic_tools`: Tool states associated with threads.
   - `thread_attachments`: Attachments linked to thread turns.
2. **`thread_history_1.sqlite`**:
   - Massive repository of thread turns, turns projection, items, and realtime streaming checkpoints.
   - Slicing: `async` extracts rows where `thread_id IN (parent_thread_id, child_thread_ids)` into a temporary SQLite transfer database.
   - Target Merging: Target `async` executes `INSERT OR REPLACE INTO ...` within a transaction.
3. **Rollout Files**:
   - `sessions/YYYY/MM/DD/rollout-<timestamp>-<thread_id>.jsonl`.
   - The first line contains JSON metadata with `id`, `cwd`, and `runtime_workspace_roots`. During import, `cwd` and workspace roots are rewritten to the target workspace path.
4. **Lock Mechanism**:
   - `~/.codex/thread-writer-locks/<thread_id>.lock`.
   - An advisory OS lock (`flock`) is held in exclusive mode by Codex while active. `async` verifies lock availability with `flock(fd, LOCK_EX | LOCK_NB)` before proceeding.

### 2.2. Antigravity CLI (AGY)

Antigravity CLI persists state in `~/.gemini/antigravity-cli/`:

1. **`conversations/<id>.db`**:
   - Individual SQLite database for each conversation. Contains `trajectory_meta`, `steps`, `gen_metadata`, `executor_metadata`, `parent_references`, and `trajectory_metadata_blob`.
   - WAL Checkpointing: Before copying, `async` executes `PRAGMA wal_checkpoint(TRUNCATE)` to ensure all data is consolidated in the main `.db` file.
2. **`brain/<id>/`**:
   - Directory containing generated markdown plans, images, task files, and `.system_generated/logs/transcript.jsonl`.
   - Packaged and synchronized in its entirety.
3. **`conversation_summaries.db`**:
   - Central index containing `conversation_id`, `title`, `preview`, `step_count`, `last_modified_time`, `workspace_uris` (JSON array: `["file:///..."]`), and `parent_conversation_id`.
   - Surgical update: `async` reads the summary record from the source, updates `workspace_uris` to point to the target path, and upserts it into the destination index.
4. **Lock Mechanism**:
   - `~/.gemini/antigravity-cli/presence/<id>.lock`.
   - OS lock held via `flock` while an AGY session is open. Verified non-blocking before sync.

---

## 3. Concurrency & Collision Prevention

- **Kernel Lock Check**: Always non-blocking. If either source or destination holds an active lock, `async` aborts immediately with an actionable error.
- **Monotonicity Check**: Before replacing a session on the destination, `async` compares `updated_at` / `last_modified_time` and step count. If the destination session is newer or has progressed further than the incoming payload, sync fails unless `--force` is supplied.
- **Atomic Staging**: Transferred files and database chunks are written to a temporary `.async-staging-<uuid>` directory. Once verification succeeds, files are moved into their final locations atomically, ensuring incomplete transfers never corrupt provider state.
