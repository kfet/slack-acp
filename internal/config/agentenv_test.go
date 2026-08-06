package config_test

import (
	"bytes"
	"reflect"
	"slices"
	"testing"

	"github.com/kfet/slack-acp/internal/config"
)

// AgentClientConfig is the assembly point for the credential scrub. It
// lives in internal/ (not cmd/slack-acp/main.go, which .covignore
// excludes) precisely so this test can pin it: the scrub itself now runs
// inside acp-kit's client.Start, but the config assembled here must still
// DECLARE the Slack credentials as secrets, or Start has nothing to drop
// and the spawned agent would inherit the tokens.
func TestAgentClientConfigDeclaresCredentials(t *testing.T) {
	c := &config.Config{
		AgentCmd: []string{"fir", "--mode", "acp"},
		StateDir: "/var/lib/slack-acp",
		BotToken: "xoxb-secret",
		AppToken: "xapp-secret",
	}
	var stderr bytes.Buffer

	got := c.AgentClientConfig(&stderr)

	// Names dropped by variable name.
	if want := []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "SLACK_USER_TOKEN"}; !reflect.DeepEqual(got.SecretEnvNames, want) {
		t.Fatalf("SecretEnvNames = %q, want %q", got.SecretEnvNames, want)
	}
	// Live token values dropped whatever variable carries them.
	if want := []string{"xoxb-secret", "xapp-secret"}; !reflect.DeepEqual(got.Secrets, want) {
		t.Fatalf("Secrets = %q, want %q", got.Secrets, want)
	}
	// Env left nil so client.Start materialises and scrubs os.Environ().
	if got.Env != nil {
		t.Fatalf("Env = %q, want nil (client.Start owns the environment)", got.Env)
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

// The declared secrets must be exactly the two Slack credentials, so a
// provider key the agent legitimately needs survives the scrub
// client.Start runs. The agent's provider key is neither dropped by name
// (ANTHROPIC_API_KEY is not in SecretEnvNames) nor by value (its value is
// not in Secrets), so client.Start passes it through untouched.
func TestAgentClientConfigDoesNotOverScrub(t *testing.T) {
	const providerName, providerValue = "ANTHROPIC_API_KEY", "sk-ant-provider-key"
	c := &config.Config{BotToken: "xoxb-secret", AppToken: "xapp-secret"}
	got := c.AgentClientConfig(nil)

	for _, name := range got.SecretEnvNames {
		if name == providerName {
			t.Fatalf("%s declared in SecretEnvNames — the agent needs its provider key", providerName)
		}
	}
	for _, v := range got.Secrets {
		if v == providerValue {
			t.Fatalf("provider key value declared in Secrets — the agent needs it")
		}
	}
	// Sanity: exactly the two Slack credentials are declared.
	if want := []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "SLACK_USER_TOKEN"}; !reflect.DeepEqual(got.SecretEnvNames, want) {
		t.Fatalf("SecretEnvNames = %q, want only the Slack names %q", got.SecretEnvNames, want)
	}
}

// TestAgentClientConfigScrubsTheHarnessUserToken is a security
// regression test for a hole opened by human_author_user_ids.
//
// `slack-acp verify` needs a Slack USER token, and the deployment
// carries it in the same env file the supervisor exports — so the relay
// process holds it, and the spawned agent would inherit it. That is
// worse than it looks: the relay now deliberately reclassifies messages
// posted through OUR app on behalf of a named human as human-authored,
// so an agent holding that token could post as the named human and
// summon ITSELF. A reply → trigger → reply loop the blanket bot_id drop
// used to make impossible, plus a secret handed to a model-driven
// process that has no use for it.
//
// The relay never calls Slack as the user; only the harness does.
func TestAgentClientConfigScrubsTheHarnessUserToken(t *testing.T) {
	t.Setenv("SLACK_USER_TOKEN", "xoxp-harness-secret")
	c := &config.Config{AgentCmd: []string{"fir"}, BotToken: "xoxb-s", AppToken: "xapp-s"}

	got := c.AgentClientConfig(&bytes.Buffer{})

	if !slices.Contains(got.SecretEnvNames, "SLACK_USER_TOKEN") {
		t.Errorf("SLACK_USER_TOKEN must be scrubbed by name, got %q", got.SecretEnvNames)
	}
	// Also by value, so a token exported under a bespoke name is caught.
	if !slices.Contains(got.Secrets, "xoxp-harness-secret") {
		t.Errorf("the live user token value must be scrubbed too, got %d secrets", len(got.Secrets))
	}
}

// TestAgentClientConfigNeverDeclaresAnEmptySecret guards the common
// case where no user token is present: an empty string as a "secret"
// would be a substring of every environment value.
func TestAgentClientConfigNeverDeclaresAnEmptySecret(t *testing.T) {
	t.Setenv("SLACK_USER_TOKEN", "")
	c := &config.Config{AgentCmd: []string{"fir"}}

	for _, s := range c.AgentClientConfig(&bytes.Buffer{}).Secrets {
		if s == "" {
			t.Fatal("an empty secret would match every value in the environment")
		}
	}
}
