package ssh

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
)

// Runner abstracts remote command execution over SSH.
type Runner interface {
	RunRPC(args []string, stdin io.Reader, stdout io.Writer) error
	Deploy(localBinPath string) (string, error)
	TargetHost() string
}

// Client executes remote commands over the native system ssh binary, respecting ~/.ssh/config.
type Client struct {
	Host       string
	RemoteBin  string
	SSHCommand string
}

// NewClient creates a new SSH Client for target host and remote binary path.
func NewClient(host, remoteBin string) *Client {
	if remoteBin == "" {
		remoteBin = "~/.local/bin/async"
	}
	return &Client{
		Host:       host,
		RemoteBin:  remoteBin,
		SSHCommand: "ssh",
	}
}

// TargetHost returns the configured remote host name.
func (c *Client) TargetHost() string {
	return c.Host
}

// RunRPC invokes `<RemoteBin> _rpc <args...>` on the remote host over SSH stdio.
func (c *Client) RunRPC(args []string, stdin io.Reader, stdout io.Writer) error {
	remoteCmd := buildRemoteCommand(c.RemoteBin, append([]string{"_rpc"}, args...))
	cmd := exec.Command(c.SSHCommand, c.Host, remoteCmd)
	cmd.Stdin = stdin
	cmd.Stdout = stdout

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("remote (%s): %s", c.Host, errMsg)
		}
		return fmt.Errorf("ssh %s failed: %w", c.Host, err)
	}
	return nil
}

// Deploy streams localBinPath to c.RemoteBin on the target host atomically and returns the remote version output.
func (c *Client) Deploy(localBinPath string) (string, error) {
	f, err := os.Open(localBinPath)
	if err != nil {
		return "", fmt.Errorf("opening local binary %s: %w", localBinPath, err)
	}
	defer f.Close()

	remoteBinExpr := expandRemoteTildeExpr(c.RemoteBin)
	remoteDirExpr := path.Dir(remoteBinExpr)

	script := fmt.Sprintf(
		"set -eu; mkdir -p %s; tmp=%s.tmp.$$; cat > \"$tmp\"; chmod 0755 \"$tmp\"; mv -f \"$tmp\" %s; %s --version",
		remoteDirExpr, remoteBinExpr, remoteBinExpr, remoteBinExpr,
	)

	cmd := exec.Command(c.SSHCommand, c.Host, script)
	cmd.Stdin = f

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return "", fmt.Errorf("deploy to %s failed: %s", c.Host, errMsg)
		}
		return "", fmt.Errorf("deploy to %s failed: %w", c.Host, err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func buildRemoteCommand(remoteBin string, args []string) string {
	var parts []string
	parts = append(parts, expandRemoteTildeExpr(remoteBin))
	for _, a := range args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

func expandRemoteTildeExpr(p string) string {
	if p == "~" {
		return "\"$HOME\""
	}
	if strings.HasPrefix(p, "~/") {
		rest := p[2:]
		return "\"$HOME\"/" + shellQuote(rest)
	}
	return shellQuote(p)
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
