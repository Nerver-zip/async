package pathmap

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveWorkspaceDir_ExactSingleMatch(t *testing.T) {
	base := t.TempDir()
	proj := filepath.Join(base, "my-project")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	got, exists, err := ResolveWorkspaceDir(base, "my-project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Errorf("exists = false, want true")
	}
	if got != proj {
		t.Errorf("got %q, want %q", got, proj)
	}
}

func TestResolveWorkspaceDir_MissingDirectory(t *testing.T) {
	base := t.TempDir()
	got, exists, err := ResolveWorkspaceDir(base, "new-project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Errorf("exists = true, want false")
	}
	want := filepath.Join(base, "new-project")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveWorkspaceDir_AmbiguityCollisionAborts(t *testing.T) {
	base := t.TempDir()
	c1 := filepath.Join(base, "my-project")
	c2 := filepath.Join(base, "archive", "my-project")
	if err := os.MkdirAll(c1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(c2, 0o755); err != nil {
		t.Fatal(err)
	}

	_, _, err := ResolveWorkspaceDir(base, "my-project")
	if err == nil {
		t.Fatalf("expected AmbiguousWorkspaceError, got nil")
	}
	if !IsAmbiguousError(err) {
		t.Fatalf("expected IsAmbiguousError == true, got %T: %v", err, err)
	}
	msg := err.Error()
	if !strings.Contains(msg, c1) || !strings.Contains(msg, c2) || !strings.Contains(msg, "--target-path") {
		t.Errorf("error message missing candidate paths or guidance: %s", msg)
	}
}

func TestRewriteRolloutFirstLine(t *testing.T) {
	raw := `{"timestamp":"2026-10-10T21:54:29.298Z","ordinal":0,"type":"session_meta","payload":{"id":"01a127cf-7325-7a93-a4fc-63d244f874b5","cwd":"/home/user/projects/sample-apps","runtime_workspace_roots":["/home/user/projects/sample-apps"],"source":"vscode","history_mode":"paginated"}}`
	rewritten, err := RewriteRolloutFirstLine([]byte(raw), "/home/user/workspace/sample-apps")
	if err != nil {
		t.Fatalf("RewriteRolloutFirstLine failed: %v", err)
	}

	var parsed struct {
		Type    string `json:"type"`
		Payload struct {
			ID                    string   `json:"id"`
			Cwd                   string   `json:"cwd"`
			RuntimeWorkspaceRoots []string `json:"runtime_workspace_roots"`
			HistoryMode           string   `json:"history_mode"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(rewritten, &parsed); err != nil {
		t.Fatalf("unmarshaling rewritten JSON: %v", err)
	}
	if parsed.Payload.Cwd != "/home/user/workspace/sample-apps" {
		t.Errorf("cwd = %q, want /home/user/workspace/sample-apps", parsed.Payload.Cwd)
	}
	if len(parsed.Payload.RuntimeWorkspaceRoots) != 1 || parsed.Payload.RuntimeWorkspaceRoots[0] != "/home/user/workspace/sample-apps" {
		t.Errorf("runtime_workspace_roots = %v, want [/home/user/workspace/sample-apps]", parsed.Payload.RuntimeWorkspaceRoots)
	}
	if parsed.Payload.ID != "01a127cf-7325-7a93-a4fc-63d244f874b5" {
		t.Errorf("id was altered: %q", parsed.Payload.ID)
	}
}

func TestRewriteProtobufURIs(t *testing.T) {
	// Sample prefix from AGY trajectory_metadata_blob
	sample := []byte("\n\xb2\x01\n&file:///home/user/projects/sample-apps\x12&file:///home/user/projects/sample-apps\x1a=\n\x11sample/sample-app\x12(https://github.com/sample/sample-app.git\"!feat/board-first-review-workspace\x12\x0c\x08\xa6\xdd\xc3\xd6\x06\x10\xd3\xa4\xe6\xc1\x02\x1a$56016b76-6ab0-4e6b-9a6d-4aa3816c6c992$abc28ad7-564c-44e6-abe8-6e9f638967b3:&file:///home/user/projects/sample-apps")
	oldURI := "file:///home/user/projects/sample-apps"
	newURI := "file:///home/user/workspace/sample-apps"

	updated := RewriteProtobufURIs(sample, oldURI, newURI)
	if bytes.Contains(updated, []byte(oldURI)) {
		t.Fatalf("updated protobuf still contains old URI")
	}
	if got := bytes.Count(updated, []byte(newURI)); got != 3 {
		t.Fatalf("expected 3 occurrences of newURI, got %d", got)
	}

	// Round-trip back to oldURI and verify byte-for-byte equality with original sample!
	roundTrip := RewriteProtobufURIs(updated, newURI, oldURI)
	if !bytes.Equal(roundTrip, sample) {
		t.Fatalf("round-trip protobuf does not match original byte-for-byte!\ngot:  %x\nwant: %x", roundTrip, sample)
	}
}
