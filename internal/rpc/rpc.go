package rpc

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/Nerver-zip/async/internal/config"
	"github.com/Nerver-zip/async/internal/pathmap"
	"github.com/Nerver-zip/async/internal/providers"
	"github.com/Nerver-zip/async/internal/providers/agy"
	"github.com/Nerver-zip/async/internal/providers/codex"
	"github.com/Nerver-zip/async/internal/transport/ssh"
)

// ResolvePathResponse is returned by `_rpc resolve-path`.
type ResolvePathResponse struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

// LockCheckResponse is returned by `_rpc lock-check`.
type LockCheckResponse struct {
	Locked bool `json:"locked"`
}

// HandleServer executes an `_rpc <subcommand>` request on the remote machine,
// reading from stdin and writing the response to stdout.
func HandleServer(cfg *config.Config, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("missing _rpc subcommand")
	}
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "resolve-path":
		fs := flag.NewFlagSet("resolve-path", flag.ContinueOnError)
		prefix := fs.String("prefix", "", "base prefix")
		project := fs.String("project", "", "project folder name")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		resolved, exists, err := pathmap.ResolveWorkspaceDir(*prefix, *project)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(ResolvePathResponse{
			Path:   resolved,
			Exists: exists,
		})

	case "list":
		fs := flag.NewFlagSet("list", flag.ContinueOnError)
		provider := fs.String("provider", "", "provider (codex|agy)")
		wsPath := fs.String("path", "", "workspace path")
		all := fs.Bool("all", false, "all workspaces")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		list, err := listLocal(cfg, *provider, *wsPath, *all)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(list)

	case "find":
		fs := flag.NewFlagSet("find", flag.ContinueOnError)
		provider := fs.String("provider", "", "provider (codex|agy)")
		id := fs.String("id", "", "conversation id")
		wsPath := fs.String("path", "", "workspace path")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		meta, err := findLocal(cfg, *provider, *id, *wsPath)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(meta)

	case "lock-check":
		fs := flag.NewFlagSet("lock-check", flag.ContinueOnError)
		provider := fs.String("provider", "", "provider (codex|agy)")
		id := fs.String("id", "", "conversation id")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if err := checkLocksLocal(cfg, *provider, *id); err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(LockCheckResponse{Locked: false})

	case "dump":
		fs := flag.NewFlagSet("dump", flag.ContinueOnError)
		provider := fs.String("provider", "", "provider (codex|agy)")
		id := fs.String("id", "", "conversation id")
		wsPath := fs.String("path", "", "workspace path")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		_, err := exportLocal(cfg, *provider, *id, *wsPath, stdout)
		return err

	case "import":
		fs := flag.NewFlagSet("import", flag.ContinueOnError)
		provider := fs.String("provider", "", "provider (codex|agy)")
		targetPath := fs.String("target-path", "", "target workspace path")
		force := fs.Bool("force", false, "force overwrite")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		meta, err := importLocal(cfg, *provider, stdin, *targetPath, *force)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(meta)

	default:
		return fmt.Errorf("unknown _rpc subcommand %q", sub)
	}
}

func listLocal(cfg *config.Config, provider, wsPath string, all bool) ([]providers.SessionMeta, error) {
	switch provider {
	case "codex":
		return codex.ListSessions(cfg.CodexHome, wsPath, all)
	case "agy":
		return agy.ListSessions(cfg.AGYHome, wsPath, all)
	default:
		return nil, fmt.Errorf("unsupported provider %q (expected 'codex' or 'agy')", provider)
	}
}

func findLocal(cfg *config.Config, provider, id, wsPath string) (*providers.SessionMeta, error) {
	switch provider {
	case "codex":
		return codex.FindSession(cfg.CodexHome, id, wsPath)
	case "agy":
		return agy.FindSession(cfg.AGYHome, id, wsPath)
	default:
		return nil, fmt.Errorf("unsupported provider %q (expected 'codex' or 'agy')", provider)
	}
}

func checkLocksLocal(cfg *config.Config, provider, id string) error {
	switch provider {
	case "codex":
		return codex.CheckSessionLocks(cfg.CodexHome, id)
	case "agy":
		return agy.CheckSessionLocks(cfg.AGYHome, id)
	default:
		return fmt.Errorf("unsupported provider %q", provider)
	}
}

