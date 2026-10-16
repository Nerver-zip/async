package agy

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Nerver-zip/async/internal/archive"
	"github.com/Nerver-zip/async/internal/lock"
	"github.com/Nerver-zip/async/internal/pathmap"
	"github.com/Nerver-zip/async/internal/providers"
	_ "modernc.org/sqlite"
)

// BundleManifest is stored as manifest.json inside an Antigravity CLI session bundle.
type BundleManifest struct {
	Version         int                    `json:"version"`
	Meta            providers.SessionMeta  `json:"meta"`
	ConversationIDs []string               `json:"conversation_ids"`
	SummarySlice    *providers.TableSlice  `json:"summary_slice"`
}

const summariesSchemaDDL = `
CREATE TABLE IF NOT EXISTS conversation_summaries (
    conversation_id text PRIMARY KEY,
    title text NOT NULL DEFAULT "",
    preview text NOT NULL DEFAULT "",
    step_count integer NOT NULL DEFAULT 0,
    last_modified_time datetime NOT NULL,
    workspace_uris text NOT NULL,
    status text NOT NULL DEFAULT "",
    source text NOT NULL DEFAULT "",
    project_id text NOT NULL DEFAULT "",
    agent_name text NOT NULL DEFAULT "",
    parent_conversation_id text NOT NULL DEFAULT "",
    nesting_depth integer NOT NULL DEFAULT 0,
    battle_id text NOT NULL DEFAULT "",
    winning_conversation_id text NOT NULL DEFAULT "",
    not_fully_idle numeric NOT NULL DEFAULT false,
    killed numeric NOT NULL DEFAULT false,
    last_user_input_time datetime NOT NULL,
    last_user_input_step_index integer NOT NULL DEFAULT -1,
    app_data_dir text NOT NULL DEFAULT "",
    raw_summary BLOB,
    group_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_conversation_summaries_last_user_input_time ON conversation_summaries(last_user_input_time);
CREATE INDEX IF NOT EXISTS idx_conversation_summaries_last_modified_time ON conversation_summaries(last_modified_time);
`

func openSQLite(dbPath string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", filepath.ToSlash(dbPath))
	return sql.Open("sqlite", dsn)
}

