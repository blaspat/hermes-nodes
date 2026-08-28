// hermes-node is the Go binary that pairs a laptop with a remote Hermes
// Agent brain. Subcommands:
//
//	hermes-node pair --server <wss-url> --token <token> [--name <name>] [--config <path>]
//	  Write a fresh config.toml with the supplied values, mode 0600.
//
//	hermes-node run [--config <path>]
//	  Long-lived background service. Loads the config, connects to the
//	  server, and stays connected via the reconnect supervisor.
//
//	hermes-node status
//	  Read the daemon's status file and display connection state.
//
//	hermes-node update [--version <tag>] [--restart-service] [--yes]
//	  Self-update from GitHub Releases.
//
//	hermes-node uninstall [--purge] [--dry-run]
//	  Remove binary, service registration, and optionally config.
//
//	hermes-node validate [--config <path>]
//	  Validate config.toml without connecting to the server.
//
//	hermes-node --version | --help
//	  Print version / usage and exit.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/blaspat/hermes-node/internal/audit"
	"github.com/blaspat/hermes-node/internal/config"
	execer "github.com/blaspat/hermes-node/internal/exec"
	"github.com/blaspat/hermes-node/internal/logger"
	"github.com/blaspat/hermes-node/internal/wire"
)

// version is set at build time via -ldflags "-X main.version=...". The
// default "dev" identifies a build made with `go run` or `go build` from
// source, not a tagged release.
var version = "dev"

// buildMetadata is populated from debug.ReadBuildInfo on init. It
// carries the Go version, VCS commit SHA, and commit timestamp so
// operators can identify exactly which build is running.
var buildMetadata string

func init() {
	if info, ok := debug.ReadBuildInfo(); ok {
		var goVer, revision, commitTime, dirty string
		goVer = info.GoVersion
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.time":
				commitTime = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					dirty = "-dirty"
				}
			}
		}
		parts := version
		if goVer != "" {
			parts += " " + goVer
		}
		if revision != "" {
			parts += " " + revision[:min(8, len(revision))] + dirty
		}
		if commitTime != "" {
			// Trim to date-only for conciseness.
			if len(commitTime) > 10 {
				commitTime = commitTime[:10]
			}
			parts += " " + commitTime
		}
		buildMetadata = parts
	}
	// If ReadBuildInfo failed (unlikely), buildMetadata stays empty
	// and --version falls back to the simple format.
}

// osExecutable is a variable so tests can override it.
var osExecutable = os.Executable

// osUserHomeDir is a variable so tests can override it.
var osUserHomeDir = os.UserHomeDir

const usage = `hermes-node — pair a laptop with a Hermes Agent brain

Usage:
  hermes-node pair --server <wss-url> --token <token> [--config <path>]
  hermes-node run [--config <path>]
  hermes-node stop
  hermes-node status
  hermes-node update [--version <tag>] [--no-service] [--yes]
  hermes-node uninstall [--purge] [--dry-run]
  hermes-node validate [--config <path>]
  hermes-node --version
  hermes-node --help

Flags:
  --config <path>   load/save config from this path
                    (default: ~/.hermes-node/config.toml)

'status':
  hermes-node status reads the daemon's status file and displays
  the current state, session ID, uptime, and last error. No server
  connection is made.

'stop':
  hermes-node stop sends SIGTERM to the running daemon. If the
  daemon doesn't exit within 5 seconds, it sends SIGKILL.

'update':
  hermes-node update downloads the latest release binary from
  GitHub and replaces the running binary. Service registration
  is not modified unless --restart-service is passed.

  Flags:
    --version <tag>       update to a specific version (default: latest)
    --restart-service     re-register the background service (requires
                          systemctl/launchd)
    --yes                 skip confirmation prompt

'uninstall':
  hermes-node uninstall removes the binary, stops and removes the
  background service (systemd/launchd), and leaves the config directory
  in place. Add --purge to also remove ~/.hermes-node/ (all config,
  tokens, and audit logs). Use --dry-run to preview what would be
  removed without making changes.

'validate':
  hermes-node validate parses config.toml, checks required fields,
  verifies allowed_paths exist and are accessible, validates TLS
  settings, and confirms the log path is writable. No server
  connection is made.

  Exit codes:
    0 — all checks passed
    1 — one or more validation failures
    2 — flag/argument error or config file not found

After pairing, run the node as a background service. See README.md for
the install and pair flows.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is the testable entry point. stdout/stderr are injected so tests
// can assert on output and so the binary can be re-exec'd from main()
// without leaking file descriptors across a test boundary.
func run(args []string, stdout, stderr io.Writer) int {
	// We do two passes: first extract the global --help / --version /
	// --config flags (which may appear before OR after the
	// subcommand), then dispatch the subcommand. A single FlagSet
	// doesn't gracefully handle flags-after-subcommand, and the
	// alternative (passing --config to every subcommand's FlagSet
	// separately) duplicates the parsing logic.
	showVersion, showHelp, configPath, defaultCfgErr, subArgs, err := parseGlobalArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}

	if *showVersion {
		if buildMetadata != "" {
			fmt.Fprintf(stdout, "hermes-node %s\n", buildMetadata)
		} else {
			fmt.Fprintf(stdout, "hermes-node %s\n", version)
		}
		return 0
	}
	if *showHelp {
		fmt.Fprint(stdout, usage)
		return 0
	}

	if len(subArgs) == 0 {
		fmt.Fprintln(stderr, "hermes-node: missing subcommand (run 'hermes-node --help')")
		return 2
	}

	// The `run` subcommand is the only one that must have a usable
	// config path; surface defaultCfgErr here (and only here) so
	// version/help work even when $HOME is unset, and pair does
	// too because pair is also a write path that will surface a
	// clearer error from config.Save.
	if (subArgs[0] == "run" || subArgs[0] == "pair") && *configPath == "" && defaultCfgErr != nil {
		fmt.Fprintf(stderr, "hermes-node: %v (pass --config <path> to override)\n", defaultCfgErr)
		return 1
	}

	switch subArgs[0] {
	case "pair":
		return runPair(subArgs[1:], *configPath, stdout, stderr)
	case "run":
		return runDetach(*configPath, subArgs[1:], stderr)
	case "uninstall":
		return runUninstall(subArgs[1:], stdout, stderr)
	case "validate":
		return runValidate(subArgs[1:], *configPath, stdout, stderr)
	case "status":
		return runStatus(subArgs[1:], stdout, stderr, *configPath)
	case "stop":
		return runStop(*configPath, stdout, stderr)
	case "update":
		return runUpdate(subArgs[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "hermes-node: unknown subcommand %q (run 'hermes-node --help')\n", subArgs[0])
		return 2
	}
}

// runDetach starts the daemon in the background. It forks the current
// process with stdout and stderr redirected to the log file.
//
// When called with HERMES_NODE_INNER=1 in the environment, it runs
// in the foreground instead — the env var is set by the parent before
// forking and acts as a sentinel so the child executes the real daemon
// logic instead of forking again.
func runDetach(configPath string, args []string, stderr io.Writer) int {
	// If we're the inner daemon (forked by the outer runDetach),
	// skip the fork and run the real daemon logic directly.
	if os.Getenv("HERMES_NODE_INNER") != "" {
		return runRunWithSignalCtx(configPath, os.Stdout, stderr)
	}

	// Single-instance guard (outer check): refuse before forking so
	// the CLI reports the refusal on the terminal, not in daemon.log.
	// The authoritative lock is taken by the daemon itself in runRun —
	// this early check just avoids spawning a doomed child. A missing
	// config means no lock file location yet; runRun will surface the
	// config error anyway.
	if configPath != "" {
		if pid, _ := lockOwnerPID(getConfigDir(configPath)); pid > 0 {
			fmt.Fprintf(stderr, "hermes-node: daemon already running (PID %d) — run 'hermes-node stop' first\n", pid)
			return 1
		}
	}

	binPath, err := osExecutable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes-node: could not determine binary path: %v\n", err)
		return 1
	}

	// Rebuild args for the child process.
	childArgs := []string{"run"}
	childArgs = append(childArgs, args...)
	if configPath != "" {
		childArgs = append(childArgs, "--config", configPath)
	}

	// Redirect stdout/stderr to a log file.
	logDir := filepath.Join(getConfigDir(configPath), "daemon.log")
	if err := os.MkdirAll(filepath.Dir(logDir), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "hermes-node: create log directory: %v\n", err)
		return 1
	}
	f, err := os.OpenFile(logDir, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermes-node: open log file: %v\n", err)
		return 1
	}

	cmd := exec.Command(binPath, childArgs...)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Env = append(os.Environ(), "HERMES_NODE_INNER=1")

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "hermes-node: start daemon: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stdout, "hermes-node: daemon started (PID %d). Log: %s\n", cmd.Process.Pid, logDir)
	return 0
}

// runRunWithSignalCtx is the production entry point for `hermes-node
// run`. It wires the signal handler and then calls runRun.
//
// We split signal-handling from the run loop so the test harness can
// drive runRun directly with its own context — Go's signal package
// only listens for OS signals, which a unit test can't easily send
// to its own goroutine without subprocess gymnastics.
func runRunWithSignalCtx(configPath string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runRun(ctx, configPath, stdout, stderr)
}

