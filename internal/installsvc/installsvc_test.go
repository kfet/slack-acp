package installsvc

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fullOpts returns an Options populated with deterministic values so
// Render output is byte-stable across platforms.
func fullOpts(goos, dir string) Options {
	home := dir
	return Options{
		GOOS:       goos,
		BinaryPath: "/opt/homebrew/bin/slack-acp",
		ConfigPath: filepath.Join(home, ".config/slack-acp/config.json"),
		EnvPath:    filepath.Join(home, ".config/slack-acp/env"),
		Home:       home,
		User:       "alice",
		Label:      "dev.alice.slack-acp",
		AgentPATH:  "/Users/alice/go/bin:/opt/homebrew/bin:/usr/bin:/bin",
		OutPath:    filepath.Join(dir, "unit"),
	}
}

func TestRender_Linux(t *testing.T) {
	opts := fullOpts("linux", "/h")
	body, err := Render(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Description=slack-acp",
		"EnvironmentFile=/h/.config/slack-acp/env",
		"ExecStart=/opt/homebrew/bin/slack-acp --config /h/.config/slack-acp/config.json",
		"Restart=on-failure",
		"WantedBy=default.target",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("linux body missing %q\nfull:\n%s", want, body)
		}
	}
	// systemd doesn't need launchd-isms.
	for _, bad := range []string{"launchctl", "<plist", "RunAtLoad"} {
		if strings.Contains(body, bad) {
			t.Errorf("linux body shouldn't contain %q", bad)
		}
	}
}

func TestRender_Darwin(t *testing.T) {
	opts := fullOpts("darwin", "/Users/alice")
	body, err := Render(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"<key>Label</key><string>dev.alice.slack-acp</string>",
		"<string>/bin/sh</string>",
		// The sh -c payload is XML-escaped, so &apos; is the apostrophe.
		"set -a; . &apos;/Users/alice/.config/slack-acp/env&apos;",
		"exec &apos;/opt/homebrew/bin/slack-acp&apos; --config &apos;/Users/alice/.config/slack-acp/config.json&apos;",
		"<key>PATH</key><string>/Users/alice/go/bin:/opt/homebrew/bin:/usr/bin:/bin</string>",
		"<key>HOME</key><string>/Users/alice</string>",
		"<key>RunAtLoad</key><true/>",
		"<key>KeepAlive</key><true/>",
		"<key>StandardErrorPath</key><string>/Users/alice/Library/Logs/slack-acp.err.log</string>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("darwin body missing %q\nfull:\n%s", want, body)
		}
	}
}

func TestRender_UnsupportedGOOS(t *testing.T) {
	_, err := Render(Options{GOOS: "freebsd"})
	if err == nil || !strings.Contains(err.Error(), "unsupported GOOS") {
		t.Fatalf("err = %v", err)
	}
}

func TestRun_DryRun(t *testing.T) {
	out := &bytes.Buffer{}
	opts := fullOpts("linux", "/h")
	opts.DryRun = true
	opts.Out = out
	if err := Run(opts); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "# would write ") {
		t.Errorf("dry-run output should start with marker: %q", got)
	}
	if !strings.Contains(got, "EnvironmentFile=") {
		t.Errorf("dry-run output should include rendered body: %q", got)
	}
	// Nothing was written.
	if _, err := os.Stat(opts.OutPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dry-run shouldn't create the file: %v", err)
	}
}

func TestRun_WritesAndPrintsHints(t *testing.T) {
	for _, tc := range []struct {
		goos     string
		wantHint string
	}{
		{"linux", "systemctl --user enable --now slack-acp"},
		{"darwin", "launchctl bootstrap gui/$UID"},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			dir := t.TempDir()
			out := &bytes.Buffer{}
			opts := fullOpts(tc.goos, dir)
			opts.OutPath = filepath.Join(dir, "unit")
			opts.Out = out
			if err := Run(opts); err != nil {
				t.Fatal(err)
			}
			if fi, err := os.Stat(opts.OutPath); err != nil {
				t.Fatalf("stat: %v", err)
			} else if fi.Mode().Perm() != 0o644 {
				t.Errorf("mode = %o", fi.Mode().Perm())
			}
			if !strings.Contains(out.String(), "wrote "+opts.OutPath) {
				t.Errorf("missing wrote line: %q", out.String())
			}
			if !strings.Contains(out.String(), tc.wantHint) {
				t.Errorf("missing %s hint %q\nfull: %q", tc.goos, tc.wantHint, out.String())
			}
		})
	}
}

func TestRun_RefuseOverwrite(t *testing.T) {
	dir := t.TempDir()
	out := &bytes.Buffer{}
	opts := fullOpts("linux", dir)
	opts.OutPath = filepath.Join(dir, "unit")
	opts.Out = out
	if err := os.WriteFile(opts.OutPath, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := Run(opts)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err = %v", err)
	}
	// File untouched.
	if got, _ := os.ReadFile(opts.OutPath); string(got) != "existing" {
		t.Errorf("file overwritten: %q", got)
	}
}

