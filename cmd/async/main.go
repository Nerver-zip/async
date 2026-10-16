package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"

	"github.com/Nerver-zip/async/internal/config"
	"github.com/Nerver-zip/async/internal/pathmap"
	"github.com/Nerver-zip/async/internal/providers"
	"github.com/Nerver-zip/async/internal/providers/agy"
	"github.com/Nerver-zip/async/internal/providers/codex"
	"github.com/Nerver-zip/async/internal/rpc"
	"github.com/Nerver-zip/async/internal/transport/ssh"
)

// Version is the semantic version of the async binary (overridden via -ldflags "-X main.Version=...").
var Version = "0.1.0"

var uuidLikeFlagRe = regexp.MustCompile(`^--([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|[0-9a-fA-F]{8,})$`)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stdout)
		return nil
	}

	cfg, err := config.Load("")
	if err != nil {
		return err
	}

	first := args[0]
	switch first {
	case "-h", "--help", "help":
		printUsage(stdout)
		return nil
	case "-v", "--version", "version":
		fmt.Fprintf(stdout, "async %s\n", Version)
		return nil
	case "_rpc":
		return rpc.HandleServer(cfg, args[1:], stdin, stdout)
	case "deploy":
		return runDeploy(cfg, args[1:], stdout)
	case "status":
		return runStatus(cfg, args[1:], stdout, nil)
	case "codex", "agy":
		if len(args) < 2 {
			return fmt.Errorf("missing operation for provider %q (expected push, pull, or ls)", first)
		}
		op := args[1]
		switch op {
		case "push":
			return runPush(cfg, first, args[2:], stdout, nil)
		case "pull":
			return runPull(cfg, first, args[2:], stdout, nil)
		case "ls", "list":
			return runList(cfg, first, args[2:], stdout, nil)
		default:
			return fmt.Errorf("unknown operation %q for provider %q (expected push, pull, or ls)", op, first)
		}
	default:
		return fmt.Errorf("unknown command or provider %q; run 'async --help' for usage", first)
	}
}

type parsedFlags struct {
	id          string
	targetPath  string
	cwd         string
	force       bool
	dryRun      bool
	all         bool
	jsonOutput  bool
	positionals []string
}

func parseFlexibleFlags(args []string) (*parsedFlags, error) {
	pf := &parsedFlags{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if m := uuidLikeFlagRe.FindStringSubmatch(arg); m != nil {
			pf.id = m[1]
			continue
		}
		switch {
		case arg == "--id" || arg == "-i":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("flag %s requires a value", arg)
			}
			i++
			pf.id = args[i]
		case strings.HasPrefix(arg, "--id="):
			pf.id = strings.TrimPrefix(arg, "--id=")
		case arg == "--target-path":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("flag --target-path requires a value")
			}
			i++
			pf.targetPath = args[i]
		case strings.HasPrefix(arg, "--target-path="):
			pf.targetPath = strings.TrimPrefix(arg, "--target-path=")
		case arg == "--cwd":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("flag --cwd requires a value")
			}
			i++
			pf.cwd = args[i]
		case strings.HasPrefix(arg, "--cwd="):
			pf.cwd = strings.TrimPrefix(arg, "--cwd=")
		case arg == "--force" || arg == "-f":
			pf.force = true
		case arg == "--dry-run":
			pf.dryRun = true
		case arg == "--all" || arg == "-a":
			pf.all = true
		case arg == "--json":
			pf.jsonOutput = true
		case strings.HasPrefix(arg, "-"):
			return nil, fmt.Errorf("unknown flag %q", arg)
		default:
			pf.positionals = append(pf.positionals, arg)
		}
	}
	return pf, nil
}

func resolveWorkingDir(flagCwd string) (string, error) {
	if flagCwd != "" {
		return filepath.Clean(pathmap.ExpandPath(flagCwd)), nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("determining working directory: %w", err)
	}
	return filepath.Clean(wd), nil
}

