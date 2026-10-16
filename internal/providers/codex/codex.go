package codex

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
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

// BundleManifest is serialized as manifest.json inside a Codex session bundle.
type BundleManifest struct {
	Version         int                               `json:"version"`
	Meta            providers.SessionMeta             `json:"meta"`
	ThreadIDs       []string                          `json:"thread_ids"`
	RolloutRelPaths map[string]string                 `json:"rollout_rel_paths"`
	StateSlices     map[string]*providers.TableSlice  `json:"state_slices"`
	HistorySlices   map[string]*providers.TableSlice  `json:"history_slices,omitempty"`
	GoalsSlices     map[string]*providers.TableSlice  `json:"goals_slices,omitempty"`
	MemoriesSlices  map[string]*providers.TableSlice  `json:"memories_slices,omitempty"`
}

const state5SchemaDDL = `
CREATE TABLE IF NOT EXISTS threads (
    id TEXT PRIMARY KEY,
    rollout_path TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    source TEXT NOT NULL,
    model_provider TEXT NOT NULL,
    cwd TEXT NOT NULL,
    title TEXT NOT NULL,
    sandbox_policy TEXT NOT NULL,
    approval_mode TEXT NOT NULL,
    tokens_used INTEGER NOT NULL DEFAULT 0,
    has_user_event INTEGER NOT NULL DEFAULT 0,
    archived INTEGER NOT NULL DEFAULT 0,
    archived_at INTEGER,
    git_sha TEXT,
    git_branch TEXT,
    git_origin_url TEXT,
    cli_version TEXT NOT NULL DEFAULT '',
    first_user_message TEXT NOT NULL DEFAULT '',
    agent_nickname TEXT,
    agent_role TEXT,
    memory_mode TEXT NOT NULL DEFAULT 'enabled',
    model TEXT,
    reasoning_effort TEXT,
    agent_path TEXT,
    created_at_ms INTEGER,
    updated_at_ms INTEGER,
    thread_source TEXT,
    preview TEXT NOT NULL DEFAULT '',
    recency_at INTEGER NOT NULL DEFAULT 0,
    recency_at_ms INTEGER NOT NULL DEFAULT 0,
    history_mode TEXT NOT NULL DEFAULT 'legacy',
    name TEXT,
    is_pinned INTEGER NOT NULL DEFAULT 0,
    thread_section_id TEXT,
    section_position INTEGER,
    section_entered_at_ms INTEGER,
    project_id TEXT,
    originator TEXT,
    daybreak_enabled BOOLEAN,
    creator_user_id TEXT,
    creator_account_id TEXT
);
CREATE TABLE IF NOT EXISTS thread_spawn_edges (
    parent_thread_id TEXT NOT NULL,
    child_thread_id TEXT NOT NULL PRIMARY KEY,
    status TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS thread_dynamic_tools (
    thread_id TEXT NOT NULL,
    position INTEGER NOT NULL,
    name TEXT NOT NULL,
    description TEXT NOT NULL,
    input_schema TEXT NOT NULL,
    defer_loading INTEGER NOT NULL DEFAULT 0,
    namespace TEXT,
    PRIMARY KEY(thread_id, position)
);
CREATE TABLE IF NOT EXISTS thread_attachments (
    id TEXT PRIMARY KEY,
    thread_id TEXT NOT NULL,
    attachment_type TEXT NOT NULL,
    identity_key TEXT NOT NULL,
    payload TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    UNIQUE (thread_id, attachment_type, identity_key)
);
`

