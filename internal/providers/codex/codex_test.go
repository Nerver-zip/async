package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nerver-zip/async/internal/lock"
	"github.com/Nerver-zip/async/internal/providers"
)

func seedCodexFixture(t *testing.T, codexHome, parentID, childID, workspace string, updatedSec int64, itemCount int) {
	t.Helper()
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create rollout files
	parentRollout := filepath.Join(codexHome, "sessions", "2026", "10", "15", "rollout-2026-10-15T10-00-00-"+parentID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(parentRollout), 0o755); err != nil {
		t.Fatal(err)
	}
	header := `{"timestamp":"2026-10-15T10:00:00Z","ordinal":0,"type":"session_meta","payload":{"id":"` + parentID + `","cwd":"` + workspace + `","runtime_workspace_roots":["` + workspace + `"],"thread_source":"user"}}` + "\n"
	event := `{"timestamp":"2026-10-15T10:01:00Z","ordinal":1,"type":"event_msg","payload":{"text":"hello"}}` + "\n"
	if err := os.WriteFile(parentRollout, []byte(header+event), 0o600); err != nil {
		t.Fatal(err)
	}

	var childRollout string
	if childID != "" {
		childRollout = filepath.Join(codexHome, "sessions", "2026", "10", "15", "rollout-2026-10-15T10-02-00-"+childID+".jsonl")
		cHeader := `{"timestamp":"2026-10-15T10:02:00Z","ordinal":0,"type":"session_meta","payload":{"id":"` + childID + `","cwd":"` + workspace + `","runtime_workspace_roots":["` + workspace + `"],"thread_source":"subagent"}}` + "\n"
		if err := os.WriteFile(childRollout, []byte(cHeader), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Populate state_5.sqlite
	stateDB, err := openSQLite(filepath.Join(codexHome, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateDB.Close()

	if _, err := stateDB.Exec(state5SchemaDDL); err != nil {
		t.Fatal(err)
	}

	_, err = stateDB.Exec(`
INSERT OR REPLACE INTO threads (id, rollout_path, created_at, updated_at, created_at_ms, updated_at_ms, source, thread_source, model_provider, cwd, title, sandbox_policy, approval_mode, archived, history_mode)
VALUES (?, ?, ?, ?, ?, ?, 'cli', 'user', 'openai', ?, ?, 'workspace-write', 'on-request', 0, 'paginated')
`, parentID, parentRollout, updatedSec-100, updatedSec, (updatedSec-100)*1000, updatedSec*1000, workspace, "Parent Session "+parentID)
	if err != nil {
		t.Fatal(err)
	}

	if childID != "" {
		// Notice child subagent has a slightly newer updated_at than parent, testing that ListSessions ignores subagents as root!
		_, err = stateDB.Exec(`
INSERT OR REPLACE INTO threads (id, rollout_path, created_at, updated_at, created_at_ms, updated_at_ms, source, thread_source, model_provider, cwd, title, sandbox_policy, approval_mode, archived, history_mode)
VALUES (?, ?, ?, ?, ?, ?, ?, 'subagent', 'openai', ?, '', 'workspace-write', 'on-request', 0, 'paginated')
`, childID, childRollout, updatedSec-50, updatedSec+50, (updatedSec-50)*1000, (updatedSec+50)*1000, `{"subagent":{"thread_spawn":{"parent_thread_id":"`+parentID+`"}}}`, workspace)
		if err != nil {
			t.Fatal(err)
		}

		_, err = stateDB.Exec(`INSERT OR REPLACE INTO thread_spawn_edges (parent_thread_id, child_thread_id, status) VALUES (?, ?, 'completed')`, parentID, childID)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Populate thread_history_1.sqlite
	histDB, err := openSQLite(filepath.Join(codexHome, "thread_history_1.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer histDB.Close()

	if _, err := histDB.Exec(history1SchemaDDL); err != nil {
		t.Fatal(err)
	}

	_, err = histDB.Exec(`INSERT OR REPLACE INTO thread_turns (thread_id, turn_id, rollout_ordinal, status) VALUES (?, 'turn-1', 1, 'completed')`, parentID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= itemCount; i++ {
		_, err = histDB.Exec(`INSERT OR REPLACE INTO thread_items (thread_id, turn_id, item_id, rollout_ordinal, created_at_ms, item_json, item_type) VALUES (?, 'turn-1', ?, ?, ?, '{"type":"msg"}', 'userMessage')`,
			parentID, "item-"+string(rune('0'+i)), i, updatedSec*1000)
		if err != nil {
			t.Fatal(err)
		}
	}
	if childID != "" {
		_, err = histDB.Exec(`INSERT OR REPLACE INTO thread_turns (thread_id, turn_id, rollout_ordinal, status) VALUES (?, 'sub-turn-1', 1, 'completed')`, childID)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestCodexSurgicalExportImportAndSubagents(t *testing.T) {
	srcHome := t.TempDir()
	dstHome := t.TempDir()

	srcWS := "/home/user/projects/sample-project"
	dstWS := "/home/user/workspace/sample-project"

	parentID := "01a127cf-7325-7a93-a4fc-63d244f874b5"
	childID := "01a14046-75fd-7971-8c72-a6e077e7ee80"
	unrelatedSrcID := "01a10000-0000-0000-0000-000000000001"
	unrelatedDstID := "01a19999-9999-9999-9999-999999999999"

	seedCodexFixture(t, srcHome, parentID, childID, srcWS, 1792096000, 4)
	seedCodexFixture(t, srcHome, unrelatedSrcID, "", "/home/user/projects/other", 1792099000, 2)
	seedCodexFixture(t, dstHome, unrelatedDstID, "", "/home/user/workspace/keep-me", 1792090000, 3)

	// 1. Auto-detect latest user session for srcWS (must pick parentID, not childID!)
	found, err := FindSession(srcHome, "", srcWS)
	if err != nil {
		t.Fatalf("FindSession failed: %v", err)
	}
	if found.ID != parentID {
		t.Fatalf("FindSession picked %q, want parent %q", found.ID, parentID)
	}
	if found.SubagentCount != 1 {
		t.Errorf("SubagentCount = %d, want 1", found.SubagentCount)
	}
	if found.StepCount != 4 {
		t.Errorf("StepCount = %d, want 4", found.StepCount)
	}

	// 2. Export bundle for srcWS
	var bundle bytes.Buffer
	exportedMeta, err := ExportBundle(srcHome, "", srcWS, &bundle)
	if err != nil {
		t.Fatalf("ExportBundle failed: %v", err)
	}
	if exportedMeta.ID != parentID {
		t.Fatalf("exported ID = %q, want %q", exportedMeta.ID, parentID)
	}

	// 3. Import into dstHome with path translation to dstWS
	importedMeta, err := ImportBundle(dstHome, &bundle, dstWS, false)
	if err != nil {
		t.Fatalf("ImportBundle failed: %v", err)
	}
	if importedMeta.Workspace != dstWS {
		t.Errorf("imported Workspace = %q, want %q", importedMeta.Workspace, dstWS)
	}

	// 4. Verify parentID is discoverable in dstHome under dstWS
	dstFound, err := FindSession(dstHome, "", dstWS)
	if err != nil {
		t.Fatalf("FindSession on destination failed: %v", err)
	}
	if dstFound.ID != parentID {
		t.Errorf("destination ID = %q, want %q", dstFound.ID, parentID)
	}
	if dstFound.SubagentCount != 1 {
		t.Errorf("destination SubagentCount = %d, want 1", dstFound.SubagentCount)
	}
	if dstFound.StepCount != 4 {
		t.Errorf("destination StepCount = %d, want 4", dstFound.StepCount)
	}

	// 5. Verify unrelatedSrcID was NOT exported into dstHome, and unrelatedDstID is completely untouched!
	if _, err := FindSession(dstHome, unrelatedSrcID, ""); !providers.IsNotFoundError(err) {
		t.Errorf("unrelated source thread %s should not exist in destination, got err=%v", unrelatedSrcID, err)
	}
	keepMeta, err := FindSession(dstHome, unrelatedDstID, "")
	if err != nil || keepMeta.StepCount != 3 {
		t.Errorf("unrelated destination thread %s was affected: meta=%+v err=%v", unrelatedDstID, keepMeta, err)
	}

	// 6. Verify rollout file header was rewritten to dstWS and rollout_path points inside dstHome
	dstRollout := filepath.Join(dstHome, "sessions", "2026", "10", "15", "rollout-2026-10-15T10-00-00-"+parentID+".jsonl")
	data, err := os.ReadFile(dstRollout)
	if err != nil {
		t.Fatalf("reading destination rollout: %v", err)
	}
	firstLine := strings.SplitN(string(data), "\n", 2)[0]
	var header struct {
		Payload struct {
			Cwd                   string   `json:"cwd"`
			RuntimeWorkspaceRoots []string `json:"runtime_workspace_roots"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(firstLine), &header); err != nil {
		t.Fatal(err)
	}
	if header.Payload.Cwd != dstWS {
		t.Errorf("rollout header cwd = %q, want %q", header.Payload.Cwd, dstWS)
	}

	// 7. Test stale overwrite protection: advance dstHome session and try re-importing older bundle without --force
	seedCodexFixture(t, dstHome, parentID, childID, dstWS, 1792199999, 6)

	var olderBundle bytes.Buffer
	if _, err := ExportBundle(srcHome, parentID, "", &olderBundle); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportBundle(dstHome, &olderBundle, dstWS, false); !providers.IsStaleOverwriteError(err) {
		t.Fatalf("expected StaleOverwriteError when destination is newer, got: %v", err)
	}

	// With --force = true, overwrite succeeds
	var forceBundle bytes.Buffer
	if _, err := ExportBundle(srcHome, parentID, "", &forceBundle); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportBundle(dstHome, &forceBundle, dstWS, true); err != nil {
		t.Fatalf("expected ImportBundle with force=true to succeed, got: %v", err)
	}

	// 8. Test lock contention protection on child subagent
	releaseSubLock, err := lock.AcquireExclusive("codex", childID, lock.CodexLockPath(srcHome, childID))
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSubLock()

	var lockedBundle bytes.Buffer
	if _, err := ExportBundle(srcHome, parentID, "", &lockedBundle); !lock.IsLockedError(err) {
		t.Fatalf("expected LockedError when child subagent lock is held, got: %v", err)
	}
}

func TestDeleteCodexSessionAndSubagents(t *testing.T) {
	home := t.TempDir()
	ws := "/workspace/test-proj"
	parentID := "01a14200-1111-2222-3333-444455556666"
	childID := "01a14200-7777-8888-9999-aaaabbbbcccc"

	seedCodexFixture(t, home, parentID, childID, ws, 1792100000, 5)

	// Verify session can be listed and found
	meta, err := FindSession(home, parentID, "")
	if err != nil {
		t.Fatalf("FindSession failed: %v", err)
	}
	if meta.SubagentCount != 1 {
		t.Errorf("expected 1 subagent, got %d", meta.SubagentCount)
	}

	// 1. When locked, DeleteSession must fail and not delete anything
	release, err := lock.AcquireExclusive("codex", parentID, lock.CodexLockPath(home, parentID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DeleteSession(home, parentID); !lock.IsLockedError(err) {
		t.Fatalf("expected LockedError for locked session, got: %v", err)
	}
	release()

	// 2. Delete the session
	deleted, err := DeleteSession(home, parentID)
	if err != nil {
		t.Fatalf("DeleteSession failed: %v", err)
	}
	if deleted.ID != parentID {
		t.Errorf("deleted ID = %s, want %s", deleted.ID, parentID)
	}

	// 3. Verify session no longer found
	if _, err := FindSession(home, parentID, ""); err == nil {
		t.Fatalf("expected error finding deleted session, got nil")
	}

	// 4. Verify threads table is empty
	stateDB, err := openSQLite(filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateDB.Close()

	var count int
	if err := stateDB.QueryRow("SELECT COUNT(*) FROM threads WHERE id IN (?, ?)", parentID, childID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 threads in DB after delete, found %d", count)
	}
}