func runDeploy(cfg *config.Config, args []string, stdout io.Writer) error {
	pf, err := parseFlexibleFlags(args)
	if err != nil {
		return err
	}
	if len(pf.positionals) < 1 {
		return fmt.Errorf("usage: async deploy <targethost>")
	}
	host := pf.positionals[0]

	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating current binary: %w", err)
	}
	exePath, _ = filepath.EvalSymlinks(exePath)

	remoteBin := cfg.HostBinary(host)
	client := ssh.NewClient(host, remoteBin)
	fmt.Fprintf(stdout, "Deploying %s to %s:%s ...\n", exePath, host, remoteBin)
	remoteVer, err := client.Deploy(exePath)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Successfully deployed to %s (%s)\n", host, remoteVer)
	return nil
}

func runList(cfg *config.Config, provider string, args []string, stdout io.Writer, customRunner ssh.Runner) error {
	pf, err := parseFlexibleFlags(args)
	if err != nil {
		return err
	}
	cwd, err := resolveWorkingDir(pf.cwd)
	if err != nil {
		return err
	}

	var sessions []providers.SessionMeta
	if len(pf.positionals) == 0 {
		ws := cwd
		if pf.all {
			ws = ""
		}
		sessions, err = listLocalSessions(cfg, provider, ws, pf.all)
		if err != nil {
			return err
		}
	} else {
		host := pf.positionals[0]
		runner := customRunner
		if runner == nil {
			runner = ssh.NewClient(host, cfg.HostBinary(host))
		}
		remoteWS := ""
		if !pf.all {
			if pf.targetPath != "" {
				remoteWS = pf.targetPath
			} else {
				projName := pathmap.ProjectName(cwd)
				resolved, err := rpc.RemoteResolvePath(runner, cfg.HostPrefix(host), projName)
				if err != nil {
					return err
				}
				remoteWS = resolved.Path
			}
		}
		sessions, err = rpc.RemoteList(runner, provider, remoteWS, pf.all)
		if err != nil {
			return err
		}
	}

	if pf.jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(sessions)
	}

	if len(sessions) == 0 {
		fmt.Fprintf(stdout, "No %s sessions found.\n", provider)
		return nil
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tUPDATED\tSTEPS\tSUBS\tLOCKED\tWORKSPACE\tTITLE")
	for _, s := range sessions {
		title := truncateText(s.Title, 48)
		if title == "" {
			title = truncateText(s.Preview, 48)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%v\t%s\t%s\n",
			s.ID, s.UpdatedAtISO, s.StepCount, s.SubagentCount, s.Locked, s.Workspace, title)
	}
	return tw.Flush()
}

func runPush(cfg *config.Config, provider string, args []string, stdout io.Writer, customRunner ssh.Runner) error {
	pf, err := parseFlexibleFlags(args)
	if err != nil {
		return err
	}
	if len(pf.positionals) < 1 {
		return fmt.Errorf("usage: async %s push [--id <uuid> | --<uuid>] [--target-path <path>] [--force] <targethost>", provider)
	}
	host := pf.positionals[0]
	cwd, err := resolveWorkingDir(pf.cwd)
	if err != nil {
		return err
	}

	runner := customRunner
	if runner == nil {
		runner = ssh.NewClient(host, cfg.HostBinary(host))
	}

	// 1. Locate local session
	localMeta, err := findLocalSession(cfg, provider, pf.id, cwd)
	if err != nil {
		return err
	}
	if err := checkLocalLocks(cfg, provider, localMeta.ID); err != nil {
		return err
	}

	// 2. Resolve destination workspace path on remote host
	remoteWS := pf.targetPath
	if remoteWS == "" {
		sourceWS := localMeta.Workspace
		if sourceWS == "" {
			sourceWS = cwd
		}
		projName := pathmap.ProjectName(sourceWS)
		resolved, err := rpc.RemoteResolvePath(runner, cfg.HostPrefix(host), projName)
		if err != nil {
			return err
		}
		remoteWS = resolved.Path
	}

	// 3. Check remote locks and conflict status
	if err := rpc.RemoteLockCheck(runner, provider, localMeta.ID); err != nil {
		return err
	}

	if remoteExisting, err := rpc.RemoteFind(runner, provider, localMeta.ID, ""); err == nil && remoteExisting != nil {
		if remoteExisting.Locked {
			return fmt.Errorf("remote %s session %s is currently locked by a running agent on %s", provider, localMeta.ID, host)
		}
		if !pf.force {
			if remoteExisting.UpdatedAt > localMeta.UpdatedAt ||
				(remoteExisting.UpdatedAt == localMeta.UpdatedAt && remoteExisting.StepCount > localMeta.StepCount) {
				return &providers.StaleOverwriteError{
					Provider:  provider,
					SessionID: localMeta.ID,
					SrcMeta:   *localMeta,
					DstMeta:   *remoteExisting,
				}
			}
		}
	}

	if pf.dryRun {
		fmt.Fprintf(stdout, "[dry-run] Would push %s session %s (%q, %d steps) from %s -> %s:%s\n",
			provider, localMeta.ID, truncateText(localMeta.Title, 40), localMeta.StepCount, localMeta.Workspace, host, remoteWS)
		return nil
	}

	// 4. Export surgical bundle and stream to remote import
	var bundle bytes.Buffer
	if _, err := exportLocalBundle(cfg, provider, localMeta.ID, cwd, &bundle); err != nil {
		return err
	}

	importedMeta, err := rpc.RemoteImport(runner, provider, &bundle, remoteWS, pf.force)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Pushed %s session %s to %s\n", provider, importedMeta.ID, host)
	fmt.Fprintf(stdout, "  Title:      %s\n", truncateText(importedMeta.Title, 70))
	fmt.Fprintf(stdout, "  Steps:      %d (subagents: %d)\n", importedMeta.StepCount, importedMeta.SubagentCount)
	fmt.Fprintf(stdout, "  Workspace:  %s -> %s\n", localMeta.Workspace, importedMeta.Workspace)
	return nil
}