// parseGlobalArgs separates the global flags (which can appear anywhere
// in args) from the subcommand and its own args. This is the boring
// bookkeeping that lets `--config` work both before and after the
// subcommand keyword.
//
// Algorithm: a single pass over args, collecting non-global tokens
// into subArgs as we go. Global flags (`--config` and its value,
// `--version`, `--help`) are never appended. The first non-global
// token is the subcommand keyword; everything after it is the
// subcommand's own arg list, handed back verbatim to its FlagSet.
//
// The second return value is the error from deriving the default
// config path (i.e. $HOME is unset). It is propagated to the caller
// so the run subcommand can fail loudly; the version / help / pair
// subcommands don't need a path and can proceed without it.
//
// This means the subcommand keyword itself must not begin with `-`.
// That's the same constraint the standard Go `flag` package enforces.
func parseGlobalArgs(args []string) (showVersion, showHelp *bool, configPath *string, defaultCfgErr error, subArgs []string, err error) {
	version := false
	help := false
	cfg, derr := defaultConfigPath()
	defaultCfgErr = derr
	if derr != nil {
		// We can still parse args and dispatch subcommands
		// that don't need a path. The run subcommand will
		// surface this error itself.
		cfg = ""
	}
	subArgs = []string{}

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--version":
			version = true
			continue
		case "-h", "--help":
			help = true
			continue
		case "--config":
			if i+1 >= len(args) {
				return nil, nil, nil, nil, nil, errors.New("hermes-node: --config requires a path argument")
			}
			cfg = args[i+1]
			i++ // skip the value
			continue
		}
		if v, ok := stripFlagValue(a, "--config"); ok {
			cfg = v
			continue
		}
		subArgs = append(subArgs, a)
	}
	return &version, &help, &cfg, defaultCfgErr, subArgs, nil
}

// stripFlagValue handles --name=value form for a single known flag.
// Returns (value, true) if arg is `--name=value`, else ("", false).
func stripFlagValue(arg, name string) (string, bool) {
	prefix := name + "="
	if len(arg) > len(prefix) && arg[:len(prefix)] == prefix {
		return arg[len(prefix):], true
	}
	return "", false
}