func TestRun_Force(t *testing.T) {
	dir := t.TempDir()
	opts := fullOpts("linux", dir)
	opts.OutPath = filepath.Join(dir, "unit")
	opts.Force = true
	opts.Out = io.Discard
	if err := os.WriteFile(opts.OutPath, []byte("UNIQUE_OLD_BODY"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run(opts); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(opts.OutPath)
	if strings.Contains(string(got), "UNIQUE_OLD_BODY") {
		t.Errorf("not overwritten\nGOT:\n%s", got)
	}
}

func TestRun_StatError(t *testing.T) {
	// Point OutPath under a non-directory so os.Stat returns
	// something other than NotExist (specifically a ENOTDIR).
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := fullOpts("linux", dir)
	opts.OutPath = filepath.Join(blocker, "child", "unit")
	opts.Out = io.Discard
	err := Run(opts)
	if err == nil {
		t.Fatal("want stat error")
	}
}

func TestRun_UnsupportedGOOS(t *testing.T) {
	opts := fullOpts("freebsd", t.TempDir())
	opts.OutPath = filepath.Join(t.TempDir(), "unit")
	opts.Out = io.Discard
	if err := Run(opts); err == nil {
		t.Fatal("want unsupported error")
	}
}

func TestRun_MkdirError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.MkdirAll(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	opts := fullOpts("linux", dir)
	opts.OutPath = filepath.Join(ro, "sub", "unit")
	opts.Out = io.Discard
	err := Run(opts)
	if err == nil || !strings.Contains(err.Error(), "mkdir") {
		t.Fatalf("err = %v", err)
	}
}

func TestRun_WriteError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root")
	}
	dir := t.TempDir()
	ro := filepath.Join(dir, "ro")
	if err := os.MkdirAll(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	opts := fullOpts("linux", dir)
	opts.OutPath = filepath.Join(ro, "unit") // parent exists, file write fails
	opts.Out = io.Discard
	err := Run(opts)
	if err == nil || !strings.Contains(err.Error(), "write") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultUnitPath(t *testing.T) {
	cases := map[string]string{
		"linux":  "/h/.config/systemd/user/slack-acp.service",
		"darwin": "/h/Library/LaunchAgents/dev.alice.slack-acp.plist",
		"plan9":  "", // unsupported
	}
	for goos, want := range cases {
		if got := DefaultUnitPath(goos, "/h", "dev.alice.slack-acp"); got != want {
			t.Errorf("DefaultUnitPath(%q) = %q, want %q", goos, got, want)
		}
	}
}

func TestFillDefaults(t *testing.T) {
	// All defaults flow path.
	d := t.TempDir()
	t.Setenv("HOME", d)
	t.Setenv("USER", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	opts := Options{}
	if err := fillDefaults(&opts); err != nil {
		t.Fatal(err)
	}
	if opts.GOOS != runtime.GOOS {
		t.Errorf("GOOS = %q", opts.GOOS)
	}
	if opts.Home != d {
		t.Errorf("Home = %q", opts.Home)
	}
	if opts.User != filepath.Base(d) {
		t.Errorf("User fallback to home tail: %q", opts.User)
	}
	if !strings.HasSuffix(opts.Label, ".slack-acp") {
		t.Errorf("Label = %q", opts.Label)
	}
	if opts.BinaryPath == "" {
		t.Errorf("BinaryPath empty")
	}
	if opts.ConfigPath == "" || opts.EnvPath == "" {
		t.Errorf("paths empty")
	}
	if opts.AgentPATH == "" {
		t.Errorf("AgentPATH empty")
	}
}

func TestFillDefaults_HomeError(t *testing.T) {
	// On macOS/Linux os.UserHomeDir returns the HOME env var when set
	// and errors when not.
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "") // Windows belt-and-braces
	opts := Options{}
	err := fillDefaults(&opts)
	if err == nil || !strings.Contains(err.Error(), "home") {
		t.Fatalf("err = %v", err)
	}
}

func TestRun_FillDefaultsError(t *testing.T) {
	// Run should propagate fillDefaults errors.
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	err := Run(Options{GOOS: "linux", Out: io.Discard})
	if err == nil {
		t.Fatal("want fillDefaults error from Run")
	}
}

func TestFillDefaults_ExecutableEmpty(t *testing.T) {
	// Force osExecutable to return empty so the "slack-acp" fallback fires.
	orig := osExecutable
	osExecutable = func() (string, error) { return "", nil }
	t.Cleanup(func() { osExecutable = orig })
	opts := Options{Home: t.TempDir(), User: "x"}
	if err := fillDefaults(&opts); err != nil {
		t.Fatal(err)
	}
	if opts.BinaryPath != "slack-acp" {
		t.Errorf("BinaryPath = %q, want unqualified fallback", opts.BinaryPath)
	}
}

func TestEnableHints_Unsupported(t *testing.T) {
	if got := enableHints(Options{GOOS: "plan9"}); got != nil {
		t.Errorf("want nil for unsupported, got %v", got)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":          `'plain'`,
		"with space":     `'with space'`,
		"already'quoted": `'already'\''quoted'`,
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestXMLEscape(t *testing.T) {
	if got := xmlEscape(`a<b>&"c'`); got != `a&lt;b&gt;&amp;&quot;c&apos;` {
		t.Errorf("xmlEscape: %q", got)
	}
}
