// Package installsvc generates a supervisor unit (systemd user unit on
// Linux, launchd LaunchAgent plist on macOS) that runs slack-acp under
// a long-lived process supervisor, pointing at the config + env file
// that `slack-acp init` already wrote.
//
// Render() is a pure function that returns the unit body for a
// fully-populated Options. Run() handles defaults + filesystem writes
// + post-write hints. Nothing in this package shells out to systemctl
// / launchctl — operators copy/paste the printed commands once
// they've reviewed the unit. Keeping the side effects minimal keeps
// blast radius small if --force is used carelessly.
package installsvc

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kfet/slack-acp/internal/config"
)

// Options configures unit-file generation. Zero values are filled in
// with defaults by Run; Render() requires every field set.
type Options struct {
	// GOOS is "linux" or "darwin". Zero → runtime.GOOS.
	GOOS string

	// BinaryPath is the absolute path to the slack-acp binary the
	// supervisor should exec. Zero → best-effort from os.Executable.
	BinaryPath string

	// ConfigPath / EnvPath are the files `slack-acp init` writes.
	// Zero → config.DefaultConfigPath / DefaultEnvPath.
	ConfigPath string
	EnvPath    string

	// Home + User are used to expand %h-style placeholders on Linux
	// and to build the launchd label / log paths on macOS. Zero →
	// os.UserHomeDir / os.Getenv("USER").
	Home string
	User string

	// Label is the launchd Label (also used as the plist filename).
	// Zero → "dev.<user>.slack-acp".
	Label string

	// OutPath is where the unit file is written. Zero → conventional
	// per-platform path (see DefaultUnitPath).
	OutPath string

	// AgentPATH is the PATH= value injected into the launchd
	// EnvironmentVariables block so the spawned ACP agent (`fir`,
	// `claude-code`, …) is findable. Ignored on Linux (systemd inherits
	// the user's PATH via the session). Zero → a sensible default.
	AgentPATH string

	// DryRun: print the rendered unit to Out instead of writing.
	DryRun bool

	// Force: overwrite an existing unit file.
	Force bool

	// Out is where dry-run output and post-write hints go. Zero → stdout.
	Out io.Writer
}