// runPair writes a fresh config.toml. The file must not exist already —
// the operator's manual `rm` is the explicit "start over" signal. We
// don't prompt for confirmation because the data we're writing (URL +
// name + token) is one-shot: re-typing it costs more than re-typing the
// `rm`.
func runPair(args []string, configPath string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hermes-node pair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		server = fs.String("server", "", "server WSS URL (e.g. wss://vps.example.com:6969)")
		token  = fs.String("token", "", "pairing token issued by the server")
		name   = fs.String("name", "", "node name (required — must match the one registered on the server)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *server == "" {
		fmt.Fprintln(stderr, "hermes-node pair: --server is required")
		return 2
	}
	if *token == "" {
		fmt.Fprintln(stderr, "hermes-node pair: --token is required")
		return 2
	}
	if *name == "" {
		fmt.Fprintln(stderr, "hermes-node pair: --name is required")
		return 2
	}

	// The pair subcommand writes a minimal config. allowed_paths and
	// log_path stay at their operator-edited defaults (empty / default);
	// the operator adds allowed_paths by editing the file before
	// running `hermes-node run`. We intentionally don't ask for
	// allowed_paths at pair time — it would make the prompt long and
	// there's no validation we can do at this point (the paths may
	// not exist yet on a fresh machine).
	nodeName := *name
	cfg := &config.Config{
		Node: config.NodeConfig{
			ServerURL: *server,
			Name:      nodeName,
			Token:     *token,
		},
	}
	if err := config.Save(configPath, cfg); err != nil {
		fmt.Fprintf(stderr, "hermes-node pair: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "hermes-node: paired. Config written to %s (mode 0600).\n", configPath)
	fmt.Fprintf(stdout, "\n")
	fmt.Fprintf(stdout, "  Before starting the service, edit %s to set:\n", configPath)
	fmt.Fprintf(stdout, "    [node].allowed_paths  — list of filesystem roots exec/read/write can touch.\n")
	fmt.Fprintf(stdout, "                           Empty list (the default) means every path is\n")
	fmt.Fprintf(stdout, "                           rejected by the handlers. See SECURITY-REVIEW.md.\n")
	fmt.Fprintf(stdout, "\n")
	fmt.Fprintf(stdout, "  Then start the service: hermes-node run\n")
	return 0
}

// runUninstall removes the binary, stops and deregisters the background
// service, and optionally removes the config directory.
//
// Flags:
//
//	--purge    also remove ~/.hermes-node/ (config, audit log, tokens)
//	--dry-run  preview what would be removed without making changes
func runUninstall(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hermes-node uninstall", flag.ContinueOnError)
	fs.SetOutput(stderr)
	purge := fs.Bool("purge", false, "also remove ~/.hermes-node/ — THIS DELETES ALL STORED TOKENS AND CONFIG")
	dryRun := fs.Bool("dry-run", false, "preview changes without removing anything")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	home, err := osUserHomeDir()
	if err != nil {
		fmt.Fprintln(stderr, "hermes-node: could not determine home directory:", err)
		return 1
	}

	if *dryRun {
		fmt.Fprintln(stdout, "dry-run: no changes will be made.")
	}

	binPath, err := osExecutable()
	if err != nil {
		binPath = filepath.Join(home, ".local", "bin", "hermes-node")
	}
	configDir := filepath.Join(home, ".hermes-node")
	removed := 0
	errCount := 0

	// ---- service removal (per OS) ----
	switch runtime.GOOS {
	case "linux":
		serviceDir := filepath.Join(home, ".config", "systemd", "user")
		serviceFile := filepath.Join(serviceDir, "hermes-node.service")
		if _, err := os.Stat(serviceFile); err == nil {
			if commandExists("systemctl") {
				if *dryRun {
					fmt.Fprintf(stdout, "would run: systemctl --user disable --now hermes-node.service\n")
				} else {
					cmd := exec.Command("systemctl", "--user", "disable", "--now", "hermes-node.service")
					cmd.Stderr = stderr
					if err := cmd.Run(); err != nil {
						fmt.Fprintf(stderr, "hermes-node: warning: systemctl disable/stop failed: %v\n", err)
					}
					// Sync systemd state so a stale service definition
					// doesn't persist in the user instance.
					_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
				}
			}
			if *dryRun {
				fmt.Fprintf(stdout, "would remove service: %s\n", serviceFile)
			} else if err := os.Remove(serviceFile); err != nil {
				fmt.Fprintf(stderr, "hermes-node: remove service file %s: %v\n", serviceFile, err)
				errCount++
			} else {
				fmt.Fprintf(stdout, "removed service: %s\n", serviceFile)
				removed++
			}
			// Clean up empty parent dir.
			if *dryRun {
				fmt.Fprintf(stdout, "would clean up empty dir: %s\n", serviceDir)
			} else {
				_ = os.Remove(serviceDir) // succeeds only if empty
			}
		}
	case "darwin":
		label := "com.blaspat.hermes-node"
		launchAgentsDir := filepath.Join(home, "Library", "LaunchAgents")
		serviceFile := filepath.Join(launchAgentsDir, label+".plist")
		if _, err := os.Stat(serviceFile); err == nil {
			if commandExists("launchctl") {
				if *dryRun {
					fmt.Fprintf(stdout, "would run: launchctl unload %s\n", serviceFile)
				} else {
					cmd := exec.Command("launchctl", "unload", serviceFile)
					cmd.Stderr = stderr
					if err := cmd.Run(); err != nil {
						fmt.Fprintf(stderr, "hermes-node: warning: launchctl unload failed — plist may still be registered: %v\n", err)
					}
				}
			}
			if *dryRun {
				fmt.Fprintf(stdout, "would remove service: %s\n", serviceFile)
			} else if err := os.Remove(serviceFile); err != nil {
				fmt.Fprintf(stderr, "hermes-node: remove service file %s: %v\n", serviceFile, err)
				errCount++
			} else {
				fmt.Fprintf(stdout, "removed service: %s\n", serviceFile)
				removed++
			}
			// Clean up empty parent dir.
			if *dryRun {
				fmt.Fprintf(stdout, "would clean up empty dir: %s\n", launchAgentsDir)
			} else {
				_ = os.Remove(launchAgentsDir) // succeeds only if empty
			}
		}
	default:
		if runtime.GOOS == "windows" {
			fmt.Fprintln(stderr, "hermes-node: service removal on Windows is handled by the PowerShell installer (install.ps1 --Uninstall)")
		}
	}

	// ---- binary removal ----
	if _, err := os.Stat(binPath); err == nil {
		if runtime.GOOS == "windows" {
			fmt.Fprintf(stderr, "hermes-node: cannot remove running binary on Windows — stop the process first or use install.ps1 --Uninstall\n")
			errCount++
		} else {
			// Resolve symlinks so we remove the real binary, not a symlink.
			realPath, err := filepath.EvalSymlinks(binPath)
			if err != nil {
				realPath = binPath
			}
			if *dryRun {
				fmt.Fprintf(stdout, "would remove binary: %s\n", realPath)
			} else if err := os.Remove(realPath); err != nil {
				fmt.Fprintf(stderr, "hermes-node: remove binary %s: %v\n", realPath, err)
				errCount++
			} else {
				fmt.Fprintf(stdout, "removed binary: %s\n", realPath)
				removed++
			}
		}
	}

	// ---- config dir removal (only with --purge) ----
	if *purge {
		if _, err := os.Stat(configDir); err == nil {
			if *dryRun {
				fmt.Fprintf(stdout, "would remove config directory: %s\n", configDir)
			} else if err := os.RemoveAll(configDir); err != nil {
				fmt.Fprintf(stderr, "hermes-node: remove config dir %s: %v\n", configDir, err)
				errCount++
			} else {
				fmt.Fprintf(stdout, "removed config directory: %s\n", configDir)
				removed++
			}
		}
	}

	if *dryRun {
		fmt.Fprintln(stdout, "dry-run complete. No changes were made.")
		return 0
	}

	if removed == 0 && errCount == 0 {
		fmt.Fprintln(stdout, "nothing to uninstall — no binary, service, or config found.")
	} else if errCount == 0 {
		fmt.Fprintln(stdout, "uninstall complete.")
	} else {
		fmt.Fprintf(stdout, "uninstall finished with %d error(s).\n", errCount)
	}

	if !*purge {
		fmt.Fprintf(stdout, "config directory left in place: %s (pass --purge to remove — THIS DELETES ALL TOKENS)\n", configDir)
	}

	if errCount > 0 {
		return 1
	}
	return 0
}

// commandExists reports whether an executable is on PATH.
func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// runValidate parses config.toml and validates its contents without
// connecting to the server. Checks:
//   - TOML syntax and required fields (via config.Load)
//   - allowed_paths exist and are accessible
//   - Log path parent directory is writable
//   - TLS ca_cert file exists (if set)
//   - TLS pinned_cert_sha256 is valid hex (if set via config.BuildTLSConfig)
func runValidate(args []string, configPath string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hermes-node validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Check the file exists before loading, so we can distinguish
	// "wrong path" (exit 2) from "config has content errors" (exit 1).
	if _, err := os.Stat(configPath); err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(stderr, "hermes-node: config file not found: %s\n", configPath)
			return 2
		}
		fmt.Fprintf(stderr, "hermes-node: config file error: %v\n", err)
		return 2
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "hermes-node: config validation failed: %v\n", err)
		return 1
	}

	passed := 0
	failed := 0

	fmt.Fprintf(stdout, "validating %s ...\n", configPath)

	// 1. Required fields — config.Load already validated these, so
	//    if we got here they're fine.
	fmt.Fprintf(stdout, "  [OK] required fields: server_url, name, token\n")
	passed++

	// 2. allowed_paths — each must exist and be a directory. This
	//    check is point-in-time: a path that exists now may be
	//    removed later. The filesystem handler re-checks at runtime.
	if len(cfg.Node.AllowedPaths) == 0 {
		fmt.Fprintf(stdout, "  [WARN] allowed_paths is empty — all filesystem calls will be rejected\n")
	} else {
		allOK := true
		for _, p := range cfg.Node.AllowedPaths {
			info, err := os.Stat(p)
			if err != nil {
				fmt.Fprintf(stdout, "  [FAIL] allowed_path %s: %v\n", p, err)
				failed++
				allOK = false
			} else if !info.IsDir() {
				fmt.Fprintf(stdout, "  [FAIL] allowed_path %s: not a directory\n", p)
				failed++
				allOK = false
			}
		}
		if allOK {
			fmt.Fprintf(stdout, "  [OK] allowed_paths (%d paths)\n", len(cfg.Node.AllowedPaths))
			passed++
		}
	}

	// 3. Log path — check parent dir is writable.
	logDir := filepath.Dir(cfg.Node.LogPath)
	info, err := os.Stat(logDir)
	if err != nil {
		fmt.Fprintf(stdout, "  [FAIL] log directory %s: %v\n", logDir, err)
		failed++
	} else if !info.IsDir() {
		fmt.Fprintf(stdout, "  [FAIL] log path %s: parent %s is not a directory\n", cfg.Node.LogPath, logDir)
		failed++
	} else {
		// Check writability by creating and removing a temp dir.
		// Using MkdirTemp avoids leaving an orphan file on SIGKILL.
		tmpDir, err := os.MkdirTemp(logDir, "hermes-node-validate-*")
		if err != nil {
			fmt.Fprintf(stdout, "  [FAIL] log directory %s: not writable: %v\n", logDir, err)
			failed++
		} else {
			os.Remove(tmpDir)
			fmt.Fprintf(stdout, "  [OK] log path: %s\n", cfg.Node.LogPath)
			passed++
		}
	}

	// 4. TLS settings — config.BuildTLSConfig validates ca_cert
	//    and pinned_cert_sha256 together.
	tlsSet := cfg.Server.CACert != "" || cfg.Server.PinnedCertSHA256 != ""
	_, err = config.BuildTLSConfig(cfg.Server)
	if err != nil {
		fmt.Fprintf(stdout, "  [FAIL] TLS config: %v\n", err)
		failed++
	} else if tlsSet {
		fmt.Fprintf(stdout, "  [OK] TLS config\n")
		passed++
	}
	// If no TLS settings are configured, that's fine — no check needed.

	// 5. Proxy URL — validate the URL when set.
	if cfg.Node.ProxyURL != "" {
		proxyURL, err := url.Parse(cfg.Node.ProxyURL)
		if err != nil {
			fmt.Fprintf(stdout, "  [FAIL] proxy_url: %v\n", err)
			failed++
		} else if proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
			fmt.Fprintf(stdout, "  [FAIL] proxy_url: scheme %q must be http or https\n", proxyURL.Scheme)
			failed++
		} else {
			fmt.Fprintf(stdout, "  [OK] proxy_url: %s\n", cfg.Node.ProxyURL)
			passed++
		}
	}

	if failed == 0 {
		fmt.Fprintf(stdout, "\nhermes-node: config is valid (%d checks passed).\n", passed)
		return 0
	}
	fmt.Fprintf(stdout, "\nhermes-node: config has %d error(s), %d check(s) passed.\n", failed, passed)
	return 1
}

