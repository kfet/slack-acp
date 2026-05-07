package config

import (
	"os"
	"path/filepath"
	"testing"
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
	p := write(t, `{"bot_token":"xoxb-x","app_token":"xapp-x","agent_cmd":["fir","--mode","acp"],"policy":"read-only"}`)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Policy != "read-only" || len(c.AgentCmd) != 3 {
		t.Fatalf("bad: %+v", c)
	}
}

func TestLoadUnknownField(t *testing.T) {
	p := write(t, `{"nope":1}`)
	if _, err := Load(p); err == nil {
		t.Fatal("want err")
	}
}

func TestLoadBadPolicy(t *testing.T) {
	p := write(t, `{"policy":"weird"}`)
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

func TestValidateAllPolicies(t *testing.T) {
	// All accepted spellings exercise the explicit allow branches in the
	// switch — the rejected case is covered by TestLoadBadPolicy above.
	for _, p := range []string{"", "allow-all", "allow", "read-only", "readonly", "deny-all", "deny"} {
		c := &Config{Policy: p}
		if err := c.Validate(); err != nil {
			t.Fatalf("policy %q: %v", p, err)
		}
	}
}