// Run fills defaults, renders the unit, and either prints it (DryRun)
// or writes it to disk. Post-write it prints the operator-facing
// commands to enable + start the service.
//
// Cross-GOOS shortcut: if opts.GOOS targets a different platform from
// runtime.GOOS and the operator hasn't supplied an explicit OutPath,
// Run auto-switches to dry-run output (writing the rendered unit to
// Out). The default unit path is meaningless on the wrong host —
// operators using `--goos linux` from a Mac dev box are virtually
// always piping the result to `ssh <host> 'cat > …'`.
func Run(opts Options) error {
	explicitOut := opts.OutPath != ""
	if err := fillDefaults(&opts); err != nil {
		return err
	}
	if !opts.DryRun && !explicitOut && opts.GOOS != runtime.GOOS {
		opts.DryRun = true
		fmt.Fprintf(opts.Out, "# cross-GOOS render (%s on %s) — preview only.\n", opts.GOOS, runtime.GOOS)
		fmt.Fprintln(opts.Out, "# Embedded paths (binary, config, env) reflect this host. For a real")
		fmt.Fprintln(opts.Out, "# install on the target box, ssh there and run `slack-acp install-service`")
		fmt.Fprintln(opts.Out, "# directly so paths match that host's layout.")
	}
	body, err := Render(opts)
	if err != nil {
		return err
	}

	if opts.DryRun {
		fmt.Fprintf(opts.Out, "# would write %s\n%s", opts.OutPath, body)
		return nil
	}

	if !opts.Force {
		if _, err := os.Stat(opts.OutPath); err == nil {
			return fmt.Errorf("refusing to overwrite existing %s (re-run with --force)", opts.OutPath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", opts.OutPath, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(opts.OutPath), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(opts.OutPath), err)
	}
	if err := os.WriteFile(opts.OutPath, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", opts.OutPath, err)
	}
	fmt.Fprintf(opts.Out, "wrote %s\n\n", opts.OutPath)
	fmt.Fprintln(opts.Out, "next, to enable and start the service:")
	for _, line := range enableHints(opts) {
		fmt.Fprintf(opts.Out, "  %s\n", line)
	}
	return nil
}

// Render returns the unit file body for the given fully-populated
// Options. Pure: no I/O, no globals.
func Render(opts Options) (string, error) {
	switch opts.GOOS {
	case "linux":
		return renderSystemd(opts), nil
	case "darwin":
		return renderLaunchd(opts), nil
	default:
		return "", fmt.Errorf("unsupported GOOS %q (linux and darwin only)", opts.GOOS)
	}
}

// DefaultUnitPath is the conventional location for the unit file on
// the given platform. Exported so the install-service subcommand can
// surface it in --help / dry-run output.
func DefaultUnitPath(goos, home, label string) string {
	switch goos {
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", "slack-acp.service")
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	}
	return ""
}

// osExecutable is overridable for tests; in production it always
// points at os.Executable.
var osExecutable = os.Executable

func fillDefaults(opts *Options) error {
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Home == "" {
		h, err := os.UserHomeDir()
		if err != nil || h == "" {
			return fmt.Errorf("cannot determine home directory: %w", err)
		}
		opts.Home = h
	}
	if opts.User == "" {
		opts.User = os.Getenv("USER")
		if opts.User == "" {
			// Last-resort fallback: derive from $HOME tail.
			opts.User = filepath.Base(opts.Home)
		}
	}
	if opts.Label == "" {
		opts.Label = "dev." + opts.User + ".slack-acp"
	}
	if opts.BinaryPath == "" {
		exe, _ := osExecutable()
		if exe == "" {
			// Last resort: unqualified name. systemd typically resolves
			// it via the user's PATH; launchd does not, so macOS
			// operators should pass --binary explicitly when this fires.
			exe = "slack-acp"
		}
		opts.BinaryPath = exe
	}
	if opts.ConfigPath == "" {
		opts.ConfigPath = config.DefaultConfigPath()
	}
	if opts.EnvPath == "" {
		opts.EnvPath = config.DefaultEnvPath()
	}
	if opts.AgentPATH == "" {
		opts.AgentPATH = defaultAgentPATH(opts.Home)
	}
	if opts.OutPath == "" {
		opts.OutPath = DefaultUnitPath(opts.GOOS, opts.Home, opts.Label)
	}
	return nil
}

// defaultAgentPATH is the PATH= the launchd plist exports for the
// spawned ACP agent. Must include the homebrew prefix (Apple Silicon
// vs Intel) plus the user's Go bin (where `fir` usually lives).
// launchd does NOT inherit the operator's shell PATH; without this,
// `fir` is unfindable.
func defaultAgentPATH(home string) string {
	parts := []string{
		filepath.Join(home, "go", "bin"),
		"/opt/homebrew/bin", // Apple Silicon
		"/usr/local/bin",    // Intel mac / Linux brew
		"/usr/bin",
		"/bin",
	}
	return strings.Join(parts, ":")
}

func renderSystemd(opts Options) string {
	return fmt.Sprintf(`[Unit]
Description=slack-acp
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=%s
ExecStart=%s --config %s
Restart=on-failure
RestartSec=2s

[Install]
WantedBy=default.target
`, opts.EnvPath, opts.BinaryPath, opts.ConfigPath)
}

func renderLaunchd(opts Options) string {
	// Wrap the binary in sh -c so the env file's KEY=value pairs are
	// exported into the process. launchd's own EnvironmentVariables
	// can't load a file directly.
	execLine := fmt.Sprintf(
		`set -a; . %s; set +a; exec %s --config %s`,
		shellQuote(opts.EnvPath), shellQuote(opts.BinaryPath), shellQuote(opts.ConfigPath),
	)
	logsDir := filepath.Join(opts.Home, "Library", "Logs")
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string>
    <string>-c</string>
    <string>%s</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>%s</string>
    <key>HOME</key><string>%s</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s/slack-acp.out.log</string>
  <key>StandardErrorPath</key><string>%s/slack-acp.err.log</string>
</dict>
</plist>
`,
		xmlEscape(opts.Label),
		xmlEscape(execLine),
		xmlEscape(opts.AgentPATH),
		xmlEscape(opts.Home),
		xmlEscape(logsDir),
		xmlEscape(logsDir),
	)
}

func enableHints(opts Options) []string {
	switch opts.GOOS {
	case "linux":
		return []string{
			"systemctl --user daemon-reload",
			"systemctl --user enable --now slack-acp",
			"loginctl enable-linger " + opts.User + "    # keep the unit alive across logouts/reboots",
		}
	case "darwin":
		return []string{
			"launchctl bootstrap gui/$UID " + opts.OutPath,
			"launchctl print     gui/$UID/" + opts.Label + " | head",
			"# restart: launchctl kickstart -k gui/$UID/" + opts.Label,
			"# stop:    launchctl bootout   gui/$UID/" + opts.Label,
		}
	}
	return nil
}

// shellQuote wraps s in single quotes for safe inclusion inside a
// POSIX shell command line (the `sh -c` payload of the launchd plist).
// Inside single quotes every byte is literal except `'` itself, which
// we close-escape-reopen as `'\”`.
func shellQuote(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `'\''`) + `'`
}

// xmlEscape escapes the five chars that can break a plist element body.
func xmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	)
	return r.Replace(s)
}