const history1SchemaDDL = `
CREATE TABLE IF NOT EXISTS _sqlx_migrations (
    version BIGINT PRIMARY KEY,
    description TEXT NOT NULL,
    installed_on TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    success BOOLEAN NOT NULL,
    checksum BLOB NOT NULL,
    execution_time BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS thread_turns (
    thread_id TEXT NOT NULL,
    turn_id TEXT NOT NULL,
    rollout_ordinal INTEGER NOT NULL,
    status TEXT NOT NULL,
    error_json TEXT,
    started_at INTEGER,
    completed_at INTEGER,
    duration_ms INTEGER,
    first_user_item_id TEXT,
    final_agent_item_id TEXT,
    rollout_byte_offset INTEGER,
    rollout_end_ordinal INTEGER,
    rollout_end_byte_offset INTEGER,
    root_turn_id TEXT,
    PRIMARY KEY (thread_id, turn_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_thread_turns_page ON thread_turns(thread_id, rollout_ordinal);
CREATE TABLE IF NOT EXISTS thread_items (
    thread_id TEXT NOT NULL,
    turn_id TEXT NOT NULL,
    item_id TEXT NOT NULL,
    rollout_ordinal INTEGER NOT NULL,
    created_at_ms INTEGER NOT NULL,
    item_json TEXT NOT NULL,
    item_type TEXT NOT NULL DEFAULT '',
    updated_at_ordinal INTEGER NOT NULL DEFAULT 0,
    started_at_ms INTEGER,
    completed_at_ms INTEGER,
    PRIMARY KEY (thread_id, turn_id, item_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_thread_items_page ON thread_items(thread_id, rollout_ordinal);
CREATE INDEX IF NOT EXISTS idx_thread_items_by_turn_page ON thread_items(thread_id, turn_id, rollout_ordinal);
CREATE TABLE IF NOT EXISTS thread_history_projection_state (
    thread_id TEXT PRIMARY KEY,
    next_rollout_byte_offset INTEGER NOT NULL,
    next_rollout_ordinal INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS thread_realtime_items (
    thread_id TEXT NOT NULL,
    item_id TEXT NOT NULL,
    rollout_ordinal INTEGER NOT NULL,
    created_at_ms INTEGER NOT NULL,
    item_type TEXT NOT NULL,
    item_json TEXT NOT NULL,
    PRIMARY KEY (thread_id, item_id)
);
`

func openSQLite(dbPath string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", filepath.ToSlash(dbPath))
	return sql.Open("sqlite", dsn)
}