func runPull(cfg *config.Config, provider string, args []string, stdout io.Writer, customRunner ssh.Runner) error {
	pf, err := parseFlexibleFlags(args)
	if err != nil {
		return err
	}
	if len(pf.positionals) < 1 {
		return fmt.Errorf("usage: async %s pull [--id <uuid> | --<uuid>] [--target-path <path>] [--force] <targethost>", provider)
	}
	host := pf.positionals[0]
	cwd, err := resolveWorkingDir(pf.cwd)
	if err != nil {
		return err
	}

	runner := customRunner
	if runner == nil {
		runner = ssh.NewClient(host, cfg.HostBinary(host))
	}

	// 1. Resolve remote workspace path if --id is not explicitly provided
	remoteWS := pf.targetPath
	if pf.id == "" && remoteWS == "" {
		projName := pathmap.ProjectName(cwd)
		resolved, err := rpc.RemoteResolvePath(runner, cfg.HostPrefix(host), projName)
		if err != nil {
			return err
		}
		remoteWS = resolved.Path
	}

	// 2. Find remote session metadata
	remoteMeta, err := rpc.RemoteFind(runner, provider, pf.id, remoteWS)
	if err != nil {
		return err
	}
	if remoteMeta.Locked {
		return fmt.Errorf("remote %s session %s is currently locked by an active agent on %s", provider, remoteMeta.ID, host)
	}
	if err := rpc.RemoteLockCheck(runner, provider, remoteMeta.ID); err != nil {
		return err
	}

	// 3. Check local locks, existing metadata, and conflict status
	if err := checkLocalLocks(cfg, provider, remoteMeta.ID); err != nil {
		return err
	}
	localExisting, _ := findLocalSession(cfg, provider, remoteMeta.ID, "")
	if localExisting != nil {
		if !pf.force {
			if localExisting.UpdatedAt > remoteMeta.UpdatedAt ||
				(localExisting.UpdatedAt == remoteMeta.UpdatedAt && localExisting.StepCount > remoteMeta.StepCount) {
				return &providers.StaleOverwriteError{
					Provider:  provider,
					SessionID: remoteMeta.ID,
					SrcMeta:   *remoteMeta,
					DstMeta:   *localExisting,
				}
			}
		}
	}

	// 4. Determine local destination workspace path
	localTargetWS := cwd
	if pf.id != "" {
		if localExisting != nil && localExisting.Workspace != "" {
			localTargetWS = localExisting.Workspace
		} else if remoteMeta.Workspace != "" {
			remoteProj := pathmap.ProjectName(remoteMeta.Workspace)
			if pathmap.ProjectName(cwd) != remoteProj {
				resolvedLocal, _, err := pathmap.ResolveWorkspaceDir(cfg.LocalPrefix, remoteProj)
				if err != nil {
					return err
				}
				localTargetWS = resolvedLocal
			}
		}
	}

	if pf.dryRun {
		fmt.Fprintf(stdout, "[dry-run] Would pull %s session %s (%q, %d steps) from %s:%s -> %s\n",
			provider, remoteMeta.ID, truncateText(remoteMeta.Title, 40), remoteMeta.StepCount, host, remoteMeta.Workspace, localTargetWS)
		return nil
	}

	// 5. Dump bundle from remote and import locally
	var bundle bytes.Buffer
	if err := rpc.RemoteDump(runner, provider, remoteMeta.ID, "", &bundle); err != nil {
		return err
	}

	importedMeta, err := importLocalBundle(cfg, provider, &bundle, localTargetWS, pf.force)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Pulled %s session %s from %s\n", provider, importedMeta.ID, host)
	fmt.Fprintf(stdout, "  Title:      %s\n", truncateText(importedMeta.Title, 70))
	fmt.Fprintf(stdout, "  Steps:      %d (subagents: %d)\n", importedMeta.StepCount, importedMeta.SubagentCount)
	fmt.Fprintf(stdout, "  Workspace:  %s -> %s\n", remoteMeta.Workspace, importedMeta.Workspace)
	return nil
}

