# Workspace Path Mapping & Ambiguity Resolution

When synchronizing sessions between different machines, directory trees typically do not share identical absolute roots.

For example:
- **Local Machine (Workstation)**: Projects reside under `~/projects/` (e.g. `/home/user/projects/my-project`).
- **Remote Machine (`sandbox`)**: Projects reside under `~/workspace/` (e.g. `/home/user/workspace/my-project`) or `~/dev/`.

`async` incorporates an automatic path translation engine that maps project directories and rewrites session metadata so resumed conversations seamlessly match the destination environment.

---

## 1. Base Prefix Configuration

`async` checks configuration from `~/.config/async/config.toml` (and environment variables):

```toml
[defaults]
local_prefix = "~/projects"
remote_prefix = "~/workspace"

[hosts.sandbox]
prefix = "/home/user/workspace"

[hosts.remote-server]
prefix = "/home/user/dev"
```

If unconfigured, defaults are:
- Local default: parent directory of current working directory or `~/projects` / `~/dev`.
- Remote default: `~/workspace`.

---

## 2. Path Translation Workflow

### Push Workflow (`workstation -> remote`)

1. User runs `async codex push sandbox` from `/home/user/projects/my-project`.
2. `async` extracts the project directory name: `my-project`.
3. If `--target-path` is provided, `async` uses it directly.
4. Otherwise, `async` contacts `sandbox` via SSH RPC and queries its base prefix (`/home/user/workspace`):
   - Searches for directories matching `my-project` directly or within immediate subfolders under the prefix.
   - **Case 1: Unique match found** (e.g. `/home/user/workspace/my-project`):
     Mapped destination path is `/home/user/workspace/my-project`.
   - **Case 2: No directory found**:
     Default destination path is `/home/user/workspace/my-project`. If requested or missing, destination directory can be created.
   - **Case 3: Multiple directories found (Ambiguity Collision)**:
     If the target contains e.g. `/home/user/workspace/my-project` AND `/home/user/workspace/archive/my-project`:
     `async` **immediately cancels the operation** with an error:
     ```
     error: ambiguous target directory: multiple matches for 'my-project' found under '/home/user/workspace':
       - /home/user/workspace/my-project
       - /home/user/workspace/archive/my-project
     Please specify the intended path explicitly with --target-path <path>.
     ```

### Pull Workflow (`remote -> workstation`)

1. User runs `async codex pull sandbox` from `/home/user/projects/my-project`.
2. Local project name is `my-project`.
3. If `--target-path` is supplied, remote queries that exact path.
4. Otherwise, remote searches its base prefix for directories named `my-project`:
   - **Unique match**: Locates latest conversation for that remote path and streams it.
   - **Ambiguity**: If multiple remote directories share that name, cancels with an error listing the candidates and asking for `--target-path`.
   - **Not found**: Warns that no workspace matches `my-project` remotely, prompting for an explicit `--target-path` or `--id`.

---

## 3. Metadata Path Rewriting on Destination

When synchronizing a conversation, internal references to absolute paths must be translated so that tools and agents function properly when resumed on the destination host:

### OpenAI Codex CLI Rewrites:
- **`state_5.sqlite` (`threads` table)**:
  - `cwd`: Rewritten from `/home/user/projects/my-project` to `/home/user/workspace/my-project`.
  - `rollout_path`: Rewritten from `/home/user/.codex/sessions/...` to destination user's `.codex/sessions/...`.
- **`rollout-*.jsonl`**:
  - The initial JSON line contains:
    ```json
    {"cwd": "/home/user/projects/my-project", "runtime_workspace_roots": ["/home/user/projects/my-project"]}
    ```
    Rewritten to target workspace path.

### Antigravity CLI Rewrites:
- **`conversation_summaries.db` (`conversation_summaries` table)**:
  - `workspace_uris`: Rewritten from `["file:///home/user/projects/my-project"]` to `["file:///home/user/workspace/my-project"]`.
- **`conversations/<id>.db`**:
  - `trajectory_meta`: Rewritten to point to target workspace path.
