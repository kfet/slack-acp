package config_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/kfet/slack-acp/internal/config"
)

func TestScrubbedEnvDropsSlackNames(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"SLACK_BOT_TOKEN=xoxb-secret",
		"HOME=/home/kfet",
		"SLACK_APP_TOKEN=xapp-secret",
	}
	got := config.ScrubbedEnv(in)
	want := []string{"PATH=/usr/bin", "HOME=/home/kfet"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ScrubbedEnv = %q, want %q", got, want)
	}
	// Input must not be mutated.
	if len(in) != 4 {
		t.Fatalf("input mutated: %q", in)
	}
}

func TestScrubbedEnvDropsByValueUnderAnyName(t *testing.T) {
	in := []string{
		"MY_BOT=xoxb-secret",
		"UNRELATED=keep",
		"COPY_OF_APP=xapp-secret",
	}
	got := config.ScrubbedEnv(in, "xoxb-secret", "xapp-secret")
	want := []string{"UNRELATED=keep"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ScrubbedEnv = %q, want %q", got, want)
	}
}

func TestScrubbedEnvIgnoresEmptySecretsAndMalformedEntries(t *testing.T) {
	// An empty secret must not drop every valueless variable, and an
	// entry with no "=" is passed through untouched.
	in := []string{"EMPTY=", "NOEQUALS", "KEEP=1"}
	got := config.ScrubbedEnv(in, "", "")
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("ScrubbedEnv = %q, want %q", got, in)
	}
}

func TestScrubbedEnvEmptyInput(t *testing.T) {
	if got := config.ScrubbedEnv(nil, "x"); len(got) != 0 {
		t.Fatalf("ScrubbedEnv(nil) = %q, want empty", got)
	}
}

// TestScrubbedEnvNeverNil pins the invariant the whole scrub rests on.
//
// acp-kit's client.Config documents Env as "if nil, os.Environ() is
// used" (client/agent.go), and exec.Cmd has the same rule: a nil Env
// inherits the parent environment, a non-nil empty Env does not. So a
// nil return here would not mean "no variables" — it would mean "hand
// the agent every variable we have, tokens included". The scrub would
// fail open, silently, exactly in the case where it scrubbed the most.
//
// Every result must therefore be non-nil, including the two paths that
// tempt a future `var out []string` simplification: an empty input, and
// an input scrubbed down to nothing.
func TestScrubbedEnvNeverNil(t *testing.T) {
	cases := []struct {
		name    string
		environ []string
		secrets []string
	}{
		{"nil input", nil, nil},
		{"empty input", []string{}, nil},
		{"everything dropped by name", []string{"SLACK_BOT_TOKEN=xoxb-secret"}, nil},
		{"everything dropped by value", []string{"MY_BOT=xoxb-secret"}, []string{"xoxb-secret"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := config.ScrubbedEnv(tc.environ, tc.secrets...)
			if got == nil {
				t.Fatal("ScrubbedEnv returned nil: acp-kit/exec would inherit the full environment, leaking the tokens this function exists to remove")
			}
			if len(got) != 0 {
				t.Fatalf("ScrubbedEnv = %q, want empty", got)
			}
		})
	}
}

// AgentClientConfig is the *call site* of the credential scrub. It
// lives in internal/ (not cmd/slack-acp/main.go, which .covignore
// excludes) precisely so this test can pin it: a regression that drops
// the Env field would hand the spawned agent the Slack tokens, and
// before this test nothing would have failed.
func TestAgentClientConfigScrubsCredentials(t *testing.T) {
	c := &config.Config{
		AgentCmd: []string{"fir", "--mode", "acp"},
		StateDir: "/var/lib/slack-acp",
		BotToken: "xoxb-secret",
		AppToken: "xapp-secret",
	}
	environ := []string{
		"PATH=/usr/bin",
		"SLACK_BOT_TOKEN=xoxb-secret",
		"SLACK_APP_TOKEN=xapp-secret",
		"BESPOKE_NAME=xoxb-secret",
	}
	var stderr bytes.Buffer

	got := c.AgentClientConfig(environ, &stderr)

	if want := []string{"PATH=/usr/bin"}; !reflect.DeepEqual(got.Env, want) {
		t.Fatalf("Env = %q, want %q (credentials must not reach the agent)", got.Env, want)
	}
	if !reflect.DeepEqual(got.Command, c.AgentCmd) {
		t.Fatalf("Command = %q, want %q", got.Command, c.AgentCmd)
	}
	if got.Cwd != c.StateDir {
		t.Fatalf("Cwd = %q, want %q", got.Cwd, c.StateDir)
	}
	if got.Stderr != &stderr {
		t.Fatalf("Stderr not forwarded")
	}
}

// A nil Env means "inherit os.Environ()" to acp-kit's client.Config, so
// an empty environment must still produce a non-nil slice — otherwise
// the scrub inverts into handing the agent everything. See the
// ScrubbedEnv doc comment.
func TestAgentClientConfigEnvNeverNil(t *testing.T) {
	c := &config.Config{BotToken: "xoxb-secret"}
	got := c.AgentClientConfig(nil, nil)
	if got.Env == nil {
		t.Fatal("Env is nil: acp-kit would inherit the full environment, tokens included")
	}
	if len(got.Env) != 0 {
		t.Fatalf("Env = %q, want empty", got.Env)
	}
}
