# CLI Reference

The general syntax for `async` is:

```bash
async [provider] <command> [arguments...] [flags...]
```

Where `[provider]` is either `codex` or `agy`.

---

## Commands

### `push`

Synchronize a conversation from the local workstation to a remote host.

```bash
async <codex|agy> push [flags] <targethost>
```

**Arguments:**
- `<targethost>`: Host name as configured in `~/.ssh/config` (e.g. `sandbox`, `remote-server`) or `user@hostname`.

**Flags:**
- `--id <uuid>`, `-i <uuid>`: Synchronize a specific conversation ID.
- `--<uuid>`: Shortcut syntax for specifying a conversation ID directly as a flag.
- `--target-path <path>`: Explicit destination workspace path override.
- `--force`, `-f`: Overwrite remote conversation even if remote has newer edits.
- `--dry-run`: Inspect actions without writing changes.

**Behavior:**
- If `--id` is omitted, `async` identifies the current working directory, queries the provider's local state index, and selects the most recently updated conversation belonging to this workspace.
- Checks local and remote locks before initiating data transfer.
- Transmits surgical SQLite payloads and session assets over SSH.
- Updates metadata on the destination host so `codex resume` or `agy -c` finds the session.

---

### `pull`

Synchronize a conversation from a remote host to the local workstation.

```bash
async <codex|agy> pull [flags] <targethost>
```

**Arguments:**
- `<targethost>`: Remote host name.

**Flags:**
- `--id <uuid>`, `-i <uuid>`: Pull a specific conversation ID.
- `--<uuid>`: Shortcut syntax for specifying a conversation ID directly.
- `--target-path <path>`: Remote workspace path to inspect for latest conversation.
- `--force`, `-f`: Overwrite local conversation even if local has newer edits.
- `--dry-run`: Inspect actions without writing changes.

**Behavior:**
- If `--id` is omitted, `async` requests the remote helper to resolve the matching workspace path under its base prefix, locate the latest session, and stream it to the local workstation.
- Checks remote and local locks.
- Surgically merges the session into local database and places rollout/brain files.

---

### `ls`

List stored conversations for a given workspace or across all workspaces.

```bash
# List local conversations for current directory
async <codex|agy> ls

# List remote conversations on 'agents' for corresponding directory
async <codex|agy> ls agents

# List all stored conversations across all workspaces
async <codex|agy> ls --all
async <codex|agy> ls agents --all
```

**Flags:**
- `--all`, `-a`: Show conversations across all workspaces, not just current directory.
- `--json`: Output conversation list in JSON format.

---

### `status`

Inspect conversation synchronization status and drift between local and remote environments.

```bash
async status <targethost>
```

**Behavior:**
- Compares the latest conversation for current workspace between local and remote.
- Displays:
  - Local session ID, last modified timestamp, and step count.
  - Remote session ID, last modified timestamp, and step count.
  - Active lock state (whether an agent is currently running).
  - Sync recommendation (e.g. `Remote is ahead by 4 steps (Pull recommended)` or `In sync`).

---

### `deploy`

Install or update the `async` static binary on a remote host over SSH.

```bash
async deploy <targethost>
```

**Behavior:**
- Compiles or reads local `async` binary.
- Connects to `<targethost>` via SSH.
- Ensures `~/.local/bin` exists on remote and copies the binary with executable permissions (`0755`).
- Verifies execution by running `async --version` remotely.

---

### Internal RPC Subcommand (`_rpc`)

Used internally by `async` to execute remote actions over SSH `stdio`. Not meant to be invoked manually by users.

```bash
async _rpc find --provider <codex|agy> --path <path>
async _rpc lock-check --provider <codex|agy> --id <uuid>
async _rpc dump --provider <codex|agy> --id <uuid>
async _rpc import --provider <codex|agy>
```
