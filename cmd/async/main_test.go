package main

import (
	"bytes"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestParseFlexibleFlags_DeleteAndCleanFlags(t *testing.T) {
	pf, err := parseFlexibleFlags([]string{"--older-than", "7d", "-w", "/tmp/ws", "-y", "--provider", "agy", "--host", "agents"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pf.olderThan != 7*24*time.Hour {
		t.Errorf("olderThan = %v, want 7d", pf.olderThan)
	}
	if pf.workspace != "/tmp/ws" {
		t.Errorf("workspace = %q, want /tmp/ws", pf.workspace)
	}
	if !pf.yes {
		t.Errorf("yes = false, want true")
	}
	if pf.provider != "agy" {
		t.Errorf("provider = %q, want agy", pf.provider)
	}
	if pf.host != "agents" {
		t.Errorf("host = %q, want agents", pf.host)
	}

	pf2, err := parseFlexibleFlags([]string{"--id", "id1", "--id", "id2", "-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(pf2.ids) != 2 || pf2.ids[0] != "id1" || pf2.ids[1] != "id2" {
		t.Errorf("ids = %v, want [id1, id2]", pf2.ids)
	}
	if !pf2.all {
		t.Errorf("all = false, want true")
	}
}

func TestCLIRunDelete_SingleAndBulkConfirmation(t *testing.T) {
	tmpRoot := t.TempDir()
	codexHome := filepath.Join(tmpRoot, ".codex")
	ws := filepath.Join(tmpRoot, "ws")

	cfg := &config.Config{
		CodexHome: codexHome,
		AGYHome:   filepath.Join(tmpRoot, ".agy"),
	}

	s1 := "01a11111-1111-1111-1111-111111111111"
	s2 := "01a22222-2222-2222-2222-222222222222"
	seedMinimalCodexSession(t, codexHome, s1, ws, 1792000000, 2)
	seedMinimalCodexSession(t, codexHome, s2, ws, 1792000000, 2)

	// 1. Single delete runs immediately without confirmation prompt
	var out, errOut bytes.Buffer
	if err := runDelete(cfg, "codex", []string{s1}, nil, &out, &errOut, nil); err != nil {
		t.Fatalf("runDelete single failed: %v", err)
	}
	if !strings.Contains(out.String(), "Deleted codex session "+s1) {
		t.Errorf("unexpected output: %s", out.String())
	}

	// Re-seed s1
	seedMinimalCodexSession(t, codexHome, s1, ws, 1792000000, 2)

	// 2. Multi delete with "n\n" input cancels
	out.Reset()
	if err := runDelete(cfg, "codex", []string{s1, s2}, strings.NewReader("n\n"), &out, &errOut, nil); err != nil {
		t.Fatalf("runDelete cancelled failed: %v", err)
	}
	if !strings.Contains(out.String(), "Deletion cancelled") {
		t.Errorf("expected cancellation, got: %s", out.String())
	}

	// 3. Multi delete with "y\n" succeeds
	out.Reset()
	if err := runDelete(cfg, "codex", []string{s1, s2}, strings.NewReader("y\n"), &out, &errOut, nil); err != nil {
		t.Fatalf("runDelete confirmed failed: %v", err)
	}
	if !strings.Contains(out.String(), "Deleted codex session "+s1) || !strings.Contains(out.String(), "Deleted codex session "+s2) {
		t.Errorf("expected both deleted, got: %s", out.String())
	}
}

func TestCLIRunClean_FiltersAndBulkConfirmation(t *testing.T) {
	tmpRoot := t.TempDir()
	codexHome := filepath.Join(tmpRoot, ".codex")
	ws1 := filepath.Join(tmpRoot, "ws1")
	ws2 := filepath.Join(tmpRoot, "ws2")

	cfg := &config.Config{
		CodexHome: codexHome,
		AGYHome:   filepath.Join(tmpRoot, ".agy"),
	}

	now := time.Now().Unix()
	sOld1 := "01a10000-0000-0000-0000-000000000001"
	sNew1 := "01a10000-0000-0000-0000-000000000002"
	sOld2 := "01a10000-0000-0000-0000-000000000003"

	// sOld1: 10 days ago, ws1
	seedMinimalCodexSession(t, codexHome, sOld1, ws1, now-10*86400, 2)
	// sNew1: 1 hour ago, ws1
	seedMinimalCodexSession(t, codexHome, sNew1, ws1, now-3600, 2)
	// sOld2: 10 days ago, ws2
	seedMinimalCodexSession(t, codexHome, sOld2, ws2, now-10*86400, 2)

	// Dry run with --older-than 5d --all
	var out, errOut bytes.Buffer
	if err := runClean(cfg, "codex", []string{"--older-than", "5d", "--all", "--dry-run"}, nil, &out, &errOut, nil); err != nil {
		t.Fatalf("runClean dry-run failed: %v", err)
	}
	if !strings.Contains(out.String(), "2 session(s) would be deleted") {
		t.Errorf("expected 2 candidates in dry run, got:\n%s", out.String())
	}

	// Clean ws1 specifically with confirmation "y\n"
	out.Reset()
	if err := runClean(cfg, "codex", []string{"--workspace", ws1}, strings.NewReader("y\n"), &out, &errOut, nil); err != nil {
		t.Fatalf("runClean workspace ws1 failed: %v", err)
	}
	if !strings.Contains(out.String(), "Successfully cleaned 2 codex session(s)") {
		t.Errorf("expected 2 sessions cleaned in ws1, got:\n%s", out.String())
	}

	// Verify sOld2 in ws2 is untouched
	out.Reset()
	if err := runList(cfg, "codex", []string{"--all"}, &out, nil); err != nil {
		t.Fatalf("runList failed: %v", err)
	}
	if !strings.Contains(out.String(), sOld2) {
		t.Errorf("expected sOld2 to still exist in ws2, got:\n%s", out.String())
	}
}

func TestCLIRemoteDeleteAndClean(t *testing.T) {
	localRoot := t.TempDir()
	remoteRoot := t.TempDir()

	localCfg := &config.Config{
		CodexHome: filepath.Join(localRoot, ".codex"),
		AGYHome:   filepath.Join(localRoot, ".agy"),
		Hosts: map[string]config.HostConfig{
			"agents": {Prefix: filepath.Join(remoteRoot, "workspace")},
		},
	}
	remoteCfg := &config.Config{
		CodexHome: filepath.Join(remoteRoot, ".codex"),
		AGYHome:   filepath.Join(remoteRoot, ".agy"),
	}

	remoteWS := filepath.Join(remoteRoot, "workspace", "proj")
	s1 := "01a33333-3333-3333-3333-333333333333"
	s2 := "01a44444-4444-4444-4444-444444444444"
	seedMinimalCodexSession(t, remoteCfg.CodexHome, s1, remoteWS, 1792000000, 2)
	seedMinimalCodexSession(t, remoteCfg.CodexHome, s2, remoteWS, 1792000000, 2)

	runner := &inProcessRunner{remoteCfg: remoteCfg}

	// Remote single delete
	var out, errOut bytes.Buffer
	if err := runDelete(localCfg, "codex", []string{"agents", s1}, nil, &out, &errOut, runner); err != nil {
		t.Fatalf("remote runDelete failed: %v", err)
	}
	if !strings.Contains(out.String(), "Deleted codex session "+s1+" on agents") {
		t.Errorf("unexpected remote delete output: %s", out.String())
	}

	// Remote clean s2 with --yes
	out.Reset()
	if err := runClean(localCfg, "codex", []string{"agents", "--all", "--yes"}, nil, &out, &errOut, runner); err != nil {
		t.Fatalf("remote runClean failed: %v", err)
	}
	if !strings.Contains(out.String(), "Successfully cleaned 1 codex session(s) on agents") {
		t.Errorf("unexpected remote clean output: %s", out.String())
	}
}