func runStatus(cfg *config.Config, args []string, stdout io.Writer, customRunner ssh.Runner) error {
	pf, err := parseFlexibleFlags(args)
	if err != nil {
		return err
	}
	cwd, err := resolveWorkingDir(pf.cwd)
	if err != nil {
		return err
	}

	var host string
	var runner ssh.Runner
	if len(pf.positionals) > 0 {
		host = pf.positionals[0]
		runner = customRunner
		if runner == nil {
			runner = ssh.NewClient(host, cfg.HostBinary(host))
		}
	}

	var remoteWS string
	if runner != nil {
		if pf.targetPath != "" {
			remoteWS = pf.targetPath
		} else {
			projName := pathmap.ProjectName(cwd)
			if resolved, err := rpc.RemoteResolvePath(runner, cfg.HostPrefix(host), projName); err == nil {
				remoteWS = resolved.Path
			} else if pathmap.IsAmbiguousError(err) {
				return err
			}
		}
	}

	fmt.Fprintf(stdout, "Workspace (local):  %s\n", cwd)
	if runner != nil {
		fmt.Fprintf(stdout, "Workspace (%s): %s\n", host, remoteWS)
	}
	fmt.Fprintln(stdout)

	for _, prov := range []string{"codex", "agy"} {
		fmt.Fprintf(stdout, "[%s]\n", prov)
		localMeta, _ := findLocalSession(cfg, prov, pf.id, cwd)
		var remoteMeta *providers.SessionMeta
		if runner != nil {
			if pf.id != "" {
				remoteMeta, _ = rpc.RemoteFind(runner, prov, pf.id, "")
			} else if localMeta != nil {
				// First check same ID on remote, otherwise latest in remoteWS
				remoteMeta, _ = rpc.RemoteFind(runner, prov, localMeta.ID, "")
				if remoteMeta == nil && remoteWS != "" {
					remoteMeta, _ = rpc.RemoteFind(runner, prov, "", remoteWS)
				}
			} else if remoteWS != "" {
				remoteMeta, _ = rpc.RemoteFind(runner, prov, "", remoteWS)
			}
		}

		if localMeta == nil {
			fmt.Fprintln(stdout, "  Local:  (no session)")
		} else {
			fmt.Fprintf(stdout, "  Local:  %s  updated=%s  steps=%d  locked=%v\n",
				localMeta.ID, localMeta.UpdatedAtISO, localMeta.StepCount, localMeta.Locked)
		}

		if runner != nil {
			if remoteMeta == nil {
				fmt.Fprintf(stdout, "  Remote: (no session on %s)\n", host)
			} else {
				fmt.Fprintf(stdout, "  Remote: %s  updated=%s  steps=%d  locked=%v\n",
					remoteMeta.ID, remoteMeta.UpdatedAtISO, remoteMeta.StepCount, remoteMeta.Locked)
			}
			fmt.Fprintf(stdout, "  Status: %s\n", computeDriftRecommendation(localMeta, remoteMeta))
		}
		fmt.Fprintln(stdout)
	}
	return nil
}

