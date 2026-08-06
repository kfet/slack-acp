package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadOK(t *testing.T) {
	p := write(t, `{"bot_token":"xoxb-x","app_token":"xapp-x","agent_cmd":["fir","--mode","acp"]}`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.AgentCmd) != 3 {
		t.Fatalf("bad: %+v", c)
	}
}

func TestLoadUnknownField(t *testing.T) {
	p := write(t, `{"nope":1}`)
	if _, err := Load(p); err == nil {
		t.Fatal("want err")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "no-such")); err == nil {
		t.Fatal("want read error")
	}
}

func TestLoadMalformedJSON(t *testing.T) {
	p := write(t, `{not json`)
	if _, err := Load(p); err == nil {
		t.Fatal("want parse error")
	}
}

func TestValidateTokens(t *testing.T) {
	cases := []struct {
		name, bot, app string
		wantErr        string // substring; empty = wantOK
	}{
		{"ok", "xoxb-1-abc", "xapp-1-abc", ""},
		{"both missing", "", "", "missing Slack tokens"},
		{"bot missing", "", "xapp-1", "missing bot token"},
		{"app missing", "xoxb-1", "", "missing app-level token"},
		{"bot wrong shape", "xapp-swapped", "xapp-1", "bot token must start"},
		{"app wrong shape", "xoxb-1", "xoxb-swapped", "app token must start"},
		{"bot short hint", "xx", "xapp-1", "\"xx\""}, // truncatePrefix short branch
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTokens(tc.bot, tc.app)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want ok, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err=%v want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestDefaultConfigDir(t *testing.T) {
	// XDG branch.
	t.Setenv("XDG_CONFIG_HOME", "/x/cfg")
	if got := DefaultConfigDir(); got != "/x/cfg/slack-acp" {
		t.Errorf("XDG branch: %q", got)
	}
	if got := DefaultConfigPath(); got != "/x/cfg/slack-acp/config.json" {
		t.Errorf("ConfigPath: %q", got)
	}
	if got := DefaultEnvPath(); got != "/x/cfg/slack-acp/env" {
		t.Errorf("EnvPath: %q", got)
	}

	// HOME branch (XDG empty).
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/h")
	if got := DefaultConfigDir(); got != "/h/.config/slack-acp" {
		t.Errorf("HOME branch: %q", got)
	}

	// $TMPDIR fallback when HOME is empty and UserHomeDir errors. On
	// Unix os.UserHomeDir uses $HOME; emptying it forces the error.
	t.Setenv("HOME", "")
	got := DefaultConfigDir()
	if !filepath.IsAbs(got) || filepath.Base(got) != "slack-acp" {
		t.Errorf("tmpdir fallback: %q", got)
	}
}

func TestSessionIdleTimeout(t *testing.T) {
	if got := (&Config{}).IdleTimeout(); got != 0 {
		t.Fatalf("zero timeout: got %v", got)
	}
	c := &Config{SessionIdleTimeoutSeconds: 7}
	if got := c.IdleTimeout(); got != 7*time.Second {
		t.Fatalf("timeout: got %v", got)
	}
	if err := (&Config{SessionIdleTimeoutSeconds: -1}).Validate(); err == nil {
		t.Fatal("negative timeout should fail validation")
	}
}

func TestSelfDriveSentinelRejectsSlackEscapedChars(t *testing.T) {
	// Slack HTML-escapes <, > and & in inbound message text, so a
	// sentinel containing any of them arrives escaped
	// (<<DRIVE-TEST>> → &lt;&lt;DRIVE-TEST&gt;&gt;) and the
	// prefix match can never fire. The hatch then does nothing at all,
	// with no error anywhere — the worst failure mode. Reject it at
	// load time instead.
	cases := []struct {
		name     string
		sentinel string
		wantErr  bool
	}{
		{"angle brackets", "<<DRIVE-TEST>>", true},
		{"single less-than", "drive<test", true},
		{"single greater-than", "drive>test", true},
		{"ampersand", "drive&test", true},
		{"already-escaped form is just as bad", "&lt;&lt;DRIVE&gt;&gt;", true},
		{"plain token is fine", "drive-me-9f3a", false},
		{"punctuation Slack leaves alone", "!!drive-test!!", false},
		{"bang-bracket mix without angles", "[[drive-test]]", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Config{SelfDriveSentinel: tc.sentinel}).Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate() = nil for sentinel %q, want an error", tc.sentinel)
				}
				if !strings.Contains(err.Error(), "escape") {
					t.Fatalf("error should explain Slack escaping, got: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate() = %v, want nil for sentinel %q", err, tc.sentinel)
			}
		})
	}
}

func TestSelfDriveSentinelEscapeRuleLeavesSilentSentinelAlone(t *testing.T) {
	// The new rule must not disturb silent_sentinel, whose default
	// (<<SILENT>>) legitimately contains angle brackets: it is only
	// ever compared against agent *output*, never matched against
	// escaped inbound text.
	if err := (&Config{}).Validate(); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
	if err := (&Config{SilentSentinel: "<<SILENT>>"}).Validate(); err != nil {
		t.Fatalf("explicit default silent_sentinel rejected: %v", err)
	}
	if err := (&Config{SilentSentinel: "<<QUIET>>"}).Validate(); err != nil {
		t.Fatalf("custom bracketed silent_sentinel rejected: %v", err)
	}
	if got := (&Config{}).GetSilentSentinel(); got != "<<SILENT>>" {
		t.Fatalf("GetSilentSentinel() = %q, want the unchanged default", got)
	}

	// And the collision check still works on tokens that can actually
	// reach it (i.e. those without escaped characters).
	if err := (&Config{SelfDriveSentinel: "same-token-x", SilentSentinel: "same-token-x"}).Validate(); err == nil {
		t.Fatal("collision with a non-escaped silent_sentinel should still be rejected")
	}
}

