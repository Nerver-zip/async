package main

import (
	"bytes"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nerver-zip/async/internal/config"
	"github.com/Nerver-zip/async/internal/rpc"
	_ "modernc.org/sqlite"
)

type inProcessRunner struct {
	remoteCfg *config.Config
}

func (r *inProcessRunner) RunRPC(args []string, stdin io.Reader, stdout io.Writer) error {
	return rpc.HandleServer(r.remoteCfg, args, stdin, stdout)
}

func (r *inProcessRunner) Deploy(localBinPath string) (string, error) {
	return "async " + Version, nil
}

func (r *inProcessRunner) TargetHost() string {
	return "agents"
}

func TestParseFlexibleFlags_UUIDShorthandAndOrder(t *testing.T) {
	pf, err := parseFlexibleFlags([]string{"--01a127cf-7325-7a93-a4fc-63d244f874b5", "agents", "--force"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pf.id != "01a127cf-7325-7a93-a4fc-63d244f874b5" {
		t.Errorf("id = %q, want 01a127cf-7325-7a93-a4fc-63d244f874b5", pf.id)
	}
	if !pf.force {
		t.Errorf("force = false, want true")
	}
	if len(pf.positionals) != 1 || pf.positionals[0] != "agents" {
		t.Errorf("positionals = %v, want [agents]", pf.positionals)
	}
}

func TestCLIEndToEndPushStatusPullAndAmbiguity(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()

	localPrefix := filepath.Join(localRoot, "Desktop", "dev")
	remotePrefix := filepath.Join(remoteRoot, "workspace")

	localProj := filepath.Join(localPrefix, "my-app")
	remoteProj := filepath.Join(remotePrefix, "my-app")
	if err := os.MkdirAll(localProj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(remoteProj, 0o755); err != nil {
		t.Fatal(err)
	}

	localCfg := &config.Config{
		LocalPrefix:  localPrefix,
		RemotePrefix: remotePrefix,
		CodexHome:    filepath.Join(localRoot, ".codex"),
		AGYHome:      filepath.Join(localRoot, ".agy"),
		Hosts: map[string]config.HostConfig{
			"sandbox": {Prefix: remotePrefix},
		},
	}
	remoteCfg := &config.Config{
		LocalPrefix:  remotePrefix,
		RemotePrefix: remotePrefix,
		CodexHome:    filepath.Join(remoteRoot, ".codex"),
		AGYHome:      filepath.Join(remoteRoot, ".agy"),
	}

	// Seed a local Codex session in localCfg.CodexHome for localProj
	sessionID := "01a127cf-7325-7a93-a4fc-63d244f874b5"
	seedMinimalCodexSession(t, localCfg.CodexHome, sessionID, localProj, 1792000000, 3)

	runner := &inProcessRunner{remoteCfg: remoteCfg}

	// 1. Push from localProj to sandbox
	var out bytes.Buffer
	if err := runPush(localCfg, "codex", []string{"--cwd", localProj, "sandbox"}, &out, runner); err != nil {
		t.Fatalf("runPush failed: %v", err)
	}
	if !strings.Contains(out.String(), "Pushed codex session "+sessionID) {
		t.Errorf("unexpected push output: %s", out.String())
	}

	// 2. Check status (should report "In sync")
	out.Reset()
	if err := runStatus(localCfg, []string{"--cwd", localProj, "sandbox"}, &out, runner); err != nil {
		t.Fatalf("runStatus failed: %v", err)
	}
	if !strings.Contains(out.String(), "In sync") {
		t.Errorf("expected 'In sync' in status output, got:\n%s", out.String())
	}

	// 3. Advance session on remote (simulate continuing work inside remote sandbox)
	seedMinimalCodexSession(t, remoteCfg.CodexHome, sessionID, remoteProj, 1792050000, 7)

	// 4. Status should now recommend Pull
	out.Reset()
	if err := runStatus(localCfg, []string{"--cwd", localProj, "sandbox"}, &out, runner); err != nil {
		t.Fatalf("runStatus failed: %v", err)
	}
	if !strings.Contains(out.String(), "Remote is ahead") {
		t.Errorf("expected 'Remote is ahead' in status output, got:\n%s", out.String())
	}

	// 5. Pull updated session back to local
	out.Reset()
	if err := runPull(localCfg, "codex", []string{"--cwd", localProj, "sandbox"}, &out, runner); err != nil {
		t.Fatalf("runPull failed: %v", err)
	}
	if !strings.Contains(out.String(), "Steps:      7") {
		t.Errorf("expected 7 steps after pull, got:\n%s", out.String())
	}

	// 6. Test ambiguity collision: create a second 'my-app' directory under remotePrefix
	dupRemoteProj := filepath.Join(remotePrefix, "archived", "my-app")
	if err := os.MkdirAll(dupRemoteProj, 0o755); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	err := runPush(localCfg, "codex", []string{"--cwd", localProj, "--force", "sandbox"}, &out, runner)
	if err == nil || !strings.Contains(err.Error(), "ambiguous target directory") {
		t.Fatalf("expected ambiguity error on duplicate remote project dir, got: %v", err)
	}

	// Explicitly passing --target-path resolves the ambiguity
	out.Reset()
	if err := runPush(localCfg, "codex", []string{"--cwd", localProj, "--target-path", remoteProj, "--force", "sandbox"}, &out, runner); err != nil {
		t.Fatalf("expected --target-path override to succeed despite duplicate dirs, got: %v", err)
	}
}

func seedMinimalCodexSession(t *testing.T, codexHome, id, ws string, updatedSec int64, items int) {
	t.Helper()
	rollout := filepath.Join(codexHome, "sessions", "2026", "10", "15", "rollout-"+id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
		t.Fatal(err)
	}
	hdr := `{"timestamp":"2026-10-15T10:00:00Z","ordinal":0,"type":"session_meta","payload":{"id":"` + id + `","cwd":"` + ws + `","runtime_workspace_roots":["` + ws + `"]}}` + "\n"
	if err := os.WriteFile(rollout, []byte(hdr), 0o600); err != nil {
		t.Fatal(err)
	}

	stateDB, err := sql.Open("sqlite", filepath.Join(codexHome, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateDB.Close()

	_, err = stateDB.Exec(`
CREATE TABLE IF NOT EXISTS threads (
    id TEXT PRIMARY KEY, rollout_path TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
    source TEXT NOT NULL, model_provider TEXT NOT NULL, cwd TEXT NOT NULL, title TEXT NOT NULL,
    sandbox_policy TEXT NOT NULL, approval_mode TEXT NOT NULL, archived INTEGER NOT NULL DEFAULT 0,
    created_at_ms INTEGER, updated_at_ms INTEGER, thread_source TEXT, preview TEXT NOT NULL DEFAULT '',
    history_mode TEXT NOT NULL DEFAULT 'paginated', project_id TEXT, thread_section_id TEXT
);`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = stateDB.Exec(`
INSERT OR REPLACE INTO threads (id, rollout_path, created_at, updated_at, created_at_ms, updated_at_ms, source, thread_source, model_provider, cwd, title, sandbox_policy, approval_mode, archived)
VALUES (?, ?, ?, ?, ?, ?, 'cli', 'user', 'openai', ?, 'Test Session', 'workspace-write', 'on-request', 0)`,
		id, rollout, updatedSec-10, updatedSec, (updatedSec-10)*1000, updatedSec*1000, ws)
	if err != nil {
		t.Fatal(err)
	}

	histDB, err := sql.Open("sqlite", filepath.Join(codexHome, "thread_history_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer histDB.Close()

	_, err = histDB.Exec(`
CREATE TABLE IF NOT EXISTS thread_turns (thread_id TEXT NOT NULL, turn_id TEXT NOT NULL, rollout_ordinal INTEGER NOT NULL, status TEXT NOT NULL, PRIMARY KEY (thread_id, turn_id));
CREATE TABLE IF NOT EXISTS thread_items (thread_id TEXT NOT NULL, turn_id TEXT NOT NULL, item_id TEXT NOT NULL, rollout_ordinal INTEGER NOT NULL, created_at_ms INTEGER NOT NULL, item_json TEXT NOT NULL, item_type TEXT NOT NULL DEFAULT '', PRIMARY KEY (thread_id, turn_id, item_id));
`)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = histDB.Exec(`INSERT OR REPLACE INTO thread_turns (thread_id, turn_id, rollout_ordinal, status) VALUES (?, 't1', 1, 'completed')`, id)
	for i := 1; i <= items; i++ {
		_, err = histDB.Exec(`INSERT OR REPLACE INTO thread_items (thread_id, turn_id, item_id, rollout_ordinal, created_at_ms, item_json, item_type) VALUES (?, 't1', ?, ?, ?, '{}', 'userMessage')`,
			id, "item-"+string(rune('0'+i)), i, updatedSec*1000)
		if err != nil {
			t.Fatal(err)
		}
	}
}
