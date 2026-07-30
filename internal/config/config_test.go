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