// ListSessions returns Codex user sessions ordered by recency.
func ListSessions(codexHome, workspace string, all bool) ([]providers.SessionMeta, error) {
	statePath := filepath.Join(codexHome, "state_5.sqlite")
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		return nil, nil
	}
	db, err := openSQLite(statePath)
	if err != nil {
		return nil, fmt.Errorf("opening state_5.sqlite: %w", err)
	}
	defer db.Close()

	exists, err := providers.TableExists(db, "threads")
	if err != nil || !exists {
		return nil, err
	}

	var histDB *sql.DB
	histPath := filepath.Join(codexHome, "thread_history_1.sqlite")
	if _, err := os.Stat(histPath); err == nil {
		histDB, _ = openSQLite(histPath)
		if histDB != nil {
			defer histDB.Close()
		}
	}

	query := `
SELECT id, rollout_path, cwd, title, COALESCE(preview, ''), updated_at, COALESCE(updated_at_ms, updated_at * 1000)
FROM threads
WHERE archived = 0
  AND (thread_source = 'user' OR thread_source IS NULL OR thread_source = '')
`
	var args []any
	if !all && workspace != "" {
		cleanWS := filepath.Clean(pathmap.ExpandPath(workspace))
		query += ` AND cwd = ?`
		args = append(args, cleanWS)
	}
	query += ` ORDER BY COALESCE(updated_at_ms, updated_at * 1000) DESC, updated_at DESC, id DESC`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("querying threads: %w", err)
	}
	defer rows.Close()

	var rawThreads []struct {
		id          string
		rolloutPath string
		cwd         string
		title       string
		preview     string
		updatedSec  int64
		updatedMs   int64
	}
	for rows.Next() {
		var r struct {
			id          string
			rolloutPath string
			cwd         string
			title       string
			preview     string
			updatedSec  int64
			updatedMs   int64
		}
		if err := rows.Scan(&r.id, &r.rolloutPath, &r.cwd, &r.title, &r.preview, &r.updatedSec, &r.updatedMs); err != nil {
			return nil, err
		}
		rawThreads = append(rawThreads, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	results := make([]providers.SessionMeta, 0, len(rawThreads))
	for _, r := range rawThreads {
		family, _ := collectThreadFamily(db, r.id)
		subCount := 0
		if len(family) > 1 {
			subCount = len(family) - 1
		}
		locked := false
		for _, tid := range family {
			if lock.IsLocked("codex", tid, lock.CodexLockPath(codexHome, tid)) {
				locked = true
				break
			}
		}

		steps := countCodexSteps(histDB, r.id, r.rolloutPath)
		tsMs := r.updatedMs
		if tsMs == 0 && r.updatedSec > 0 {
			tsMs = r.updatedSec * 1000
		}

		results = append(results, providers.SessionMeta{
			ID:            r.id,
			Provider:      "codex",
			Title:         r.title,
			Preview:       r.preview,
			Workspace:     r.cwd,
			UpdatedAt:     tsMs,
			UpdatedAtISO:  time.UnixMilli(tsMs).UTC().Format(time.RFC3339),
			StepCount:     steps,
			SubagentCount: subCount,
			Locked:        locked,
		})
	}
	return results, nil
}

// FindSession locates a specific Codex session by ID (exact or prefix) or the latest session for workspace.
func FindSession(codexHome, id, workspace string) (*providers.SessionMeta, error) {
	if id == "" {
		list, err := ListSessions(codexHome, workspace, false)
		if err != nil {
			return nil, err
		}
		if len(list) == 0 {
			return nil, &providers.NotFoundError{Provider: "codex", Workspace: workspace}
		}
		return &list[0], nil
	}

	statePath := filepath.Join(codexHome, "state_5.sqlite")
	if _, err := os.Stat(statePath); os.IsNotExist(err) {
		return nil, &providers.NotFoundError{Provider: "codex", ID: id}
	}
	db, err := openSQLite(statePath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	exists, err := providers.TableExists(db, "threads")
	if err != nil || !exists {
		return nil, &providers.NotFoundError{Provider: "codex", ID: id}
	}

	rows, err := db.Query(`
SELECT id, rollout_path, cwd, title, COALESCE(preview, ''), updated_at, COALESCE(updated_at_ms, updated_at * 1000)
FROM threads
WHERE id = ? OR id LIKE ? || '%'
ORDER BY COALESCE(updated_at_ms, updated_at * 1000) DESC
`, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var matches []struct {
		id          string
		rolloutPath string
		cwd         string
		title       string
		preview     string
		updatedSec  int64
		updatedMs   int64
	}
	for rows.Next() {
		var m struct {
			id          string
			rolloutPath string
			cwd         string
			title       string
			preview     string
			updatedSec  int64
			updatedMs   int64
		}
		if err := rows.Scan(&m.id, &m.rolloutPath, &m.cwd, &m.title, &m.preview, &m.updatedSec, &m.updatedMs); err != nil {
			return nil, err
		}
		matches = append(matches, m)
	}
	if len(matches) == 0 {
		return nil, &providers.NotFoundError{Provider: "codex", ID: id}
	}
	if len(matches) > 1 && matches[0].id != id {
		return nil, fmt.Errorf("ambiguous codex session id prefix %q matched %d sessions", id, len(matches))
	}

	chosen := matches[0]
	var histDB *sql.DB
	histPath := filepath.Join(codexHome, "thread_history_1.sqlite")
	if _, err := os.Stat(histPath); err == nil {
		histDB, _ = openSQLite(histPath)
		if histDB != nil {
			defer histDB.Close()
		}
	}

	family, _ := collectThreadFamily(db, chosen.id)
	locked := false
	for _, tid := range family {
		if lock.IsLocked("codex", tid, lock.CodexLockPath(codexHome, tid)) {
			locked = true
			break
		}
	}
	tsMs := chosen.updatedMs
	if tsMs == 0 && chosen.updatedSec > 0 {
		tsMs = chosen.updatedSec * 1000
	}

	return &providers.SessionMeta{
		ID:            chosen.id,
		Provider:      "codex",
		Title:         chosen.title,
		Preview:       chosen.preview,
		Workspace:     chosen.cwd,
		UpdatedAt:     tsMs,
		UpdatedAtISO:  time.UnixMilli(tsMs).UTC().Format(time.RFC3339),
		StepCount:     countCodexSteps(histDB, chosen.id, chosen.rolloutPath),
		SubagentCount: len(family) - 1,
		Locked:        locked,
	}, nil
}

// CheckSessionLocks verifies that neither the root session nor any of its subagents holds an active lock.
func CheckSessionLocks(codexHome, id string) error {
	if err := lock.CheckAvailable("codex", id, lock.CodexLockPath(codexHome, id)); err != nil {
		return err
	}
	statePath := filepath.Join(codexHome, "state_5.sqlite")
	if _, err := os.Stat(statePath); err != nil {
		return nil
	}
	db, err := openSQLite(statePath)
	if err != nil {
		return nil
	}
	defer db.Close()

	family, _ := collectThreadFamily(db, id)
	for _, tid := range family {
		if err := lock.CheckAvailable("codex", tid, lock.CodexLockPath(codexHome, tid)); err != nil {
			return err
		}
	}
	return nil
}

// ExportBundle surgically extracts the specified Codex thread and all child subagents into a tar.gz stream written to w.
func ExportBundle(codexHome, id, workspace string, w io.Writer) (*providers.SessionMeta, error) {
	meta, err := FindSession(codexHome, id, workspace)
	if err != nil {
		return nil, err
	}

	if err := CheckSessionLocks(codexHome, meta.ID); err != nil {
		return nil, err
	}

	statePath := filepath.Join(codexHome, "state_5.sqlite")
	stateDB, err := openSQLite(statePath)
	if err != nil {
		return nil, fmt.Errorf("opening state_5.sqlite: %w", err)
	}
	defer stateDB.Close()
	_, _ = stateDB.Exec(`PRAGMA wal_checkpoint(PASSIVE)`)

	family, err := collectThreadFamily(stateDB, meta.ID)
	if err != nil {
		return nil, fmt.Errorf("collecting subagent family: %w", err)
	}

	inClause, inArgs := buildInClause(family)

	manifest := &BundleManifest{
		Version:         1,
		Meta:            *meta,
		ThreadIDs:       family,
		RolloutRelPaths: make(map[string]string),
		StateSlices:     make(map[string]*providers.TableSlice),
		HistorySlices:   make(map[string]*providers.TableSlice),
		GoalsSlices:     make(map[string]*providers.TableSlice),
		MemoriesSlices:  make(map[string]*providers.TableSlice),
	}

	// 1. Export from state_5.sqlite
	if s, err := providers.ExportTableSlice(stateDB, "threads", "id IN "+inClause, inArgs...); err != nil {
		return nil, err
	} else if s != nil {
		manifest.StateSlices["threads"] = s
	}

	edgeArgs := append(append([]any{}, inArgs...), inArgs...)
	if s, err := providers.ExportTableSlice(stateDB, "thread_spawn_edges", "parent_thread_id IN "+inClause+" OR child_thread_id IN "+inClause, edgeArgs...); err != nil {
		return nil, err
	} else if s != nil {
		manifest.StateSlices["thread_spawn_edges"] = s
	}

	for _, tbl := range []string{"thread_dynamic_tools", "thread_attachments"} {
		if s, err := providers.ExportTableSlice(stateDB, tbl, "thread_id IN "+inClause, inArgs...); err != nil {
			return nil, err
		} else if s != nil {
			manifest.StateSlices[tbl] = s
		}
	}

	// 2. Export from thread_history_1.sqlite if present
	histPath := filepath.Join(codexHome, "thread_history_1.sqlite")
	if _, err := os.Stat(histPath); err == nil {
		histDB, err := openSQLite(histPath)
		if err != nil {
			return nil, fmt.Errorf("opening thread_history_1.sqlite: %w", err)
		}
		defer histDB.Close()
		_, _ = histDB.Exec(`PRAGMA wal_checkpoint(PASSIVE)`)

		if s, err := providers.ExportTableSlice(histDB, "_sqlx_migrations", ""); err == nil && s != nil {
			manifest.HistorySlices["_sqlx_migrations"] = s
		}
		for _, tbl := range []string{"thread_turns", "thread_items", "thread_history_projection_state", "thread_realtime_items"} {
			if s, err := providers.ExportTableSlice(histDB, tbl, "thread_id IN "+inClause, inArgs...); err != nil {
				return nil, err
			} else if s != nil {
				manifest.HistorySlices[tbl] = s
			}
		}
	}

	// 3. Export from goals_1.sqlite & memories_1.sqlite if present
	goalsPath := filepath.Join(codexHome, "goals_1.sqlite")
	if _, err := os.Stat(goalsPath); err == nil {
		if gDB, err := openSQLite(goalsPath); err == nil {
			defer gDB.Close()
			if s, err := providers.ExportTableSlice(gDB, "thread_goals", "thread_id IN "+inClause, inArgs...); err == nil && s != nil {
				manifest.GoalsSlices["thread_goals"] = s
			}
		}
	}
	memPath := filepath.Join(codexHome, "memories_1.sqlite")
	if _, err := os.Stat(memPath); err == nil {
		if mDB, err := openSQLite(memPath); err == nil {
			defer mDB.Close()
			if s, err := providers.ExportTableSlice(mDB, "stage1_outputs", "thread_id IN "+inClause, inArgs...); err == nil && s != nil {
				manifest.MemoriesSlices["stage1_outputs"] = s
			}
		}
	}

	// 4. Stage rollout files and manifest.json
	stageDir, err := os.MkdirTemp("", "async-codex-export-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stageDir)

	if threadsSlice := manifest.StateSlices["threads"]; threadsSlice != nil {
		for _, row := range threadsSlice.Rows {
			tid := row["id"].Str
			rPath := row["rollout_path"].Str
			if tid == "" || rPath == "" {
				continue
			}
			relPath := computeRolloutRelPath(codexHome, rPath)
			manifest.RolloutRelPaths[tid] = relPath

			if st, err := os.Stat(rPath); err == nil && st.Mode().IsRegular() {
				dstRollout := filepath.Join(stageDir, "rollouts", filepath.FromSlash(relPath))
				if err := archive.AtomicInstallFile(rPath, dstRollout, 0o644); err != nil {
					return nil, fmt.Errorf("staging rollout file %s: %w", rPath, err)
				}
			}
		}
	}

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(stageDir, "manifest.json"), manifestBytes, 0o644); err != nil {
		return nil, err
	}

	if err := archive.PackDirTarGz(stageDir, w); err != nil {
		return nil, fmt.Errorf("packing codex bundle: %w", err)
	}
	return meta, nil
}

// ImportBundle unpacks a Codex bundle and surgically merges its rows and rollout files into codexHome.
func ImportBundle(codexHome string, r io.Reader, targetWorkspace string, force bool) (*providers.SessionMeta, error) {
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		return nil, fmt.Errorf("creating codex home %s: %w", codexHome, err)
	}

	stageDir, cleanup, err := archive.UnpackTarGzToStaged(r, codexHome)
	if err != nil {
		return nil, fmt.Errorf("unpacking codex bundle: %w", err)
	}
	defer cleanup()

	manifestBytes, err := os.ReadFile(filepath.Join(stageDir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("reading bundle manifest.json: %w", err)
	}
	var manifest BundleManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("parsing bundle manifest.json: %w", err)
	}

	// Verify no target threads are locked
	for _, tid := range manifest.ThreadIDs {
		if err := lock.CheckAvailable("codex", tid, lock.CodexLockPath(codexHome, tid)); err != nil {
			return nil, err
		}
	}

	releaseRootLock, err := lock.AcquireExclusive("codex", manifest.Meta.ID, lock.CodexLockPath(codexHome, manifest.Meta.ID))
	if err != nil {
		return nil, err
	}
	defer releaseRootLock()

	// Check conflict / stale overwrite protection
	if existing, err := FindSession(codexHome, manifest.Meta.ID, ""); err == nil && existing != nil {
		if !force {
			if existing.UpdatedAt > manifest.Meta.UpdatedAt ||
				(existing.UpdatedAt == manifest.Meta.UpdatedAt && existing.StepCount > manifest.Meta.StepCount) {
				return nil, &providers.StaleOverwriteError{
					Provider:  "codex",
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

	// Rewrite and install rollout files
	for tid, relSlash := range manifest.RolloutRelPaths {
		stagedRollout := filepath.Join(stageDir, "rollouts", filepath.FromSlash(relSlash))
		if _, err := os.Stat(stagedRollout); err != nil {
			continue
		}
		if cleanTargetWS != "" {
			if err := rewriteRolloutFileHeader(stagedRollout, cleanTargetWS); err != nil {
				return nil, fmt.Errorf("rewriting rollout header for thread %s: %w", tid, err)
			}
		}
		finalRollout := filepath.Join(codexHome, filepath.FromSlash(relSlash))
		if err := archive.AtomicInstallFile(stagedRollout, finalRollout, 0o600); err != nil {
			return nil, fmt.Errorf("installing rollout file %s: %w", finalRollout, err)
		}
	}

	// Rewrite threads rows (rollout_path and cwd)
	if threadsSlice := manifest.StateSlices["threads"]; threadsSlice != nil {
		for i, row := range threadsSlice.Rows {
			tid := row["id"].Str
			if relSlash, ok := manifest.RolloutRelPaths[tid]; ok && relSlash != "" {
				newAbsRollout := filepath.Join(codexHome, filepath.FromSlash(relSlash))
				row["rollout_path"] = providers.FromSQL(newAbsRollout)
			}
			if cleanTargetWS != "" {
				row["cwd"] = providers.FromSQL(cleanTargetWS)
			}
			// Nullify local-only foreign keys to prevent FK errors on destination
			row["project_id"] = providers.FromSQL(nil)
			row["thread_section_id"] = providers.FromSQL(nil)
			threadsSlice.Rows[i] = row
		}
	}

	// Merge into state_5.sqlite
	statePath := filepath.Join(codexHome, "state_5.sqlite")
	if err := mergeIntoSQLite(statePath, state5SchemaDDL, manifest.StateSlices, []string{
		"threads",
		"thread_spawn_edges",
		"thread_dynamic_tools",
		"thread_attachments",
	}); err != nil {
		return nil, fmt.Errorf("merging state_5.sqlite: %w", err)
	}

	// Merge into thread_history_1.sqlite
	if len(manifest.HistorySlices) > 0 {
		histPath := filepath.Join(codexHome, "thread_history_1.sqlite")
		if err := mergeIntoSQLite(histPath, history1SchemaDDL, manifest.HistorySlices, []string{
			"_sqlx_migrations",
			"thread_turns",
			"thread_items",
			"thread_history_projection_state",
			"thread_realtime_items",
		}); err != nil {
			return nil, fmt.Errorf("merging thread_history_1.sqlite: %w", err)
		}
	}

	// Merge optional goals and memories
	if len(manifest.GoalsSlices) > 0 {
		goalsPath := filepath.Join(codexHome, "goals_1.sqlite")
		_ = mergeIntoSQLite(goalsPath, "", manifest.GoalsSlices, []string{"thread_goals"})
	}
	if len(manifest.MemoriesSlices) > 0 {
		memPath := filepath.Join(codexHome, "memories_1.sqlite")
		_ = mergeIntoSQLite(memPath, "", manifest.MemoriesSlices, []string{"stage1_outputs"})
	}

	_ = EnsurePersistenceEnabled(codexHome)

	if cleanTargetWS != "" {
		manifest.Meta.Workspace = cleanTargetWS
	}
	return &manifest.Meta, nil
}

// EnsurePersistenceEnabled ensures ~/.codex/config.toml does not have persistence = "none"
// so that resumed sessions in containers record turns to disk.
func EnsurePersistenceEnabled(codexHome string) error {
	cfgPath := filepath.Join(codexHome, "config.toml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil
	}
	content := string(data)
	if !strings.Contains(content, `persistence = "none"`) && !strings.Contains(content, `persistence = 'none'`) {
		return nil
	}
	updated := strings.ReplaceAll(content, `persistence = "none"`, `persistence = "save-all"`)
	updated = strings.ReplaceAll(updated, `persistence = 'none'`, `persistence = "save-all"`)
	tmp := cfgPath + ".async-tmp"
	if err := os.WriteFile(tmp, []byte(updated), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, cfgPath)
}

func mergeIntoSQLite(dbPath, bootstrapDDL string, slices map[string]*providers.TableSlice, order []string) error {
	db, err := openSQLite(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if bootstrapDDL != "" {
		if _, err := db.Exec(bootstrapDDL); err != nil {
			return fmt.Errorf("executing bootstrap DDL: %w", err)
		}
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback()
	}()

	for _, name := range order {
		slice, ok := slices[name]
		if !ok || slice == nil {
			continue
		}
		if err := providers.ImportTableSlice(db, tx, slice); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, _ = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return nil
}

func rewriteRolloutFileHeader(filePath, newCwd string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	reader := bufio.NewReader(f)
	firstLine, err := reader.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		_ = f.Close()
		return err
	}
	rest, err := io.ReadAll(reader)
	_ = f.Close()
	if err != nil {
		return err
	}

	rewrittenFirst, err := pathmap.RewriteRolloutFirstLine(firstLine, newCwd)
	if err != nil {
		return err
	}

	var out bytes.Buffer
	out.Write(rewrittenFirst)
	out.WriteByte('\n')
	out.Write(rest)
	return os.WriteFile(filePath, out.Bytes(), 0o600)
}

func computeRolloutRelPath(codexHome, rolloutPath string) string {
	cleanHome := filepath.Clean(codexHome)
	cleanRollout := filepath.Clean(rolloutPath)
	if rel, err := filepath.Rel(cleanHome, cleanRollout); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	if idx := strings.Index(filepath.ToSlash(cleanRollout), "/sessions/"); idx >= 0 {
		return filepath.ToSlash(cleanRollout)[idx+1:]
	}
	return filepath.ToSlash(filepath.Join("sessions", filepath.Base(cleanRollout)))
}

func collectThreadFamily(db *sql.DB, rootID string) ([]string, error) {
	visited := map[string]bool{rootID: true}
	queue := []string{rootID}
	result := []string{rootID}

	hasEdges, _ := providers.TableExists(db, "thread_spawn_edges")
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		if hasEdges {
			rows, err := db.Query(`SELECT child_thread_id FROM thread_spawn_edges WHERE parent_thread_id = ? ORDER BY child_thread_id`, curr)
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

		// Also check source JSON column in threads for parent_thread_id references
		pattern := fmt.Sprintf(`%%"parent_thread_id":"%s"%%`, curr)
		rows2, err := db.Query(`SELECT id FROM threads WHERE source LIKE ? ORDER BY id`, pattern)
		if err == nil {
			for rows2.Next() {
				var child string
				if err := rows2.Scan(&child); err == nil && child != "" && !visited[child] {
					visited[child] = true
					queue = append(queue, child)
					result = append(result, child)
				}
			}
			rows2.Close()
		}
	}
	return result, nil
}

func countCodexSteps(histDB *sql.DB, threadID, rolloutPath string) int {
	if histDB != nil {
		var count int
		if err := histDB.QueryRow(`SELECT COUNT(*) FROM thread_items WHERE thread_id = ?`, threadID).Scan(&count); err == nil && count > 0 {
			return count
		}
		if err := histDB.QueryRow(`SELECT COUNT(*) FROM thread_turns WHERE thread_id = ?`, threadID).Scan(&count); err == nil && count > 0 {
			return count
		}
	}
	if rolloutPath != "" {
		if f, err := os.Open(rolloutPath); err == nil {
			defer f.Close()
			scanner := bufio.NewScanner(f)
			buf := make([]byte, 64*1024)
			scanner.Buffer(buf, 4*1024*1024)
			lines := 0
			for scanner.Scan() {
				lines++
			}
			if lines > 1 {
				return lines - 1
			}
		}
	}
	return 0
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