// ListSessions returns Antigravity CLI top-level sessions ordered by last_modified_time DESC.
func ListSessions(agyHome, workspace string, all bool) ([]providers.SessionMeta, error) {
	sumPath := filepath.Join(agyHome, "conversation_summaries.db")
	if _, err := os.Stat(sumPath); os.IsNotExist(err) {
		return nil, nil
	}
	db, err := openSQLite(sumPath)
	if err != nil {
		return nil, fmt.Errorf("opening conversation_summaries.db: %w", err)
	}
	defer db.Close()

	exists, err := providers.TableExists(db, "conversation_summaries")
	if err != nil || !exists {
		return nil, err
	}

	rows, err := db.Query(`
SELECT conversation_id, title, preview, step_count, CAST(last_modified_time AS TEXT), workspace_uris
FROM conversation_summaries
WHERE parent_conversation_id = '' OR parent_conversation_id IS NULL
ORDER BY last_modified_time DESC, conversation_id DESC
`)
	if err != nil {
		return nil, fmt.Errorf("querying conversation_summaries: %w", err)
	}
	defer rows.Close()

	cleanFilterWS := ""
	if !all && workspace != "" {
		cleanFilterWS = filepath.Clean(pathmap.ExpandPath(workspace))
	}

	type rawSummary struct {
		id            string
		title         string
		preview       string
		stepCount     int
		lastModStr    string
		workspaceURIs string
	}
	var items []rawSummary
	for rows.Next() {
		var r rawSummary
		if err := rows.Scan(&r.id, &r.title, &r.preview, &r.stepCount, &r.lastModStr, &r.workspaceURIs); err != nil {
			return nil, err
		}
		items = append(items, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var results []providers.SessionMeta
	for _, r := range items {
		wsPath := parseFirstWorkspaceURI(r.workspaceURIs)
		if cleanFilterWS != "" && wsPath != cleanFilterWS {
			continue
		}

		family, _ := collectConversationFamily(db, r.id)
		locked := false
		for _, cid := range family {
			if lock.IsLocked("agy", cid, lock.AGYLockPath(agyHome, cid)) {
				locked = true
				break
			}
		}

		tsMs, isoStr := parseAGYTimestamp(r.lastModStr)
		results = append(results, providers.SessionMeta{
			ID:            r.id,
			Provider:      "agy",
			Title:         r.title,
			Preview:       r.preview,
			Workspace:     wsPath,
			UpdatedAt:     tsMs,
			UpdatedAtISO:  isoStr,
			StepCount:     r.stepCount,
			SubagentCount: len(family) - 1,
			Locked:        locked,
		})
	}
	return results, nil
}

// FindSession locates a specific Antigravity CLI session by ID (exact or prefix) or the latest session for workspace.
func FindSession(agyHome, id, workspace string) (*providers.SessionMeta, error) {
	if id == "" {
		list, err := ListSessions(agyHome, workspace, false)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return nil, &providers.NotFoundError{Provider: "agy", Workspace: workspace}
		}
		return &list[0], nil
	}

	sumPath := filepath.Join(agyHome, "conversation_summaries.db")
	if _, err := os.Stat(sumPath); os.IsNotExist(err) {
		return nil, &providers.NotFoundError{Provider: "agy", ID: id}
	}
	db, err := openSQLite(sumPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	exists, err := providers.TableExists(db, "conversation_summaries")
	if err != nil || !exists {
		return nil, &providers.NotFoundError{Provider: "agy", ID: id}
	}

	rows, err := db.Query(`
SELECT conversation_id, title, preview, step_count, CAST(last_modified_time AS TEXT), workspace_uris
FROM conversation_summaries
WHERE conversation_id = ? OR conversation_id LIKE ? || '%'
ORDER BY last_modified_time DESC
`, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type rawSummary struct {
		id            string
		title         string
		preview       string
		stepCount     int
		lastModStr    string
		workspaceURIs string
	}
	var matches []rawSummary
	for rows.Next() {
		var m rawSummary
		if err := rows.Scan(&m.id, &m.title, &m.preview, &m.stepCount, &m.lastModStr, &m.workspaceURIs); err != nil {
			return nil, err
		}
		matches = append(matches, m)
	}
	if len(matches) == 0 {
		return nil, &providers.NotFoundError{Provider: "agy", ID: id}
	}
	if len(matches) > 1 && matches[0].id != id {
		return nil, fmt.Errorf("ambiguous agy session id prefix %q matched %d sessions", id, len(matches))
	}

	chosen := matches[0]
	family, _ := collectConversationFamily(db, chosen.id)
	locked := false
	for _, cid := range family {
		if lock.IsLocked("agy", cid, lock.AGYLockPath(agyHome, cid)) {
			locked = true
			break
		}
	}
	tsMs, isoStr := parseAGYTimestamp(chosen.lastModStr)

	return &providers.SessionMeta{
		ID:            chosen.id,
		Provider:      "agy",
		Title:         chosen.title,
		Preview:       chosen.preview,
		Workspace:     parseFirstWorkspaceURI(chosen.workspaceURIs),
		UpdatedAt:     tsMs,
		UpdatedAtISO:  isoStr,
		StepCount:     chosen.stepCount,
		SubagentCount: len(family) - 1,
		Locked:        locked,
	}, nil
}

// CheckSessionLocks verifies that neither the root conversation nor any child subagent holds an active presence lock.
func CheckSessionLocks(agyHome, id string) error {
	if err := lock.CheckAvailable("agy", id, lock.AGYLockPath(agyHome, id)); err != nil {
		return err
	}
	sumPath := filepath.Join(agyHome, "conversation_summaries.db")
	if _, err := os.Stat(sumPath); err != nil {
		return nil
	}
	db, err := openSQLite(sumPath)
	if err != nil {
		return nil
	}
	defer db.Close()

	family, _ := collectConversationFamily(db, id)
	for _, cid := range family {
		if err := lock.CheckAvailable("agy", cid, lock.AGYLockPath(agyHome, cid)); err != nil {
			return err
		}
	}
	return nil
}

// ExportBundle packages an Antigravity CLI conversation (DB, brain directory, summary metadata, and subagent conversations) into a tar.gz stream.
func ExportBundle(agyHome, id, workspace string, w io.Writer) (*providers.SessionMeta, error) {
	meta, err := FindSession(agyHome, id, workspace)
	if err != nil {
		return nil, err
	}

	if err := CheckSessionLocks(agyHome, meta.ID); err != nil {
		return nil, err
	}

	sumPath := filepath.Join(agyHome, "conversation_summaries.db")
	sumDB, err := openSQLite(sumPath)
	if err != nil {
		return nil, fmt.Errorf("opening conversation_summaries.db: %w", err)
	}
	defer sumDB.Close()
	_, _ = sumDB.Exec(`PRAGMA wal_checkpoint(PASSIVE)`)

	family, err := collectConversationFamily(sumDB, meta.ID)
	if err != nil {
		return nil, err
	}

	inClause, inArgs := buildInClause(family)
	sumSlice, err := providers.ExportTableSlice(sumDB, "conversation_summaries", "conversation_id IN "+inClause, inArgs...)
	if err != nil {
		return nil, fmt.Errorf("exporting conversation_summaries rows: %w", err)
	}

	stageDir, err := os.MkdirTemp("", "async-agy-export-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stageDir)

	for _, cid := range family {
		// 1. Checkpoint and stage conversations/<cid>.db
		convDBPath := filepath.Join(agyHome, "conversations", cid+".db")
		if st, err := os.Stat(convDBPath); err == nil && st.Mode().IsRegular() {
			if cdb, err := openSQLite(convDBPath); err == nil {
				_, _ = cdb.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
				_ = cdb.Close()
			}
			stagedConvDB := filepath.Join(stageDir, "conversations", cid+".db")
			if err := archive.AtomicInstallFile(convDBPath, stagedConvDB, 0o600); err != nil {
				return nil, fmt.Errorf("staging conversation db %s: %w", convDBPath, err)
			}
		}

		// 2. Stage brain/<cid>/ directory
		brainDir := filepath.Join(agyHome, "brain", cid)
		if st, err := os.Stat(brainDir); err == nil && st.IsDir() {
			stagedBrain := filepath.Join(stageDir, "brain", cid)
			if err := archive.AtomicCopyTree(brainDir, stagedBrain); err != nil {
				return nil, fmt.Errorf("staging brain dir %s: %w", brainDir, err)
			}
		}

		// 3. Stage optional annotations/<cid>.pbtxt and implicit/<cid>.pb
		annotPath := filepath.Join(agyHome, "annotations", cid+".pbtxt")
		if st, err := os.Stat(annotPath); err == nil && st.Mode().IsRegular() {
			_ = archive.AtomicInstallFile(annotPath, filepath.Join(stageDir, "annotations", cid+".pbtxt"), 0o600)
		}
		impPath := filepath.Join(agyHome, "implicit", cid+".pb")
		if st, err := os.Stat(impPath); err == nil && st.Mode().IsRegular() {
			_ = archive.AtomicInstallFile(impPath, filepath.Join(stageDir, "implicit", cid+".pb"), 0o600)
		}
	}

	manifest := &BundleManifest{
		Version:         1,
		Meta:            *meta,
		ConversationIDs: family,
		SummarySlice:    sumSlice,
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(stageDir, "manifest.json"), manifestBytes, 0o644); err != nil {
		return nil, err
	}

	if err := archive.PackDirTarGz(stageDir, w); err != nil {
		return nil, fmt.Errorf("packing agy bundle: %w", err)
	}
	return meta, nil
}

// ImportBundle unpacks an Antigravity CLI bundle and merges its conversation databases, brain artifacts, and summary rows into agyHome.
func ImportBundle(agyHome string, r io.Reader, targetWorkspace string, force bool) (*providers.SessionMeta, error) {
	if err := os.MkdirAll(agyHome, 0o755); err != nil {
		return nil, fmt.Errorf("creating agy home %s: %w", agyHome, err)
	}

	stageDir, cleanup, err := archive.UnpackTarGzToStaged(r, agyHome)
	if err != nil {
		return nil, fmt.Errorf("unpacking agy bundle: %w", err)
	}
	defer cleanup()

	manifestBytes, err := os.ReadFile(filepath.Join(stageDir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("reading manifest.json: %w", err)
	}
	var manifest BundleManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("parsing manifest.json: %w", err)
	}

	// Verify no target conversations are locked
	for _, cid := range manifest.ConversationIDs {
		if err := lock.CheckAvailable("agy", cid, lock.AGYLockPath(agyHome, cid)); err != nil {
			return nil, err
		}
	}

	releaseRootLock, err := lock.AcquireExclusive("agy", manifest.Meta.ID, lock.AGYLockPath(agyHome, manifest.Meta.ID))
	if err != nil {
		return nil, err
	}
	defer releaseRootLock()

	// Check conflict / stale overwrite protection
	if existing, err := FindSession(agyHome, manifest.Meta.ID, ""); err == nil && existing != nil {
		if !force {
			if existing.UpdatedAt > manifest.Meta.UpdatedAt ||
				(existing.UpdatedAt == manifest.Meta.UpdatedAt && existing.StepCount > manifest.Meta.StepCount) {
				return nil, &providers.StaleOverwriteError{
					Provider:  "agy",
					SessionID: manifest.Meta.ID,
					SrcMeta:   manifest.Meta,
					DstMeta:   *existing,
				}
			}
		}
	}

	cleanTargetWS := ""
	if targetWorkspace != "" {
		cleanTargetWS = filepath.Clean(pathmap.ExpandPath(targetWorkspace))
	}

	oldURI := ""
	if manifest.Meta.Workspace != "" {
		oldURI = pathmap.PathToFileURI(manifest.Meta.Workspace)
	}
	newURI := ""
	if cleanTargetWS != "" {
		newURI = pathmap.PathToFileURI(cleanTargetWS)
	}

	// Install conversation DBs, brain directories, annotations, and implicit files
	for _, cid := range manifest.ConversationIDs {
		stagedConvDB := filepath.Join(stageDir, "conversations", cid+".db")
		if _, err := os.Stat(stagedConvDB); err == nil {
			if oldURI != "" && newURI != "" && oldURI != newURI {
				if err := rewriteConvDBMetadataBlob(stagedConvDB, oldURI, newURI); err != nil {
					return nil, fmt.Errorf("rewriting metadata blob in %s.db: %w", cid, err)
				}
			}
			dstConvDB := filepath.Join(agyHome, "conversations", cid+".db")
			_ = os.Remove(dstConvDB + "-wal")
			_ = os.Remove(dstConvDB + "-shm")
			if err := archive.AtomicInstallFile(stagedConvDB, dstConvDB, 0o600); err != nil {
				return nil, fmt.Errorf("installing conversation db %s: %w", dstConvDB, err)
			}
		}

		stagedBrain := filepath.Join(stageDir, "brain", cid)
		if st, err := os.Stat(stagedBrain); err == nil && st.IsDir() {
			dstBrain := filepath.Join(agyHome, "brain", cid)
			if err := archive.AtomicCopyTree(stagedBrain, dstBrain); err != nil {
				return nil, fmt.Errorf("installing brain dir %s: %w", dstBrain, err)
			}
		}

		stagedAnnot := filepath.Join(stageDir, "annotations", cid+".pbtxt")
		if _, err := os.Stat(stagedAnnot); err == nil {
			_ = archive.AtomicInstallFile(stagedAnnot, filepath.Join(agyHome, "annotations", cid+".pbtxt"), 0o600)
		}
		stagedImp := filepath.Join(stageDir, "implicit", cid+".pb")
		if _, err := os.Stat(stagedImp); err == nil {
			_ = archive.AtomicInstallFile(stagedImp, filepath.Join(agyHome, "implicit", cid+".pb"), 0o600)
		}
	}

	// Rewrite workspace_uris and raw_summary in SummarySlice and upsert into conversation_summaries.db
	if manifest.SummarySlice != nil {
		for i, row := range manifest.SummarySlice.Rows {
			if cleanTargetWS != "" {
				rewrittenURIs := pathmap.RewriteWorkspaceURIsJSON(row["workspace_uris"].Str, manifest.Meta.Workspace, cleanTargetWS)
				row["workspace_uris"] = providers.FromSQL(rewrittenURIs)

				if rawTV, ok := row["raw_summary"]; ok && rawTV.Type == "blob" && oldURI != "" && newURI != "" {
					if rawBytes, err := base64.StdEncoding.DecodeString(rawTV.Str); err == nil {
						updatedBlob := pathmap.RewriteProtobufURIs(rawBytes, oldURI, newURI)
						row["raw_summary"] = providers.FromSQL(updatedBlob)
					}
				}
			}
			manifest.SummarySlice.Rows[i] = row
		}

		sumPath := filepath.Join(agyHome, "conversation_summaries.db")
		sumDB, err := openSQLite(sumPath)
		if err != nil {
			return nil, fmt.Errorf("opening destination conversation_summaries.db: %w", err)
		}
		defer sumDB.Close()

		if _, err := sumDB.Exec(summariesSchemaDDL); err != nil {
			return nil, fmt.Errorf("creating conversation_summaries schema: %w", err)
		}

		tx, err := sumDB.Begin()
		if err != nil {
			return nil, err
		}
		if err := providers.ImportTableSlice(sumDB, tx, manifest.SummarySlice); err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("upserting conversation_summaries: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		_, _ = sumDB.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	}

	// Remove cached protobuf summary index so AGY regenerates/reconciles on startup
	_ = os.Remove(filepath.Join(agyHome, "jetbox_summaries_proto.pb"))

	if cleanTargetWS != "" {
		manifest.Meta.Workspace = cleanTargetWS
	}
	return &manifest.Meta, nil
}

func rewriteConvDBMetadataBlob(dbPath, oldURI, newURI string) error {
	db, err := openSQLite(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	exists, err := providers.TableExists(db, "trajectory_metadata_blob")
	if err != nil || !exists {
		return nil
	}

	rows, err := db.Query(`SELECT id, data FROM trajectory_metadata_blob`)
	if err != nil {
		return nil
	}
	type blobItem struct {
		id   string
		data []byte
	}
	var items []blobItem
	for rows.Next() {
		var it blobItem
		if err := rows.Scan(&it.id, &it.data); err == nil {
			items = append(items, it)
		}
	}
	rows.Close()

	for _, it := range items {
		updated := pathmap.RewriteProtobufURIs(it.data, oldURI, newURI)
		if _, err := db.Exec(`UPDATE trajectory_metadata_blob SET data = ? WHERE id = ?`, updated, it.id); err != nil {
			return err
		}
	}
	_, _ = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return nil
}

func collectConversationFamily(db *sql.DB, rootID string) ([]string, error) {
	visited := map[string]bool{rootID: true}
	queue := []string{rootID}
	result := []string{rootID}

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		rows, err := db.Query(`SELECT conversation_id FROM conversation_summaries WHERE parent_conversation_id = ? ORDER BY conversation_id`, curr)
		if err == nil {
			for rows.Next() {
				var child string
				if err := rows.Scan(&child); err == nil && child != "" && !visited[child] {
					visited[child] = true
					queue = append(queue, child)
					result = append(result, child)
				}
			}
			rows.Close()
		}
	}
	return result, nil
}

func parseFirstWorkspaceURI(rawJSON string) string {
	var uris []string
	if err := json.Unmarshal([]byte(rawJSON), &uris); err != nil || len(uris) == 0 {
		return ""
	}
	return pathmap.FileURIToPath(uris[0])
}

func parseAGYTimestamp(s string) (int64, string) {
	s = strings.TrimSpace(s)
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999+00:00",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli(), t.UTC().Format(time.RFC3339)
		}
	}
	return 0, s
}

func buildInClause(ids []string) (string, []any) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return "(" + strings.Join(placeholders, ",") + ")", args
}
