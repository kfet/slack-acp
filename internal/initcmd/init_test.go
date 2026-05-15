package initcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubVerifier returns a Verifier that records its call and returns a
// canned response. Used to keep tests offline.
func stubVerifier(want string, err error) (Verifier, *int) {
	calls := 0
	return func(_ context.Context, bot, app string) (string, error) {
		calls++
		return want, err
	}, &calls
}

func tmpPaths(t *testing.T) (cfg, env string) {
	t.Helper()
	d := t.TempDir()
	return filepath.Join(d, "config.json"), filepath.Join(d, "env")
}

func TestRun_HappyPath_Interactive(t *testing.T) {
	cfg, env := tmpPaths(t)
	in := strings.NewReader("xoxb-paste\nxapp-paste\n")
	out := &bytes.Buffer{}
	verify, calls := stubVerifier("team T1 user U1", nil)

	err := Run(context.Background(), Options{
		ConfigPath: cfg,
		EnvPath:    env,
		In:         in,
		Out:        out,
		Verify:     verify,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("verifier calls = %d, want 1", *calls)
	}
	if !strings.Contains(out.String(), "ok (team T1 user U1)") {
		t.Fatalf("output missing verifier success line: %q", out.String())
	}

	// config.json: well-formed JSON with both tokens, mode 0600.
	if got := mustStat(t, cfg).Mode().Perm(); got != 0o600 {
		t.Errorf("config mode = %o, want 0600", got)
	}
	body, _ := os.ReadFile(cfg)
	var got struct {
		BotToken string `json:"bot_token"`
		AppToken string `json:"app_token"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("config not valid json: %v", err)
	}
	if got.BotToken != "xoxb-paste" || got.AppToken != "xapp-paste" {
		t.Errorf("config tokens = %+v", got)
	}

	// env: KEY=value pair, mode 0600.
	if got := mustStat(t, env).Mode().Perm(); got != 0o600 {
		t.Errorf("env mode = %o, want 0600", got)
	}
	envBody, _ := os.ReadFile(env)
	want := "SLACK_BOT_TOKEN=xoxb-paste\nSLACK_APP_TOKEN=xapp-paste\n"
	if string(envBody) != want {
		t.Errorf("env body = %q", envBody)
	}
}

func TestRun_HappyPath_FlagsAndSkipVerify(t *testing.T) {
	cfg, env := tmpPaths(t)
	out := &bytes.Buffer{}
	verify, calls := stubVerifier("never called", nil)

	err := Run(context.Background(), Options{
		BotToken:   "xoxb-flag",
		AppToken:   "xapp-flag",
		ConfigPath: cfg,
		EnvPath:    env,
		SkipVerify: true,
		// In: nil — should NOT be read since both tokens preset.
		Out:    out,
		Verify: verify,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("verifier should be skipped, got %d calls", *calls)
	}
	if !strings.Contains(out.String(), "wrote "+cfg) {
		t.Fatalf("output missing wrote line: %q", out.String())
	}
}

func TestRun_NonInteractive_MissingToken(t *testing.T) {
	cfg, env := tmpPaths(t)
	err := Run(context.Background(), Options{
		ConfigPath:     cfg,
		EnvPath:        env,
		NonInteractive: true,
		SkipVerify:     true,
		Out:            io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "non-interactive") {
		t.Fatalf("err = %v, want non-interactive complaint", err)
	}
}

func TestRun_BadTokenShape(t *testing.T) {
	cfg, env := tmpPaths(t)
	err := Run(context.Background(), Options{
		BotToken:   "xapp-swapped",
		AppToken:   "xapp-real",
		ConfigPath: cfg,
		EnvPath:    env,
		SkipVerify: true,
		Out:        io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "bot token must start") {
		t.Fatalf("err = %v, want shape complaint", err)
	}
	// Nothing should have been written.
	if _, err := os.Stat(cfg); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config should not exist on failure: %v", err)
	}
}

func TestRun_VerifyFails(t *testing.T) {
	cfg, env := tmpPaths(t)
	out := &bytes.Buffer{}
	verify, _ := stubVerifier("", errors.New("invalid_auth"))

	err := Run(context.Background(), Options{
		BotToken:   "xoxb-x",
		AppToken:   "xapp-x",
		ConfigPath: cfg,
		EnvPath:    env,
		Out:        out,
		Verify:     verify,
	})
	if err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Fatalf("err = %v, want invalid_auth", err)
	}
	if !strings.Contains(out.String(), "failed") {
		t.Fatalf("output missing failure marker: %q", out.String())
	}
	if _, err := os.Stat(cfg); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("config should not exist on verify failure: %v", err)
	}
}

func TestRun_RefuseOverwrite(t *testing.T) {
	cfg, env := tmpPaths(t)
	if err := os.WriteFile(cfg, []byte("existing\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), Options{
		BotToken:   "xoxb-1",
		AppToken:   "xapp-1",
		ConfigPath: cfg,
		EnvPath:    env,
		SkipVerify: true,
		Out:        io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err = %v, want overwrite refusal", err)
	}
	// Existing file untouched.
	if got, _ := os.ReadFile(cfg); string(got) != "existing\n" {
		t.Errorf("file overwritten: %q", got)
	}
}

func TestRun_ForceOverwrites(t *testing.T) {
	cfg, env := tmpPaths(t)
	if err := os.WriteFile(cfg, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), Options{
		BotToken:   "xoxb-1",
		AppToken:   "xapp-1",
		ConfigPath: cfg,
		EnvPath:    env,
		SkipVerify: true,
		Force:      true,
		Out:        io.Discard,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := os.ReadFile(cfg)
	if strings.Contains(string(got), "old") {
		t.Errorf("not overwritten: %q", got)
	}
}

func TestRun_DefaultsFilled(t *testing.T) {
	// When ConfigPath/EnvPath are empty, defaults should be used.
	// We can't write to the real default location (would clobber the
	// operator's config), so just check the wiring via a config home
	// override.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	out := &bytes.Buffer{}
	err := Run(context.Background(), Options{
		BotToken:   "xoxb-1",
		AppToken:   "xapp-1",
		SkipVerify: true,
		Out:        out,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "slack-acp/config.json") {
		t.Errorf("output should mention default config path: %q", out.String())
	}
}

func TestRun_StatErrorOnPreflight(t *testing.T) {
	// Force os.Stat to return a non-NotExist error by pointing the
	// config path at a path under a non-directory.
	d := t.TempDir()
	blocker := filepath.Join(d, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(blocker, "child", "config.json")
	env := filepath.Join(d, "env")
	err := Run(context.Background(), Options{
		BotToken:   "xoxb-1",
		AppToken:   "xapp-1",
		ConfigPath: cfg,
		EnvPath:    env,
		SkipVerify: true,
		Out:        io.Discard,
	})
	if err == nil {
		t.Fatal("want stat error")
	}
}

func TestRun_ReadTokenEOF(t *testing.T) {
	// Empty stdin → ReadString returns io.EOF with empty line; we
	// treat that as an empty token, then ValidateTokens fails.
	cfg, env := tmpPaths(t)
	err := Run(context.Background(), Options{
		ConfigPath: cfg,
		EnvPath:    env,
		SkipVerify: true,
		In:         strings.NewReader(""),
		Out:        io.Discard,
	})
	if err == nil {
		t.Fatal("want error from empty input")
	}
}

func TestRun_MkdirFailure(t *testing.T) {
	// Make the parent directory unwritable so MkdirAll fails inside
	// writeConfig. Skipped on root or systems where we can't drop
	// permissions; the failure path itself is still exercised by
	// the TestRun_StatErrorOnPreflight case via Stat error.
	if os.Getuid() == 0 {
		t.Skip("running as root; cannot test write-protected dir")
	}
	d := t.TempDir()
	ro := filepath.Join(d, "ro")
	if err := os.MkdirAll(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	cfg := filepath.Join(ro, "sub", "config.json")
	env := filepath.Join(d, "env")
	err := Run(context.Background(), Options{
		BotToken:   "xoxb-1",
		AppToken:   "xapp-1",
		ConfigPath: cfg,
		EnvPath:    env,
		SkipVerify: true,
		Out:        io.Discard,
	})
	if err == nil {
		t.Fatal("want mkdir error")
	}
}

func TestRun_EnvMkdirFailure(t *testing.T) {
	// Config writes successfully, then env mkdir fails.
	if os.Getuid() == 0 {
		t.Skip("running as root")
	}
	d := t.TempDir()
	cfg := filepath.Join(d, "config.json")
	ro := filepath.Join(d, "ro")
	if err := os.MkdirAll(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	env := filepath.Join(ro, "sub", "env")
	err := Run(context.Background(), Options{
		BotToken:   "xoxb-1",
		AppToken:   "xapp-1",
		ConfigPath: cfg,
		EnvPath:    env,
		SkipVerify: true,
		Out:        io.Discard,
	})
	if err == nil {
		t.Fatal("want env mkdir error")
	}
}

func TestRun_AppTokenReadEOF(t *testing.T) {
	cfg, env := tmpPaths(t)
	// Bot preset; only app needs reading, and stdin is empty.
	err := Run(context.Background(), Options{
		BotToken:   "xoxb-1",
		ConfigPath: cfg,
		EnvPath:    env,
		SkipVerify: true,
		In:         strings.NewReader(""),
		Out:        io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "app token") {
		t.Fatalf("err = %v, want app-token complaint", err)
	}
}

func TestRun_DefaultOutIsStdout(t *testing.T) {
	// Pass nil Out and verify it doesn't panic. We can't easily assert
	// the writes land on os.Stdout without capturing it; the goal here
	// is purely to exercise the `opts.Out = os.Stdout` defaulting line.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })
	cfg, env := tmpPaths(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, r)
	}()
	rerr := Run(context.Background(), Options{
		BotToken:   "xoxb-1",
		AppToken:   "xapp-1",
		ConfigPath: cfg,
		EnvPath:    env,
		SkipVerify: true,
		// Out: nil — defaulted to os.Stdout.
	})
	_ = w.Close()
	<-done
	if rerr != nil {
		t.Fatalf("Run: %v", rerr)
	}
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return fi
}