func computeDriftRecommendation(local, remote *providers.SessionMeta) string {
	switch {
	case local == nil && remote == nil:
		return "No sessions on either side"
	case local != nil && remote == nil:
		return "Local only (Push recommended)"
	case local == nil && remote != nil:
		return "Remote only (Pull recommended)"
	default:
		if local.Locked || remote.Locked {
			return "Session active/locked (wait for agent completion before syncing)"
		}
		if local.ID != remote.ID {
			if local.UpdatedAt > remote.UpdatedAt {
				return fmt.Sprintf("Different latest sessions; local (%s) is newer (Push recommended)", local.ID[:8])
			}
			return fmt.Sprintf("Different latest sessions; remote (%s) is newer (Pull recommended)", remote.ID[:8])
		}
		if local.UpdatedAt == remote.UpdatedAt && local.StepCount == remote.StepCount {
			return "In sync"
		}
		if local.UpdatedAt > remote.UpdatedAt || (local.UpdatedAt == remote.UpdatedAt && local.StepCount > remote.StepCount) {
			diff := local.StepCount - remote.StepCount
			return fmt.Sprintf("Local is ahead (step delta: %+d) -> Push recommended", diff)
		}
		diff := remote.StepCount - local.StepCount
		return fmt.Sprintf("Remote is ahead (step delta: %+d) -> Pull recommended", diff)
	}
}

func listLocalSessions(cfg *config.Config, provider, ws string, all bool) ([]providers.SessionMeta, error) {
	switch provider {
	case "codex":
		return codex.ListSessions(cfg.CodexHome, ws, all)
	case "agy":
		return agy.ListSessions(cfg.AGYHome, ws, all)
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}
}

func findLocalSession(cfg *config.Config, provider, id, ws string) (*providers.SessionMeta, error) {
	switch provider {
	case "codex":
		return codex.FindSession(cfg.CodexHome, id, ws)
	case "agy":
		return agy.FindSession(cfg.AGYHome, id, ws)
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}
}

func checkLocalLocks(cfg *config.Config, provider, id string) error {
	switch provider {
	case "codex":
		return codex.CheckSessionLocks(cfg.CodexHome, id)
	case "agy":
		return agy.CheckSessionLocks(cfg.AGYHome, id)
	default:
		return fmt.Errorf("unsupported provider %q", provider)
	}
}

func exportLocalBundle(cfg *config.Config, provider, id, ws string, w io.Writer) (*providers.SessionMeta, error) {
	switch provider {
	case "codex":
		return codex.ExportBundle(cfg.CodexHome, id, ws, w)
	case "agy":
		return agy.ExportBundle(cfg.AGYHome, id, ws, w)
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}
}

func importLocalBundle(cfg *config.Config, provider string, r io.Reader, targetWS string, force bool) (*providers.SessionMeta, error) {
	switch provider {
	case "codex":
		return codex.ImportBundle(cfg.CodexHome, r, targetWS, force)
	case "agy":
		return agy.ImportBundle(cfg.AGYHome, r, targetWS, force)
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}
}

func truncateText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `async (Agent Sync) — Synchronize Codex and Antigravity CLI sessions over SSH

Usage:
  async <codex|agy> push [flags] <targethost>
  async <codex|agy> pull [flags] <targethost>
  async <codex|agy> ls [targethost] [--all] [--json]
  async status [targethost]
  async deploy <targethost>

Flags:
  --id, -i <uuid>        Explicit conversation ID (or pass --<uuid> directly)
  --target-path <path>   Explicit remote workspace directory override
  --cwd <path>           Override local working directory
  --force, -f            Overwrite destination even if destination is newer
  --dry-run              Show planned action without modifying state
  --all, -a              List sessions across all workspaces
  --json                 Output session list as JSON
  --version, -v          Show version
  --help, -h             Show this help message
`)
}
