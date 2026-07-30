package config_test

import (
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
