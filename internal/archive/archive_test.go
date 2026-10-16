package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackAndUnpackStaged(t *testing.T) {
	src := t.TempDir()
	nestedFile := filepath.Join(src, "brain", "abc", "plan.md")
	if err := os.MkdirAll(filepath.Dir(nestedFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nestedFile, []byte("# Test Plan\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := PackDirTarGz(src, &buf); err != nil {
		t.Fatalf("PackDirTarGz failed: %v", err)
	}

	destParent := t.TempDir()
	staged, cleanup, err := UnpackTarGzToStaged(&buf, destParent)
	if err != nil {
		t.Fatalf("UnpackTarGzToStaged failed: %v", err)
	}
	defer cleanup()

	gotBytes, err := os.ReadFile(filepath.Join(staged, "brain", "abc", "plan.md"))
	if err != nil {
		t.Fatalf("reading unpacked file: %v", err)
	}
	if string(gotBytes) != "# Test Plan\n" {
		t.Errorf("got %q, want %q", string(gotBytes), "# Test Plan\n")
	}

	finalDest := t.TempDir()
	if err := AtomicCopyTree(staged, finalDest); err != nil {
		t.Fatalf("AtomicCopyTree failed: %v", err)
	}
	finalBytes, err := os.ReadFile(filepath.Join(finalDest, "brain", "abc", "plan.md"))
	if err != nil || string(finalBytes) != "# Test Plan\n" {
		t.Fatalf("final file verification failed: %v", err)
	}

	cleanup()
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("expected staged dir to be removed after cleanup")
	}
}

func TestUnpackTarGz_RejectsPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	payload := []byte("malicious")
	hdr := &tar.Header{
		Name:     "../../escaped.txt",
		Mode:     0o644,
		Size:     int64(len(payload)),
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()

	dest := t.TempDir()
	err := UnpackTarGz(&buf, dest)
	if err == nil || !strings.Contains(err.Error(), "illegal path") {
		t.Fatalf("expected path traversal rejection, got: %v", err)
	}
}
