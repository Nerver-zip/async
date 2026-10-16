package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

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
var uuidRegex = regexp.MustCompile(`^[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}$`)

func isUUID(s string) bool {
	return uuidRegex.MatchString(s)
}

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
	case "rm", "delete":
		return runDeleteTopLevel(cfg, args[1:], stdin, stdout, stderr, nil)
	case "clean", "prune":
		return runCleanTopLevel(cfg, args[1:], stdin, stdout, stderr, nil)
	case "codex", "agy":
		if len(args) < 2 {
			return fmt.Errorf("missing operation for provider %q (expected push, pull, ls, rm, or clean)", first)
		}
		op := args[1]
		switch op {
		case "push":
			return runPush(cfg, first, args[2:], stdout, nil)
		case "pull":
			return runPull(cfg, first, args[2:], stdout, nil)
		case "ls", "list":
			return runList(cfg, first, args[2:], stdout, nil)
		case "rm", "delete":
			return runDelete(cfg, first, args[2:], stdin, stdout, stderr, nil)
		case "clean", "prune":
			return runClean(cfg, first, args[2:], stdin, stdout, stderr, nil)
		default:
			return fmt.Errorf("unknown operation %q for provider %q (expected push, pull, ls, rm, or clean)", op, first)
		}
	default:
		return fmt.Errorf("unknown command or provider %q; run 'async --help' for usage", first)
	}
}

type parsedFlags struct {
	id          string
	ids         []string
	targetPath  string
	cwd         string
	workspace   string
	olderThan   time.Duration
	provider    string
	host        string
	force       bool
	yes         bool
	dryRun      bool
	all         bool
	jsonOutput  bool
	positionals []string
}

func parseDurationWithDays(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if strings.HasSuffix(s, "d") {
		daysStr := strings.TrimSuffix(s, "d")
		days, err := strconv.Atoi(daysStr)
		if err != nil {
			return 0, fmt.Errorf("invalid duration days %q: %w", s, err)
		}
		if days < 0 {
			return 0, fmt.Errorf("duration days cannot be negative")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func confirmPrompt(stdin io.Reader, stdout io.Writer, prompt string) (bool, error) {
	fmt.Fprint(stdout, prompt)
	scanner := bufio.NewScanner(stdin)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return false, err
		}
		return false, nil
	}
	ans := strings.TrimSpace(strings.ToLower(scanner.Text()))
	return ans == "y" || ans == "yes", nil
}

func parseFlexibleFlags(args []string) (*parsedFlags, error) {
	pf := &parsedFlags{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if m := uuidLikeFlagRe.FindStringSubmatch(arg); m != nil {
			pf.id = m[1]
			pf.ids = append(pf.ids, m[1])
			continue
		}
		switch {
		case arg == "--id" || arg == "-i":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("flag %s requires a value", arg)
			}
			i++
			pf.id = args[i]
			pf.ids = append(pf.ids, args[i])
		case strings.HasPrefix(arg, "--id="):
			v := strings.TrimPrefix(arg, "--id=")
			pf.id = v
			pf.ids = append(pf.ids, v)
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
		case arg == "--workspace" || arg == "-w":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("flag %s requires a value", arg)
			}
			i++
			pf.workspace = args[i]
		case strings.HasPrefix(arg, "--workspace="):
			pf.workspace = strings.TrimPrefix(arg, "--workspace=")
		case arg == "--older-than":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("flag --older-than requires a value")
			}
			i++
			d, err := parseDurationWithDays(args[i])
			if err != nil {
				return nil, err
			}
			pf.olderThan = d
		case strings.HasPrefix(arg, "--older-than="):
			d, err := parseDurationWithDays(strings.TrimPrefix(arg, "--older-than="))
			if err != nil {
				return nil, err
			}
			pf.olderThan = d
		case arg == "--provider" || arg == "-p":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("flag %s requires a value", arg)
			}
			i++
			pf.provider = args[i]
		case strings.HasPrefix(arg, "--provider="):
			pf.provider = strings.TrimPrefix(arg, "--provider=")
		case arg == "--host":
			if i+1 >= len(args) {
				return nil, fmt.Errorf("flag --host requires a value")
			}
			i++
			pf.host = args[i]
		case strings.HasPrefix(arg, "--host="):
			pf.host = strings.TrimPrefix(arg, "--host=")
		case arg == "--force" || arg == "-f":
			pf.force = true
		case arg == "--yes" || arg == "-y":
			pf.yes = true
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

func deleteLocalSession(cfg *config.Config, provider, id string) (*providers.SessionMeta, error) {
	switch provider {
	case "codex":
		return codex.DeleteSession(cfg.CodexHome, id)
	case "agy":
		return agy.DeleteSession(cfg.AGYHome, id)
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}
}