func exportLocal(cfg *config.Config, provider, id, wsPath string, w io.Writer) (*providers.SessionMeta, error) {
	switch provider {
	case "codex":
		return codex.ExportBundle(cfg.CodexHome, id, wsPath, w)
	case "agy":
		return agy.ExportBundle(cfg.AGYHome, id, wsPath, w)
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}
}

func importLocal(cfg *config.Config, provider string, r io.Reader, targetPath string, force bool) (*providers.SessionMeta, error) {
	switch provider {
	case "codex":
		return codex.ImportBundle(cfg.CodexHome, r, targetPath, force)
	case "agy":
		return agy.ImportBundle(cfg.AGYHome, r, targetPath, force)
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}
}

// RemoteResolvePath invokes `_rpc resolve-path` on the remote runner.
func RemoteResolvePath(runner ssh.Runner, basePrefix, projectName string) (*ResolvePathResponse, error) {
	var out bytes.Buffer
	args := []string{"resolve-path", "--prefix", basePrefix, "--project", projectName}
	if err := runner.RunRPC(args, nil, &out); err != nil {
		return nil, err
	}
	var resp ResolvePathResponse
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		return nil, fmt.Errorf("decoding remote resolve-path response: %w", err)
	}
	return &resp, nil
}

// RemoteList invokes `_rpc list` on the remote runner.
func RemoteList(runner ssh.Runner, provider, wsPath string, all bool) ([]providers.SessionMeta, error) {
	var out bytes.Buffer
	args := []string{"list", "--provider", provider}
	if wsPath != "" {
		args = append(args, "--path", wsPath)
	}
	if all {
		args = append(args, "--all")
	}
	if err := runner.RunRPC(args, nil, &out); err != nil {
		return nil, err
	}
	var list []providers.SessionMeta
	if err := json.Unmarshal(out.Bytes(), &list); err != nil {
		return nil, fmt.Errorf("decoding remote list response: %w", err)
	}
	return list, nil
}

// RemoteFind invokes `_rpc find` on the remote runner.
func RemoteFind(runner ssh.Runner, provider, id, wsPath string) (*providers.SessionMeta, error) {
	var out bytes.Buffer
	args := []string{"find", "--provider", provider}
	if id != "" {
		args = append(args, "--id", id)
	}
	if wsPath != "" {
		args = append(args, "--path", wsPath)
	}
	if err := runner.RunRPC(args, nil, &out); err != nil {
		if strings.Contains(err.Error(), "no "+provider+" session found") {
			return nil, &providers.NotFoundError{Provider: provider, ID: id, Workspace: wsPath}
		}
		return nil, err
	}
	var meta providers.SessionMeta
	if err := json.Unmarshal(out.Bytes(), &meta); err != nil {
		return nil, fmt.Errorf("decoding remote find response: %w", err)
	}
	return &meta, nil
}

// RemoteLockCheck invokes `_rpc lock-check` on the remote runner.
func RemoteLockCheck(runner ssh.Runner, provider, id string) error {
	var out bytes.Buffer
	args := []string{"lock-check", "--provider", provider, "--id", id}
	return runner.RunRPC(args, nil, &out)
}

// RemoteDump invokes `_rpc dump` on the remote runner and writes the tar.gz bundle to w.
func RemoteDump(runner ssh.Runner, provider, id, wsPath string, w io.Writer) error {
	args := []string{"dump", "--provider", provider}
	if id != "" {
		args = append(args, "--id", id)
	}
	if wsPath != "" {
		args = append(args, "--path", wsPath)
	}
	return runner.RunRPC(args, nil, w)
}

// RemoteImport invokes `_rpc import` on the remote runner, streaming bundleReader to stdin.
func RemoteImport(runner ssh.Runner, provider string, bundleReader io.Reader, targetPath string, force bool) (*providers.SessionMeta, error) {
	var out bytes.Buffer
	args := []string{"import", "--provider", provider}
	if targetPath != "" {
		args = append(args, "--target-path", targetPath)
	}
	if force {
		args = append(args, "--force")
	}
	if err := runner.RunRPC(args, bundleReader, &out); err != nil {
		return nil, err
	}
	var meta providers.SessionMeta
	if err := json.Unmarshal(out.Bytes(), &meta); err != nil {
		return nil, fmt.Errorf("decoding remote import response: %w", err)
	}
	return &meta, nil
}