// runRun is the long-lived service. It blocks until ctx is cancelled
// (clean shutdown) and exits 0 on a clean shutdown, 1 on a fatal
// error. A recoverable error inside the supervisor (network drop,
// server bye) never bubbles up — the supervisor's reconnect cycle
// handles it.
//
// ctx is supplied by the caller. Production wires signal handling
// (see runRunWithSignalCtx); tests pass a deadline-bounded ctx.
func runRun(ctx context.Context, configPath string, stdout, stderr io.Writer) int {
	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "hermes-node: %v\n", err)
		return 1
	}

	// Single-instance guard (authoritative): refuse to start when
	// another live daemon holds the lock. The lock file lives next
	// to config.toml and is removed on a clean exit; a crashed
	// daemon leaves a stale lock that the next start reclaims.
	releaseLock, err := acquireDaemonLock(getConfigDir(configPath))
	if err != nil {
		fmt.Fprintf(stderr, "hermes-node: %v\n", err)
		return 1
	}
	defer releaseLock()

	logLevel, _ := logger.ParseLevel(cfg.Node.LogLevel)

	// When running as the inner daemon (HERMES_NODE_INNER set), replace
	// stdout/stderr with a rotating file so daemon.log auto-rotates.
	if os.Getenv("HERMES_NODE_INNER") != "" {
		dlogPath := filepath.Join(getConfigDir(configPath), "daemon.log")
		rf, err := logger.NewRotatingFile(dlogPath, 10*1024*1024, 5)
		if err == nil {
			stdout = rf
			stderr = rf
			defer rf.Close()
		}
		// If opening the rotating file fails, fall through with the
		// original stdout/stderr (the parent already set them to the
		// raw daemon.log fd, so logging still works — just no rotation).
	}

	log := logger.NewWithWriters(logLevel, stdout, stderr)

	auditLog, err := audit.New(cfg.Node.LogPath)
	if err != nil {
		log.Error("open audit log: %v", err)
		return 1
	}
	defer func() {
		if cerr := auditLog.Close(); cerr != nil {
			log.Error("close audit log: %v", cerr)
		}
	}()

	// Initialize the status file so `hermes-node status` works
	// even before the first connection attempt.
	statusPath := statusFilePath(getConfigDir(configPath))
	startedAt := time.Now().UTC().Format(time.RFC3339)
	if err := writeStatus(statusPath, &nodeStatus{
		PID:       os.Getpid(),
		State:     "starting",
		Name:      cfg.Node.Name,
		ServerURL: cfg.Node.ServerURL,
		Version:   version,
		StartedAt: startedAt,
	}); err != nil {
		log.Warn("write status file: %v", err)
	}

	// Start a goroutine that reloads config on SIGHUP. The reload
	// applies changes to the logger level; other config changes
	// (allowed_paths, log_path) require a full restart.
	sighupCh := make(chan os.Signal, 1)
	signal.Notify(sighupCh, syscall.SIGHUP)
	go func() {
		for range sighupCh {
			reloaded, err := config.Load(configPath)
			if err != nil {
				log.Warn("SIGHUP reload failed: %v", err)
				continue
			}
			// Apply log level change at runtime.
			newLevel, ok := logger.ParseLevel(reloaded.Node.LogLevel)
			if !ok {
				log.Warn("SIGHUP: unknown log_level %q, keeping current level", reloaded.Node.LogLevel)
			} else {
				log.SetLevel(newLevel)
				log.Info("SIGHUP: config reloaded (log_level=%s)", reloaded.Node.LogLevel)
			}
		}
	}()

	log.Info("version %s: connecting to %s as %q (%d allowed paths)",
		version, cfg.Node.ServerURL, cfg.Node.Name, len(cfg.Node.AllowedPaths))

	// prevSession tracks the persistent shell from the previous
	// (re)connect, so we can close it before allocating a new
	// one. Without this, a flaky-network operator leaks one
	// bash PID per reconnect (Go does not reap subprocesses
	// when a *Session reference is dropped). Quinn's review
	// flagged this as the only finding a real operator could
	// trip over.
	var (
		prevSessionMu sync.Mutex
		prevSession   *execer.Session
	)

	// Build the TLS config once, outside the Dialer closure.
	// VerifyConnection captures the pin (if any) by closure, so
	// the same config is safe to reuse across reconnects.
	tlsCfg, err := config.BuildTLSConfig(cfg.Server)
	if err != nil {
		log.Error("%v", err)
		return 1
	}

	sup, err := wire.NewSupervisor(wire.SupervisorOptions{
		Dialer: func(ctx context.Context) (*wire.Client, error) {
			return wire.Connect(ctx, wire.DialOptions{
				ServerURL:    cfg.Node.ServerURL,
				NodeName:     cfg.Node.Name,
				Token:        cfg.Node.Token,
				NodeVersion:  version,
				Platform:     runtime.GOOS,
				Arch:         runtime.GOARCH,
				Capabilities: []string{"exec", "exec_chunk", "read", "write"},
				TLSConfig:    tlsCfg,
				ProxyURL:     cfg.Node.ProxyURL,
				DebugLog:     log.Debug,
			})
		},
		// Setup is invoked once per (re)connect. We build a fresh
		// persistent shell here so each connection starts with a
		// clean bash. The protocol doesn't promise shell state
		// survives reconnects (the server re-issues calls on
		// reconnect), so this is the right grain.
		Setup: func(ctx context.Context, c *wire.Client, d *wire.Dispatcher, p *wire.Pinger) error {
			// Bump the watchdog clock on every received frame.
			d.OnRead = p.MarkAlive

			// Update status file with session info on (re)connect.
			if err := writeStatus(statusPath, &nodeStatus{
				PID:             os.Getpid(),
				State:           "connected",
				Name:            cfg.Node.Name,
				ServerURL:       cfg.Node.ServerURL,
				Version:         version,
				SessionID:       c.SessionID(),
				StartedAt:       startedAt,
				LastConnectedAt: time.Now().UTC().Format(time.RFC3339),
			}); err != nil {
				log.Warn("write status file: %v", err)
			}
			// Surface wire-level errors (handler panics, write
			// failures) to the operator via stderr AND the audit
			// log, with the panic stack included so a postmortem
			// grep has the trace, not just the panic value.
			// Without the OnError hook the panic-recovery path
			// in dispatch.go is invisible to the operator.
			d.OnError = func(err error, env wire.Envelope) {
				stack := debug.Stack()
				log.Error("dispatch error: %v (type=%s id=%s) %s",
					err, env.Type, env.ID, stack)
				_ = auditLog.Write(audit.Entry{
					Action: "dispatch_error",
					Target: err.Error() + "\n" + string(stack),
					Status: "error",
				})
			}

			// Close the previous session before opening a new
			// one so a flaky-network operator doesn't leak bash
			// PIDs across reconnects. Close is idempotent and
			// safe even if the previous session's bash already
			// exited (failPending on the demuxer is a no-op).
			prevSessionMu.Lock()
			old := prevSession
			prevSession = nil
			prevSessionMu.Unlock()
			if old != nil {
				_ = old.Close()
			}

			session, err := execer.NewSession(ctx, cfg.Node.AllowedPaths, execer.DefaultMaxOutputBytes)
			if err != nil {
				return fmt.Errorf("start shell: %w", err)
			}
			// Publish the new session so the next Setup call
			// can close it. We intentionally do NOT close it
			// here — the session is meant to live for the
			// lifetime of the connection.
			prevSessionMu.Lock()
			prevSession = session
			prevSessionMu.Unlock()

			execHandler := wire.NewExecHandler(
				execer.NewSessionAdapter(session),
				cfg.Node.AllowedPaths,
				auditLog,
			)
			execHandler.WriteFunc = d.WriteEnvelope
			execHandler.ChunkSize = cfg.Node.ChunkSizeValue()
			execHandler.SetServerCapabilities(c.ServerCapabilities())
			fsys := wire.NewFileSystem(cfg.Node.AllowedPaths, auditLog)
			if err := d.Register(wire.TypeExec, execHandler.Handle); err != nil {
				return fmt.Errorf("register exec: %w", err)
			}
			if err := d.Register(wire.TypeRead, fsys.ReadHandler); err != nil {
				return fmt.Errorf("register read: %w", err)
			}
			if err := d.Register(wire.TypeWrite, fsys.WriteHandler); err != nil {
				return fmt.Errorf("register write: %w", err)
			}
			return nil
		},
		AuditLog:       auditLog,
		Logger:         log,
		BackoffInitial: cfg.Node.BackoffInitialDuration(),
		BackoffMax:     cfg.Node.BackoffMaxDuration(),
		BackoffFactor:  cfg.Node.BackoffFactorValue(),
	})
	if err != nil {
		log.Error("build supervisor: %v", err)
		return 1
	}

	// Supervisor.Run blocks until ctx is cancelled (clean shutdown)
	// or the dialer returns a fatal error (config-invalid, etc.).
	// Network drops, heartbeats, server-byes are all recoverable
	// and do NOT cause Run to return.
	runErr := sup.Run(ctx)
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		log.Error("supervisor exited: %v", runErr)
		if err := writeStatus(statusPath, &nodeStatus{
			PID:       os.Getpid(),
			State:     "stopped",
			Name:      cfg.Node.Name,
			ServerURL: cfg.Node.ServerURL,
			Version:   version,
			StartedAt: startedAt,
			LastError: runErr.Error(),
		}); err != nil {
			log.Warn("write status file: %v", err)
		}
		return 1
	}
	log.Info("clean shutdown")
	if err := writeStatus(statusPath, &nodeStatus{
		PID:       os.Getpid(),
		State:     "stopped",
		Name:      cfg.Node.Name,
		ServerURL: cfg.Node.ServerURL,
		Version:   version,
		StartedAt: startedAt,
	}); err != nil {
		log.Warn("write status file: %v", err)
	}
	return 0
}

