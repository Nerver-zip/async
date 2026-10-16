package agy

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Nerver-zip/async/internal/lock"
	"github.com/Nerver-zip/async/internal/pathmap"
	"github.com/Nerver-zip/async/internal/providers"
)

func seedAGYFixture(t *testing.T, agyHome, convID, childID, workspace, lastMod string, stepCount int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(agyHome, "conversations"), 0o755); err != nil {
		t.Fatal(err)
	}

	wsURI := pathmap.PathToFileURI(workspace)
	rawProto := []byte("\n\xb2\x01\n&file:///home/user/projects/sample-apps\x12&file:///home/user/projects/sample-apps\x1a=\n\x11sample/sample-app\x12(https://github.com/sample/sample-app.git\"!feat/board-first-review-workspace\x12\x0c\x08\xa6\xdd\xc3\xd6\x06\x10\xd3\xa4\xe6\xc1\x02\x1a$56016b76-6ab0-4e6b-9a6d-4aa3816c6c992$abc28ad7-564c-44e6-abe8-6e9f638967b3:&file:///home/user/projects/sample-apps")
	rawProto = pathmap.RewriteProtobufURIs(rawProto, "file:///home/user/projects/sample-apps", wsURI)

	// 1. Seed conversation_summaries.db
	sumDB, err := openSQLite(filepath.Join(agyHome, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sumDB.Close()

	if _, err := sumDB.Exec(summariesSchemaDDL); err != nil {
		t.Fatal(err)
	}

	wsJSON := `["` + wsURI + `"]`
	_, err = sumDB.Exec(`
INSERT OR REPLACE INTO conversation_summaries (conversation_id, title, preview, step_count, last_modified_time, workspace_uris, parent_conversation_id, last_user_input_time, app_data_dir, raw_summary)
VALUES (?, ?, 'Preview text', ?, ?, ?, '', ?, 'antigravity-cli', ?)
`, convID, "AGY Session "+convID, stepCount, lastMod, wsJSON, lastMod, rawProto)
	if err != nil {
		t.Fatal(err)
	}

	if childID != "" {
		_, err = sumDB.Exec(`
INSERT OR REPLACE INTO conversation_summaries (conversation_id, title, preview, step_count, last_modified_time, workspace_uris, parent_conversation_id, last_user_input_time, app_data_dir, raw_summary)
VALUES (?, 'Subagent Task', 'Sub preview', 12, ?, ?, ?, ?, 'antigravity-cli', ?)
`, childID, lastMod, wsJSON, convID, lastMod, rawProto)
		if err != nil {
			t.Fatal(err)
		}
	}

	// 2. Seed conversations/<convID>.db
	createConvDB := func(id string) {
		cdb, err := openSQLite(filepath.Join(agyHome, "conversations", id+".db"))
		if err != nil {
			t.Fatal(err)
		}
		defer cdb.Close()
		_, err = cdb.Exec(`
CREATE TABLE IF NOT EXISTS trajectory_meta (trajectory_id text PRIMARY KEY, cascade_id text, trajectory_type integer, source integer);
CREATE TABLE IF NOT EXISTS steps (idx integer PRIMARY KEY, step_type integer NOT NULL DEFAULT 0, status integer NOT NULL DEFAULT 0, step_payload blob);
CREATE TABLE IF NOT EXISTS trajectory_metadata_blob (id text DEFAULT "main" PRIMARY KEY, data blob);
`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cdb.Exec(`INSERT OR REPLACE INTO trajectory_meta (trajectory_id, cascade_id, trajectory_type, source) VALUES (?, ?, 1, 1)`, id, id); err != nil {
			t.Fatal(err)
		}
		if _, err := cdb.Exec(`INSERT OR REPLACE INTO trajectory_metadata_blob (id, data) VALUES ('main', ?)`, rawProto); err != nil {
			t.Fatal(err)
		}
	}
	createConvDB(convID)
	if childID != "" {
		createConvDB(childID)
	}

	// 3. Seed brain/<convID>/ artifacts and transcript
	brainLogsDir := filepath.Join(agyHome, "brain", convID, ".system_generated", "logs")
	if err := os.MkdirAll(brainLogsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainLogsDir, "transcript.jsonl"), []byte(`{"step_index":0,"content":"hello"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agyHome, "brain", convID, "implementation_plan.md"), []byte("# Implementation Plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAGYExportImportRewritesAndLocks(t *testing.T) {
	srcHome := t.TempDir()
	dstHome := t.TempDir()

	srcWS := "/home/user/projects/sample-apps"
	dstWS := "/home/user/workspace/sample-apps"

	convID := "abc28ad7-564c-44e6-abe8-6e9f638967b3"
	childID := "sub28ad7-564c-44e6-abe8-6e9f638967b4"
	unrelatedDstID := "99999999-9999-4999-9999-999999999999"

	seedAGYFixture(t, srcHome, convID, childID, srcWS, "2026-10-15T15:50:06Z", 224)
	seedAGYFixture(t, dstHome, unrelatedDstID, "", "/home/user/workspace/other", "2026-10-14T10:00:00Z", 50)

	// 1. Auto-detect latest session for srcWS
	found, err := FindSession(srcHome, "", srcWS)
	if err != nil {
		t.Fatalf("FindSession failed: %v", err)
	}
	if found.ID != convID {
		t.Fatalf("got ID %q, want %q", found.ID, convID)
	}
	if found.SubagentCount != 1 {
		t.Errorf("SubagentCount = %d, want 1", found.SubagentCount)
	}

	// 2. Export bundle
	var bundle bytes.Buffer
	if _, err := ExportBundle(srcHome, "", srcWS, &bundle); err != nil {
		t.Fatalf("ExportBundle failed: %v", err)
	}

	// 3. Import into dstHome with path translation to dstWS
	importedMeta, err := ImportBundle(dstHome, &bundle, dstWS, false)
	if err != nil {
		t.Fatalf("ImportBundle failed: %v", err)
	}
	if importedMeta.Workspace != dstWS {
		t.Errorf("imported Workspace = %q, want %q", importedMeta.Workspace, dstWS)
	}

	// 4. Verify session is discoverable under dstWS in dstHome
	dstFound, err := FindSession(dstHome, "", dstWS)
	if err != nil {
		t.Fatalf("FindSession on destination failed: %v", err)
	}
	if dstFound.ID != convID || dstFound.StepCount != 224 || dstFound.SubagentCount != 1 {
		t.Errorf("unexpected destination session meta: %+v", dstFound)
	}

	// 5. Verify brain artifacts and transcript were copied
	planBytes, err := os.ReadFile(filepath.Join(dstHome, "brain", convID, "implementation_plan.md"))
	if err != nil || string(planBytes) != "# Implementation Plan\n" {
		t.Errorf("brain artifact missing or corrupted: %v", err)
	}

	// 6. Verify trajectory_metadata_blob inside destination conversations/<convID>.db has rewritten URI
	cdb, err := openSQLite(filepath.Join(dstHome, "conversations", convID+".db"))
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	var blob []byte
	if err := cdb.QueryRow(`SELECT data FROM trajectory_metadata_blob WHERE id='main'`).Scan(&blob); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(blob, []byte("file:///home/user/projects/sample-apps")) {
		t.Errorf("destination trajectory_metadata_blob still contains source URI")
	}
	if !bytes.Contains(blob, []byte("file:///home/user/workspace/sample-apps")) {
		t.Errorf("destination trajectory_metadata_blob missing target URI")
	}

	// 7. Verify unrelated destination session is untouched
	if keep, err := FindSession(dstHome, unrelatedDstID, ""); err != nil || keep.StepCount != 50 {
		t.Errorf("unrelated destination session affected: %+v err=%v", keep, err)
	}

	// 8. Test stale overwrite protection
	seedAGYFixture(t, dstHome, convID, childID, dstWS, "2026-10-16T18:00:00Z", 300)
	var staleBundle bytes.Buffer
	if _, err := ExportBundle(srcHome, convID, "", &staleBundle); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportBundle(dstHome, &staleBundle, dstWS, false); !providers.IsStaleOverwriteError(err) {
		t.Fatalf("expected StaleOverwriteError when destination is ahead, got: %v", err)
	}

	// 9. Test presence lock protection
	releaseLock, err := lock.AcquireExclusive("agy", convID, lock.AGYLockPath(srcHome, convID))
	if err != nil {
		t.Fatal(err)
	}
	defer releaseLock()

	var lockedBuf bytes.Buffer
	if _, err := ExportBundle(srcHome, convID, "", &lockedBuf); !lock.IsLockedError(err) {
		t.Fatalf("expected LockedError when presence lock is held, got: %v", err)
	}
}

func TestDeleteAGYSessionAndSubagents(t *testing.T) {
	home := t.TempDir()
	ws := "/workspace/my-agy-app"
	convID := "99991111-2222-3333-4444-555566667777"
	childID := "99991111-aaaa-bbbb-cccc-dddd88889999"

	seedAGYFixture(t, home, convID, childID, ws, "2026-10-15T12:00:00Z", 42)

	// Verify session found
	meta, err := FindSession(home, convID, "")
	if err != nil {
		t.Fatalf("FindSession failed: %v", err)
	}
	if meta.SubagentCount != 1 {
		t.Errorf("expected 1 subagent, got %d", meta.SubagentCount)
	}

	// 1. Lock session: DeleteSession must fail
	release, err := lock.AcquireExclusive("agy", convID, lock.AGYLockPath(home, convID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DeleteSession(home, convID); !lock.IsLockedError(err) {
		t.Fatalf("expected LockedError for locked session, got: %v", err)
	}
	release()

	// 2. Delete session
	deleted, err := DeleteSession(home, convID)
	if err != nil {
		t.Fatalf("DeleteSession failed: %v", err)
	}
	if deleted.ID != convID {
		t.Errorf("deleted ID = %s, want %s", deleted.ID, convID)
	}

	// 3. Verify session no longer found
	if _, err := FindSession(home, convID, ""); err == nil {
		t.Fatalf("expected error finding deleted session, got nil")
	}

	// 4. Verify conversation_summaries table is empty
	sumDB, err := openSQLite(filepath.Join(home, "conversation_summaries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sumDB.Close()

	var count int
	if err := sumDB.QueryRow("SELECT COUNT(*) FROM conversation_summaries WHERE conversation_id IN (?, ?)", convID, childID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("expected 0 summaries after delete, got %d", count)
	}

	// 5. Verify brain dir and db files removed
	if _, err := os.Stat(filepath.Join(home, "brain", convID)); !os.IsNotExist(err) {
		t.Errorf("brain dir should be removed")
	}
	if _, err := os.Stat(filepath.Join(home, "conversations", convID+".db")); !os.IsNotExist(err) {
		t.Errorf("conversations db should be removed")
	}
}

