// Package initcmd implements the `slack-acp init` first-run wizard.
//
// The wizard's only job is to collapse the after-manifest-paste steps
// — capture the two Slack tokens, sanity-check them, and persist them
// somewhere a supervisor unit can find them — into a single command.
//
//	$ slack-acp init
//	Bot token (xoxb-…): xoxb-...
//	App-level token (xapp-…): xapp-...
//	verifying with Slack auth.test … ok (team T01… user U01…)
//	wrote /Users/you/.config/slack-acp/config.json
//	wrote /Users/you/.config/slack-acp/env
//	next: run `slack-acp` (or set up a supervisor with the deploy skill)
//
// Tokens can also come from flags (`--bot-token` / `--app-token`) for
// non-interactive provisioning, in which case stdin isn't read at all.
//
// Verification (a live `auth.test` call) is on by default and can be
// skipped with `--skip-verify` for offline bootstrap.
package initcmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kfet/slack-acp/internal/config"
)

// Verifier validates a (bot, app) token pair against Slack. The
// returned displayName is shown to the operator on success
// (e.g. "team T01ABC user U02XYZ"). Stubbed in tests; the default
// implementation lives in DefaultVerifier and uses slack-go.
type Verifier func(ctx context.Context, botToken, appToken string) (displayName string, err error)

// Options bundles the wizard's inputs. Zero values are filled in with
// sensible defaults by Run.
type Options struct {
	// Pre-set tokens (from --bot-token / --app-token). Empty → prompt.
	BotToken string
	AppToken string

	// Output paths. Zero → config.DefaultConfigPath / DefaultEnvPath.
	ConfigPath string
	EnvPath    string

	// Behaviour switches.
	NonInteractive bool // if true and a required token is missing, fail instead of prompting
	SkipVerify     bool // if true, don't call Verifier
	Force          bool // if true, overwrite existing files; otherwise refuse

	// IO. Zero → os.Stdin / os.Stdout.
	In  io.Reader
	Out io.Writer

	// Verifier. Zero → DefaultVerifier.
	Verify Verifier
}

// Run executes the wizard. Returns nil on success; the caller (main)
// should treat any error as fatal.
func Run(ctx context.Context, opts Options) error {
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.ConfigPath == "" {
		opts.ConfigPath = config.DefaultConfigPath()
	}
	if opts.EnvPath == "" {
		opts.EnvPath = config.DefaultEnvPath()
	}
	if opts.Verify == nil {
		opts.Verify = DefaultVerifier
	}

	r := bufio.NewReader(opts.In)

	bot, err := readToken(r, opts.Out, "Bot token (xoxb-…)", opts.BotToken, opts.NonInteractive)
	if err != nil {
		return fmt.Errorf("bot token: %w", err)
	}
	app, err := readToken(r, opts.Out, "App-level token (xapp-…)", opts.AppToken, opts.NonInteractive)
	if err != nil {
		return fmt.Errorf("app token: %w", err)
	}
	if err := config.ValidateTokens(bot, app); err != nil {
		return err
	}

	if !opts.SkipVerify {
		fmt.Fprint(opts.Out, "verifying with Slack auth.test … ")
		who, err := opts.Verify(ctx, bot, app)
		if err != nil {
			fmt.Fprintln(opts.Out, "failed")
			return fmt.Errorf("verify: %w (re-run with --skip-verify to bypass)", err)
		}
		fmt.Fprintf(opts.Out, "ok (%s)\n", who)
	}

	// Refuse to clobber unless --force.
	if !opts.Force {
		for _, p := range []string{opts.ConfigPath, opts.EnvPath} {
			if _, err := os.Stat(p); err == nil {
				return fmt.Errorf("refusing to overwrite existing %s (re-run with --force)", p)
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("stat %s: %w", p, err)
			}
		}
	}

	if err := writeConfig(opts.ConfigPath, bot, app); err != nil {
		return err
	}
	fmt.Fprintf(opts.Out, "wrote %s\n", opts.ConfigPath)

	if err := writeEnv(opts.EnvPath, bot, app); err != nil {
		return err
	}
	fmt.Fprintf(opts.Out, "wrote %s\n", opts.EnvPath)

	fmt.Fprintf(opts.Out, "next: run `slack-acp --config %s`, or supervise it (see the deploy skill).\n", opts.ConfigPath)
	return nil
}

// readToken returns preset if non-empty; otherwise prompts the operator
// (when interactive) or fails (when non-interactive).
func readToken(r *bufio.Reader, out io.Writer, prompt, preset string, nonInteractive bool) (string, error) {
	if preset != "" {
		return strings.TrimSpace(preset), nil
	}
	if nonInteractive {
		return "", fmt.Errorf("missing (non-interactive mode; supply via flag)")
	}
	fmt.Fprintf(out, "%s: ", prompt)
	line, err := r.ReadString('\n')
	if err != nil && (err != io.EOF || line == "") {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// writeConfig writes a minimal JSON config containing only the tokens.
// Operators can hand-edit it afterwards to add policy / allowlist /
// agent_cmd; we deliberately don't pre-fill those so an unfamiliar
// operator doesn't end up with stale-looking defaults.
//
// File mode is 0600 — config holds secrets.
func writeConfig(path, bot, app string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	cfg := struct {
		BotToken string `json:"bot_token"`
		AppToken string `json:"app_token"`
	}{BotToken: bot, AppToken: app}
	// MarshalIndent on a struct of plain strings cannot fail; we
	// intentionally drop the error rather than carry a dead branch.
	body, _ := json.MarshalIndent(cfg, "", "  ")
	body = append(body, '\n')
	return os.WriteFile(path, body, 0o600)
}

// writeEnv writes a KEY=value env file suitable for systemd's
// EnvironmentFile= or a `set -a; . env; set +a` launchd wrapper.
func writeEnv(path, bot, app string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	body := fmt.Sprintf("SLACK_BOT_TOKEN=%s\nSLACK_APP_TOKEN=%s\n", bot, app)
	return os.WriteFile(path, []byte(body), 0o600)
}
