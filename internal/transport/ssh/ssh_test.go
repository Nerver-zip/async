package ssh

import (
	"strings"
	"testing"
)

func TestBuildRemoteCommand(t *testing.T) {
	got := buildRemoteCommand("~/.local/bin/async", []string{"_rpc", "find", "--provider", "codex", "--path", "/home/user/workspace/my project"})
	want := `"$HOME"/'.local/bin/async' '_rpc' 'find' '--provider' 'codex' '--path' '/home/user/workspace/my project'`
	if got != want {
		t.Errorf("got:  %s\nwant: %s", got, want)
	}
}

func TestShellQuoteEscapesSingleQuotes(t *testing.T) {
	got := shellQuote("it's a test")
	if !strings.Contains(got, `'\''`) {
		t.Errorf("expected escaped single quote, got: %s", got)
	}
}