// ---- update subcommand ----

const (
	githubRepo = "blaspat/hermes-node"
	githubDL   = "https://github.com/" + githubRepo + "/releases/download"
)

// githubAPI is the GitHub releases API endpoint. Made a variable so
// tests can override it to simulate network failures without actually
// hitting the network.
var githubAPI = "https://api.github.com/repos/" + githubRepo + "/releases/latest"

// httpClient is used for GitHub API and download requests. The
// timeout prevents indefinite hangs on unreachable networks.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// latestReleaseTag fetches the latest release tag from GitHub API.
func latestReleaseTag() (string, error) {
	req, err := http.NewRequest("GET", githubAPI, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", "hermes-node-update/1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch latest release: HTTP %d", resp.StatusCode)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", fmt.Errorf("parse latest release: %w", err)
	}
	if rel.TagName == "" {
		return "", fmt.Errorf("latest release has no tag_name")
	}
	return rel.TagName, nil
}

// runUpdate downloads and replaces the running binary.
func runUpdate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hermes-node update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	versionFlag := fs.String("version", "", "specific version tag (default: latest)")
	restartService := fs.Bool("restart-service", false, "re-register the background service after update")
	assumeYes := fs.Bool("yes", false, "skip confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// 1. Determine target version.
	tag := *versionFlag
	if tag == "" {
		fmt.Fprintf(stdout, "looking up latest release of %s ...\n", githubRepo)
		var err error
		tag, err = latestReleaseTag()
		if err != nil {
			fmt.Fprintf(stderr, "hermes-node: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "latest release: %s\n", tag)
	}

	// 2. Get current binary path.
	binPath, err := osExecutable()
	if err != nil {
		fmt.Fprintf(stderr, "hermes-node: could not determine binary path: %v\n", err)
		return 1
	}

	// 3. Confirm with user unless --yes.
	if !*assumeYes {
		fmt.Fprintf(stdout, "this will replace %s with %s. Continue? [y/N] ", binPath, tag)
		var reply string
		fmt.Scanln(&reply)
		reply = strings.TrimSpace(reply)
		if reply != "y" && reply != "Y" && reply != "yes" && reply != "YES" {
			fmt.Fprintln(stdout, "update cancelled.")
			return 0
		}
	}

	// 4. Download the release binary.
	assetName := fmt.Sprintf("hermes-node-%s-%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		assetName += ".exe"
	}
	dlURL := fmt.Sprintf("%s/%s/%s", githubDL, tag, assetName)
	fmt.Fprintf(stdout, "downloading %s ...\n", dlURL)

	tmpFile, err := os.CreateTemp("", "hermes-node-update-*")
	if err != nil {
		fmt.Fprintf(stderr, "hermes-node: create temp file: %v\n", err)
		return 1
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	req, err := http.NewRequest("GET", dlURL, nil)
	if err != nil {
		fmt.Fprintf(stderr, "hermes-node: build download request: %v\n", err)
		return 1
	}
	req.Header.Set("User-Agent", "hermes-node-update/1.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "hermes-node: download: %v\n", err)
		return 1
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		fmt.Fprintf(stderr, "hermes-node: download failed: HTTP %d (URL: %s)\n", resp.StatusCode, dlURL)
		return 1
	}
	_, err = io.Copy(tmpFile, resp.Body)
	resp.Body.Close()
	tmpFile.Close()
	if err != nil {
		fmt.Fprintf(stderr, "hermes-node: write download: %v\n", err)
		return 1
	}

	// 5. Verify the downloaded binary is valid.
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmpPath, 0o755); err != nil {
			fmt.Fprintf(stderr, "hermes-node: chmod temp binary: %v\n", err)
			return 1
		}
	}
	verOut, err := exec.Command(tmpPath, "--version").CombinedOutput()
	if err != nil {
		fmt.Fprintf(stderr, "hermes-node: downloaded binary is not a valid hermes-node: %v\n  output: %s\n", err, verOut)
		return 1
	}
	fmt.Fprintf(stdout, "downloaded: %s", verOut)

	// 6. Replace the running binary.
	if runtime.GOOS == "windows" {
		fmt.Fprintf(stderr, "hermes-node: cannot replace a running binary on Windows. Copy %s to %s manually.\n", tmpPath, binPath)
		return 1
	}
	if err := os.Rename(tmpPath, binPath); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			fmt.Fprintf(stderr, "hermes-node: temp file is on a different filesystem than %s.\n", binPath)
			fmt.Fprintf(stderr, "  Run: cp %s %s && chmod +x %s\n", tmpPath, binPath, binPath)
		} else {
			fmt.Fprintf(stderr, "hermes-node: replace binary %s: %v\n", binPath, err)
		}
		return 1
	}
	fmt.Fprintf(stdout, "replaced: %s\n", binPath)

	// 7. Optionally re-register the service.
	restartFailed := false
	if *restartService {
		switch runtime.GOOS {
		case "linux":
			if commandExists("systemctl") {
				fmt.Fprintf(stdout, "restarting systemd user service ...\n")
				if err := exec.Command("systemctl", "--user", "daemon-reload").Run(); err != nil {
					fmt.Fprintf(stderr, "hermes-node: daemon-reload: %v\n", err)
					restartFailed = true
				}
				if err := exec.Command("systemctl", "--user", "restart", "hermes-node.service").Run(); err != nil {
					fmt.Fprintf(stderr, "hermes-node: restart service: %v\n", err)
					restartFailed = true
				}
			}
		case "darwin":
			if commandExists("launchctl") {
				fmt.Fprintf(stdout, "restarting launchd agent ...\n")
				label := "com.blaspat.hermes-node"
				plist := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", label+".plist")
				if err := exec.Command("launchctl", "unload", plist).Run(); err != nil {
					fmt.Fprintf(stderr, "hermes-node: launchctl unload: %v\n", err)
					restartFailed = true
				}
				if err := exec.Command("launchctl", "load", plist).Run(); err != nil {
					fmt.Fprintf(stderr, "hermes-node: launchctl load: %v\n", err)
					restartFailed = true
				}
			}
		}
	}
	if restartFailed {
		fmt.Fprintf(stderr, "hermes-node: service restart failed — the binary was updated but the service was not restarted.\n")
	}

	fmt.Fprintf(stdout, "update complete. Restart the daemon or reboot to pick up the new binary.\n")
	if restartFailed {
		return 1
	}
	return 0
}