// TestExampleConfigIsLoadable guards the operator-facing example
// against drift: it must parse under DisallowUnknownFields and pass
// Validate, so a copy-paste start is always a working start. (It is
// also why the example carries no explanatory comment keys — JSON has
// no comments and an unknown key would fail Load.)
func TestExampleConfigIsLoadable(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "docs", "config.example.json"))
	if err != nil {
		t.Fatalf("docs/config.example.json does not load: %v", err)
	}
	if c.SelfDriveSentinel != "" {
		t.Errorf("example must ship with the self-drive hatch OFF, got %q", c.SelfDriveSentinel)
	}
}

func TestSelfDriveConfig(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"off by default", Config{}, false},
		{"valid sentinel", Config{SelfDriveSentinel: "drive-me-9f3a"}, false},
		{"valid with explicit rate", Config{SelfDriveSentinel: "drive-me-9f3a", SelfDrivePerMinute: 10}, false},
		{"too short", Config{SelfDriveSentinel: "short"}, true},
		{"exactly 8 is ok", Config{SelfDriveSentinel: "12345678"}, false},
		{"7 is too short", Config{SelfDriveSentinel: "1234567"}, true},
		{"collides with explicit silent_sentinel", Config{SelfDriveSentinel: "same-token-x", SilentSentinel: "same-token-x"}, true},
		{"collides with default silent_sentinel", Config{SelfDriveSentinel: "<<SILENT>>"}, true},
		{"negative rate", Config{SelfDriveSentinel: "drive-me-9f3a", SelfDrivePerMinute: -1}, true},
		{"rate ignored when hatch off", Config{SelfDrivePerMinute: -1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestGetSelfDrivePerMinute(t *testing.T) {
	if got := (&Config{}).GetSelfDrivePerMinute(); got != 4 {
		t.Fatalf("default = %d, want 4", got)
	}
	if got := (&Config{SelfDrivePerMinute: 9}).GetSelfDrivePerMinute(); got != 9 {
		t.Fatalf("explicit = %d, want 9", got)
	}
}

func TestModelProbeBudget(t *testing.T) {
	// Unset means "let probe.Models pick its default", not "no budget".
	if got := (&Config{}).ModelProbeBudget(); got != 0 {
		t.Fatalf("zero budget: got %v, want 0 (defer to probe default)", got)
	}
	c := &Config{ModelProbeBudgetSeconds: 90}
	if got := c.ModelProbeBudget(); got != 90*time.Second {
		t.Fatalf("budget: got %v, want 90s", got)
	}
	if err := (&Config{ModelProbeBudgetSeconds: -1}).Validate(); err == nil {
		t.Fatal("negative model_probe_budget_seconds should fail validation")
	}
	if err := (&Config{ModelProbeBudgetSeconds: 300}).Validate(); err != nil {
		t.Fatalf("valid budget rejected: %v", err)
	}
}

func TestValidateBackfillMaxMessages(t *testing.T) {
	if err := (&Config{BackfillMaxMessages: -1}).Validate(); err == nil {
		t.Fatal("negative backfill_max_messages should fail validation")
	}
	if err := (&Config{BackfillMaxMessages: 0}).Validate(); err != nil {
		t.Fatalf("zero backfill_max_messages should validate: %v", err)
	}
	if err := (&Config{BackfillMaxMessages: 10}).Validate(); err != nil {
		t.Fatalf("positive backfill_max_messages should validate: %v", err)
	}
}

func TestGetSilentSentinel(t *testing.T) {
	if got := (&Config{}).GetSilentSentinel(); got != "<<SILENT>>" {
		t.Fatalf("default sentinel: got %q", got)
	}
	if got := (&Config{SilentSentinel: "##QUIET##"}).GetSilentSentinel(); got != "##QUIET##" {
		t.Fatalf("override sentinel: got %q", got)
	}
}

func TestGetBackfillMaxMessages(t *testing.T) {
	if got := (&Config{}).GetBackfillMaxMessages(); got != 50 {
		t.Fatalf("default backfill max: got %d", got)
	}
	if got := (&Config{BackfillMaxMessages: 0}).GetBackfillMaxMessages(); got != 50 {
		t.Fatalf("zero -> default: got %d", got)
	}
	if got := (&Config{BackfillMaxMessages: 200}).GetBackfillMaxMessages(); got != 200 {
		t.Fatalf("override backfill max: got %d", got)
	}
}

// TestValidateRejectsEmptyHumanAuthorID: an empty id in the list would
// match every author-less message (webhooks, classic bots) if the
// lookup were ever reordered, and it is always an operator typo. The
// guard refuses "" unconditionally today; this keeps the config from
// expressing the intent at all.
func TestValidateRejectsEmptyHumanAuthorID(t *testing.T) {
	c := &Config{HumanAuthorUserIDs: []string{"U1", "  "}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "human_author_user_ids") {
		t.Fatalf("got %v", err)
	}
	if err := (&Config{HumanAuthorUserIDs: []string{"U1"}}).Validate(); err != nil {
		t.Fatalf("a well-formed list must validate: %v", err)
	}
}