func runDeleteTopLevel(cfg *config.Config, args []string, stdin io.Reader, stdout, stderr io.Writer, customRunner ssh.Runner) error {
	pf, err := parseFlexibleFlags(args)
	if err != nil {
		return err
	}
	prov := pf.provider
	if prov == "" {
		prov = "codex"
	}
	return runDelete(cfg, prov, args, stdin, stdout, stderr, customRunner)
}

func runCleanTopLevel(cfg *config.Config, args []string, stdin io.Reader, stdout, stderr io.Writer, customRunner ssh.Runner) error {
	pf, err := parseFlexibleFlags(args)
	if err != nil {
		return err
	}
	prov := pf.provider
	if prov == "" {
		prov = "codex"
	}
	return runClean(cfg, prov, args, stdin, stdout, stderr, customRunner)
}

func runDelete(cfg *config.Config, provider string, args []string, stdin io.Reader, stdout, stderr io.Writer, customRunner ssh.Runner) error {
	pf, err := parseFlexibleFlags(args)
	if err != nil {
		return err
	}

	host := pf.host
	var rawIDs []string
	rawIDs = append(rawIDs, pf.ids...)

	for i, pos := range pf.positionals {
		if host == "" && i == 0 && !isUUID(pos) && (len(pf.positionals) > 1 || len(pf.ids) > 0) {
			host = pos
			continue
		}
		rawIDs = append(rawIDs, pos)
	}

	seen := make(map[string]bool)
	var ids []string
	for _, id := range rawIDs {
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}

	if len(ids) == 0 {
		return fmt.Errorf("usage: async %s rm [targethost] <id...>", provider)
	}

	var runner ssh.Runner
	if host != "" {
		runner = customRunner
		if runner == nil {
			runner = ssh.NewClient(host, cfg.HostBinary(host))
		}
	}

	targetDesc := ""
	if host != "" {
		targetDesc = " on " + host
	}

	if len(ids) > 1 && !pf.yes && !pf.force && !pf.dryRun {
		ok, err := confirmPrompt(stdin, stdout, fmt.Sprintf("Are you sure you want to permanently delete these %d %s session(s)%s? [y/N]: ", len(ids), provider, targetDesc))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(stdout, "Deletion cancelled.")
			return nil
		}
	}

	var deleted []*providers.SessionMeta
	for _, id := range ids {
		if pf.dryRun {
			var meta *providers.SessionMeta
			if runner != nil {
				meta, _ = rpc.RemoteFind(runner, provider, id, "")
			} else {
				meta, _ = findLocalSession(cfg, provider, id, "")
			}
			title := ""
			steps := 0
			if meta != nil {
				title = truncateText(meta.Title, 40)
				steps = meta.StepCount
			}
			fmt.Fprintf(stdout, "[dry-run] Would delete %s session %s (%q, %d steps)%s\n", provider, id, title, steps, targetDesc)
			continue
		}

		var delMeta *providers.SessionMeta
		if runner != nil {
			delMeta, err = rpc.RemoteDelete(runner, provider, id)
		} else {
			delMeta, err = deleteLocalSession(cfg, provider, id)
		}
		if err != nil {
			return fmt.Errorf("deleting %s session %s: %w", provider, id, err)
		}
		deleted = append(deleted, delMeta)
		if !pf.jsonOutput {
			title := truncateText(delMeta.Title, 40)
			if title == "" {
				title = truncateText(delMeta.Preview, 40)
			}
			fmt.Fprintf(stdout, "Deleted %s session %s%s (%q, %d steps)\n", provider, delMeta.ID, targetDesc, title, delMeta.StepCount)
		}
	}

	if pf.jsonOutput && !pf.dryRun {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(deleted)
	}

	return nil
}