// ---- single-instance daemon lock (used by runRun daemon and runDetach) ----

// daemonLockPath returns the lock file path for a config dir. The lock
// lives next to config.toml, alongside the status file and daemon.log.
func daemonLockPath(configDir string) string {
	return filepath.Join(configDir, "daemon.lock")
}

// lockOwnerPID reads the lock file in configDir and returns the PID it
// names, but ONLY if that process is still alive. A lock file whose
// PID is dead (or unreadable) is stale — the previous daemon crashed
// or was killed — and is removed so a fresh start can claim it. It
// returns 0 when no live daemon holds the lock.
func lockOwnerPID(configDir string) (int, error) {
	lockPath := daemonLockPath(configDir)
	data, err := os.ReadFile(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil || pid <= 0 {
		// Unparseable content = stale/foreign file; reclaim it.
		_ = os.Remove(lockPath)
		return 0, nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		// On Unix FindProcess rarely fails; treat as not running.
		_ = os.Remove(lockPath)
		return 0, nil
	}
	if proc.Signal(os.Signal(syscall.Signal(0))) != nil {
		// Dead PID — stale lock; reclaim it.
		_ = os.Remove(lockPath)
		return 0, nil
	}
	return pid, nil
}

// acquireDaemonLock enforces the single-instance rule. It returns an
// error if a live daemon already holds the lock. On success it creates
// the lock file naming this process and returns a cleanup func that
// removes it (callers defer the cleanup so a crashed daemon leaves a
// stale lock that the next start reclaims via lockOwnerPID).
func acquireDaemonLock(configDir string) (func(), error) {
	lockPath := daemonLockPath(configDir)
	if pid, err := lockOwnerPID(configDir); err != nil {
		return nil, err
	} else if pid > 0 {
		return nil, fmt.Errorf("daemon already running (PID %d) — run 'hermes-node stop' first", pid)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		return nil, fmt.Errorf("write daemon lock: %w", err)
	}
	return func() {
		// Only remove the lock if it still names us. A takeover by a
		// newer daemon must not be clobbered by our cleanup.
		if data, err := os.ReadFile(lockPath); err == nil {
			var pid int
			if _, err := fmt.Sscanf(string(data), "%d", &pid); err == nil && pid == os.Getpid() {
				_ = os.Remove(lockPath)
			}
		}
	}, nil
}

// ---- status file (used by runRun daemon and status subcommand) ----

// nodeStatus is written by the daemon process and read by
// `hermes-node status`. Fields are JSON-tagged for file format
// stability. Times are RFC3339 strings for human readability.
//
// The status file is written atomically via tmp+rename, so a
// concurrent reader sees either the old file or the new file,
// never a partial write. On Windows, os.Rename is NOT atomic
// when the target exists, but the window is small and the
// worst case is a stale read (the old file is still there).
type nodeStatus struct {
	PID       int    `json:"pid"`
	State     string `json:"state"` // "starting" | "connected" | "stopped"
	Name      string `json:"name,omitempty"`
	ServerURL string `json:"server_url,omitempty"`
	Version   string `json:"version,omitempty"`
	SessionID string `json:"session_id,omitempty"`

	StartedAt       string `json:"started_at,omitempty"`
	LastConnectedAt string `json:"last_connected_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
}

// statusFilePath returns the default status file location under configDir.
func statusFilePath(configDir string) string {
	return filepath.Join(configDir, "status.json")
}

// writeStatus atomically writes status to the JSON file.
func writeStatus(path string, s *nodeStatus) error {
	if s == nil {
		return fmt.Errorf("status: cannot write nil")
	}
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("status: marshal: %w", err)
	}
	// Write to a temp file first, then rename for atomicity.
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o600); err != nil {
		return fmt.Errorf("status: write tmp: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("status: rename: %w", err)
	}
	return nil
}

// readStatus reads and parses the status file at path.
func readStatus(path string) (*nodeStatus, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s nodeStatus
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("status: parse: %w", err)
	}
	return &s, nil
}

// runStatus reads and displays the daemon status file, cross-checking
// the PID against the actual process table so the output reflects
// reality even if the status file is stale.
func runStatus(args []string, stdout, stderr io.Writer, configPath string) int {
	fs := flag.NewFlagSet("hermes-node status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfgDir := getConfigDir(configPath)
	statusPath := statusFilePath(cfgDir)

	s, err := readStatus(statusPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(stdout, "hermes-node: daemon not running (status file not found)")
			return 0
		}
		fmt.Fprintf(stderr, "hermes-node: read status: %v\n", err)
		return 1
	}

	// Check if the PID is actually alive by sending signal 0.
	// On most Unix systems FindProcess always succeeds; the real
	// check is whether Signal(0) returns an error (ESRCH).
	alive := false
	if s.PID > 0 {
		proc, err := os.FindProcess(s.PID)
		if err == nil && proc.Signal(os.Signal(syscall.Signal(0))) == nil {
			alive = true
		}
	}

	// Derive the real state from PID liveness, not just the file.
	displayState := s.State
	if !alive {
		switch s.State {
		case "connected", "starting":
			displayState = "stopped"
		}
	} else if s.State == "stopped" {
		// Status file is stale — process is running but file says stopped.
		displayState = "running"
	}

	fmt.Fprintf(stdout, "hermes-node %s\n", s.Version)
	fmt.Fprintf(stdout, "  PID:       %d", s.PID)
	if !alive {
		fmt.Fprintf(stdout, " (not running)")
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "  State:     %s\n", displayState)
	if s.Name != "" {
		fmt.Fprintf(stdout, "  Name:      %s\n", s.Name)
	}
	if s.ServerURL != "" {
		fmt.Fprintf(stdout, "  Server:    %s\n", s.ServerURL)
	}
	if s.SessionID != "" && alive {
		fmt.Fprintf(stdout, "  Session:   %s\n", s.SessionID)
	}
	if s.StartedAt != "" {
		fmt.Fprintf(stdout, "  Started:   %s\n", s.StartedAt)
	}
	if s.LastConnectedAt != "" {
		fmt.Fprintf(stdout, "  Connected: %s\n", s.LastConnectedAt)
	}
	if s.LastError != "" {
		fmt.Fprintf(stdout, "  Last err:  %s\n", s.LastError)
	}
	return 0
}

// runStop sends SIGTERM to the running daemon and waits for it to
// exit. If the daemon doesn't stop within 5 seconds, it escalates
// to SIGKILL.
func runStop(configPath string, stdout, stderr io.Writer) int {
	cfgDir := getConfigDir(configPath)
	statusPath := statusFilePath(cfgDir)

	s, err := readStatus(statusPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintln(stderr, "hermes-node: no daemon running (status file not found)")
			return 1
		}
		fmt.Fprintf(stderr, "hermes-node: read status: %v\n", err)
		return 1
	}

	if s.PID <= 0 {
		fmt.Fprintln(stderr, "hermes-node: no PID recorded in status file")
		return 1
	}

	proc, err := os.FindProcess(s.PID)
	if err != nil {
		fmt.Fprintf(stderr, "hermes-node: find process %d: %v\n", s.PID, err)
		return 1
	}

	// Check if the process is actually alive.
	if proc.Signal(os.Signal(syscall.Signal(0))) != nil {
		fmt.Fprintf(stdout, "hermes-node: daemon (PID %d) is not running\n", s.PID)
		return 0
	}

	fmt.Fprintf(stdout, "hermes-node: stopping daemon (PID %d)...\n", s.PID)

	// Send SIGTERM.
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		fmt.Fprintf(stderr, "hermes-node: signal SIGTERM: %v\n", err)
		return 1
	}

	// Wait up to 5 seconds for it to exit.
	for i := 0; i < 5; i++ {
		time.Sleep(time.Second)
		if proc.Signal(os.Signal(syscall.Signal(0))) != nil {
			fmt.Fprintln(stdout, "hermes-node: daemon stopped")
			return 0
		}
	}

	// Escalate to SIGKILL.
	fmt.Fprintf(stdout, "hermes-node: daemon did not respond to SIGTERM, sending SIGKILL...\n")
	if err := proc.Signal(syscall.SIGKILL); err != nil {
		fmt.Fprintf(stderr, "hermes-node: signal SIGKILL: %v\n", err)
		return 1
	}

	fmt.Fprintln(stdout, "hermes-node: daemon killed")
	return 0
}

// getConfigDir returns the config directory from a config file path.
// e.g., "/home/user/.hermes-node/config.toml" → "/home/user/.hermes-node"
func getConfigDir(configPath string) string {
	if configPath == "" {
		return ""
	}
	return filepath.Dir(configPath)
}

// defaultConfigPath returns the operator-default location for
// config.toml. It mirrors defaultLogPath in internal/config —
// returns an error if the home directory cannot be determined,
// rather than silently producing a relative path that would
// resolve to the current working directory and confuse the
// operator with a "file not found" error whose path is a lie.
func defaultConfigPath() (string, error) {
	home, err := osUserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory for default config path: %w", err)
	}
	return filepath.Join(home, ".hermes-node", "config.toml"), nil
}
