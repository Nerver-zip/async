# async (Agent Sync)

[![Go Version](https://img.shields.io/badge/go-1.24%2B-blue.svg)](https://golang.org)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A self-contained CLI utility and single static binary for synchronizing AI agent conversation sessions between workstations and remote environments (such as sandboxes, VPS, and containers) over standard SSH.

Supported agent providers:
- **OpenAI Codex CLI (`codex`)** (`~/.codex`)
- **Antigravity CLI (`agy`)** (`~/.gemini/antigravity-cli`)

---

## Why `async`?

When working with terminal AI coding agents across multiple machines (e.g. developing locally on a workstation and dispatching execution to an isolated container or remote server):
1. **Context Fragmentation**: Resuming work on another machine previously required starting over from scratch or manually hunting down session hashes.
2. **Gigantic Databases**: Codex sessions are stored in monolithic SQLite databases (`thread_history_1.sqlite`) reaching gigabytes in size. Blindly copying whole databases over the network is slow, destructive, and wipes out existing conversations on the destination.
3. **Complex Internal References**: Sessions link together absolute workspace paths, subagent trees, rollout event streams, brain artifacts, and lock files.
4. **Firewall Asymmetry**: Hardened sandboxes typically block outbound connections to local networks or tailnets, allowing only inbound SSH connections.

`async` solves this by:
- **Auto-detecting Workspaces**: Run `async codex push <host>` inside any project folder, and it automatically finds and syncs the latest session for that workspace.
- **Surgical SQLite Extraction**: Extracts only the rows belonging to the conversation and its subagents, merging them seamlessly into the destination database without touching other sessions.
- **Path Prefix Mapping**: Automatically translates workspace base paths (e.g. `~/projects/foo` $\to$ `~/workspace/foo`) and rewrites serialized paths inside transcripts and databases.
- **Active Lock Safety**: Verifies OS-level file locks (`flock`) on both ends to prevent syncing while an agent is actively running.
- **Zero-Dependency Single Binary**: Pure Go static binary (`CGO_ENABLED=0`) that runs without `sqlite3`, `rsync`, or shared library dependencies on both source and target.

---

## Installation

### From Source

```bash
git clone https://github.com/Nerver-zip/async.git
cd async
go build -o async ./cmd/async
sudo install -m 755 async /usr/local/bin/async
```

### Deploy to Remote Host

To install or update `async` on a remote host configured in your `~/.ssh/config` (e.g. `remote-host` or `sandbox`):

```bash
async deploy <targethost>
# e.g., async deploy sandbox
```

This copies the static binary to `~/.local/bin/async` on the remote machine via SSH.

---

## Quick Start

### 1. Push Latest Session to Remote

Inside your project repository (e.g. `~/projects/my-project`):

```bash
# Push latest OpenAI Codex conversation to remote host
async codex push <targethost>

# Push latest Antigravity CLI conversation to remote host
async agy push <targethost>
```

### 2. Pull Latest Session from Remote

Back on your local machine, pull remote updates:

```bash
# Pull the latest conversation corresponding to this workspace from remote
async codex pull <targethost>
async agy pull <targethost>
```

### 3. Explicit Conversation ID

You can specify a conversation ID explicitly using either `--id <uuid>` or direct `--<uuid>` syntax:

```bash
async codex push --id 01a127cf-7325-7a93-a4fc-63d244f874b5 sandbox
async codex push --01a127cf-7325-7a93-a4fc-63d244f874b5 sandbox
async agy pull --id e1e555ab-8771-4848-bb6c-5404c45a9cd6 sandbox
```

### 4. Inspect Status and List Sessions

```bash
# Compare session status between local and remote
async status <targethost>

# List conversations for the current workspace
async codex ls [targethost]
async agy ls [targethost]

# List all stored conversations across all workspaces
async codex ls --all
```

---

## Configuration

Configuration is optional. By default, `async` uses standard paths and infers workspace directories. You can customize base prefixes and host settings in `~/.config/async/config.toml`:

```toml
[defaults]
local_prefix = "~/projects"
remote_prefix = "~/workspace"

[hosts.sandbox]
prefix = "/home/user/workspace"

[hosts.remote-server]
prefix = "/home/user/dev"
```

For more details on path resolution, see [Path Mapping](docs/PATH_MAPPING.md).

---

## Documentation

- [Architecture & Design](docs/ARCHITECTURE.md)
- [CLI Reference](docs/CLI.md)
- [Workspace Path Mapping](docs/PATH_MAPPING.md)
- [Agent Guide & Contribution Rules](AGENTS.md)
- [Implementation Plan](docs/PLAN.md)

---

## License

MIT License. See [LICENSE](LICENSE) for details.