func runClean(cfg *config.Config, provider string, args []string, stdin io.Reader, stdout, stderr io.Writer, customRunner ssh.Runner) error {
	pf, err := parseFlexibleFlags(args)
	if err != nil {
		return err
	}
	cwd, err := resolveWorkingDir(pf.cwd)
	if err != nil {
		return err
	}

	host := pf.host
	if host == "" && len(pf.positionals) > 0 {
		host = pf.positionals[0]
	}

	var runner ssh.Runner
	if host != "" {
		runner = customRunner
		if runner == nil {
			runner = ssh.NewClient(host, cfg.HostBinary(host))
		}
	}

	targetWS := pf.workspace
	if targetWS == "" && !pf.all {
		if runner != nil {
			if pf.targetPath != "" {
				targetWS = pf.targetPath
			} else {
				projName := pathmap.ProjectName(cwd)
				if resolved, err := rpc.RemoteResolvePath(runner, cfg.HostPrefix(host), projName); err == nil {
					targetWS = resolved.Path
				}
			}
		} else {
			targetWS = cwd
		}
	}

	var allSessions []providers.SessionMeta
	if runner != nil {
		allSessions, err = rpc.RemoteList(runner, provider, targetWS, pf.all)
	} else {
		allSessions, err = listLocalSessions(cfg, provider, targetWS, pf.all)
	}
	if err != nil {
		return err
	}

	var candidates []providers.SessionMeta
	var skippedLocked int
	nowMs := time.Now().UnixMilli()

	for _, s := range allSessions {
		if s.Locked {
			skippedLocked++
			continue
		}
		if pf.olderThan > 0 {
			cutoffMs := nowMs - pf.olderThan.Milliseconds()
			if s.UpdatedAt >= cutoffMs {
				continue
			}
		}
		if targetWS != "" && !pf.all && s.Workspace != "" && s.Workspace != targetWS {
			continue
		}
		candidates = append(candidates, s)
	}

	if len(candidates) == 0 {
		if pf.jsonOutput {
			fmt.Fprintln(stdout, "[]")
			return nil
		}
		msg := fmt.Sprintf("No matching %s sessions found to clean", provider)
		if skippedLocked > 0 {
			msg += fmt.Sprintf(" (%d active/locked session(s) skipped)", skippedLocked)
		}
		fmt.Fprintln(stdout, msg+".")
		return nil
	}

	targetDesc := ""
	if host != "" {
		targetDesc = " on " + host
	}

	if !pf.jsonOutput {
		fmt.Fprintf(stdout, "Found %d %s session(s) to clean%s:\n", len(candidates), provider, targetDesc)
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tUPDATED\tSTEPS\tSUBS\tWORKSPACE\tTITLE")
		for _, s := range candidates {
			title := truncateText(s.Title, 40)
			if title == "" {
				title = truncateText(s.Preview, 40)
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\n",
				s.ID, s.UpdatedAtISO, s.StepCount, s.SubagentCount, s.Workspace, title)
		}
		_ = tw.Flush()
	}

	if pf.dryRun {
		if pf.jsonOutput {
			enc := json.NewEncoder(stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(candidates)
		}
		fmt.Fprintf(stdout, "\n[dry-run] %d session(s) would be deleted. No changes made.\n", len(candidates))
		return nil
	}

	if !pf.yes && !pf.force {
		ok, err := confirmPrompt(stdin, stdout, fmt.Sprintf("\nAre you sure you want to permanently delete these %d session(s)%s? [y/N]: ", len(candidates), targetDesc))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(stdout, "Clean cancelled.")
			return nil
		}
	}

	var deleted []providers.SessionMeta
	var deleteErrors []string
	for _, s := range candidates {
		var delMeta *providers.SessionMeta
		if runner != nil {
			delMeta, err = rpc.RemoteDelete(runner, provider, s.ID)
		} else {
			delMeta, err = deleteLocalSession(cfg, provider, s.ID)
		}
		if err != nil {
			deleteErrors = append(deleteErrors, fmt.Sprintf("%s: %v", s.ID, err))
			fmt.Fprintf(stderr, "warning: failed to delete %s session %s: %v\n", provider, s.ID, err)
			continue
		}
		deleted = append(deleted, *delMeta)
		if !pf.jsonOutput {
			title := truncateText(delMeta.Title, 40)
			if title == "" {
				title = truncateText(delMeta.Preview, 40)
			}
			fmt.Fprintf(stdout, "Deleted %s (%s, %d steps)\n", delMeta.ID, title, delMeta.StepCount)
		}
	}

	if pf.jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(deleted)
	}

	fmt.Fprintf(stdout, "\nSuccessfully cleaned %d %s session(s)%s.\n", len(deleted), provider, targetDesc)
	if skippedLocked > 0 {
		fmt.Fprintf(stdout, "(skipped %d active/locked session(s))\n", skippedLocked)
	}
	if len(deleteErrors) > 0 {
		return fmt.Errorf("%d deletion(s) failed", len(deleteErrors))
	}
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `async (Agent Sync) — Synchronize Codex and Antigravity CLI sessions over SSH

Usage:
  async <codex|agy> push [flags] <targethost>
  async <codex|agy> pull [flags] <targethost>
  async <codex|agy> ls [flags] [targethost]
  async <codex|agy> rm [flags] [targethost] <id...>
  async <codex|agy> clean [flags] [targethost]
  async status [targethost]
  async deploy <targethost>

Aliases:
  async rm [flags] [targethost] <id...>
  async clean [flags] [targethost]

Flags:
  --id, -i <uuid>        Explicit conversation ID (or pass --<uuid> directly)
  --target-path <path>   Explicit remote workspace directory override
  --cwd <path>           Override local working directory
  --workspace, -w <dir>  Filter clean/list by workspace directory
  --older-than <dur>     Filter clean by age (e.g. 7d, 24h, 30m)
  --provider, -p <name>  Specify provider for top-level commands (codex|agy, default: codex)
  --host <targethost>    Specify remote target host explicitly
  --yes, -y              Confirm bulk deletion without interactive prompt
  --force, -f            Overwrite destination or force deletion
  --dry-run              Show planned action without modifying state
  --all, -a              Operate across all workspaces (default is current project)
  --json                 Output results as JSON
  --version, -v          Show version
  --help, -h             Show this help message
`)
}
