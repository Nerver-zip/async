package rpc

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Nerver-zip/async/internal/config"
)

type loopbackRunner struct {
	cfg *config.Config
}

func (l *loopbackRunner) RunRPC(args []string, stdin io.Reader, stdout io.Writer) error {
	return HandleServer(l.cfg, args, stdin, stdout)
}

func (l *loopbackRunner) Deploy(localBinPath string) (string, error) {
	return "async dev", nil
}

func (l *loopbackRunner) TargetHost() string {
	return "loopback"
}

func TestRPCResolvePathAndAmbiguity(t *testing.T) {
	base := t.TempDir()
	p1 := filepath.Join(base, "myproj")
	if err := os.MkdirAll(p1, 0o755); err != nil {
		t.Fatal(err)
	}

	runner := &loopbackRunner{cfg: config.Default()}

	resp, err := RemoteResolvePath(runner, base, "myproj")
	if err != nil {
		t.Fatalf("RemoteResolvePath failed: %v", err)
	}
	if !resp.Exists || resp.Path != p1 {
		t.Errorf("unexpected response: %+v", resp)
	}

	// Create duplicate directory to trigger ambiguity
	p2 := filepath.Join(base, "nested", "myproj")
	if err := os.MkdirAll(p2, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoteResolvePath(runner, base, "myproj"); err == nil || !strings.Contains(err.Error(), "ambiguous target directory") {
		t.Fatalf("expected ambiguity error from RemoteResolvePath, got: %v", err)
	}
}

func TestRPCDumpAndImportEmptyCheck(t *testing.T) {
	tmp := t.TempDir()
	cfg := config.Default()
	cfg.CodexHome = filepath.Join(tmp, "codex")
	cfg.AGYHome = filepath.Join(tmp, "agy")
	runner := &loopbackRunner{cfg: cfg}

	list, err := RemoteList(runner, "codex", "", true)
	if err != nil {
		t.Fatalf("RemoteList on empty codex home failed: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(list))
	}

	var buf bytes.Buffer
	if err := RemoteDump(runner, "codex", "nonexistent", "", &buf); err == nil {
		t.Fatalf("expected error dumping nonexistent session")
	}
}
