package config_test

import (
	"bytes"
	"reflect"
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
	if want := []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN"}; !reflect.DeepEqual(got.SecretEnvNames, want) {
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
	if want := []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN"}; !reflect.DeepEqual(got.SecretEnvNames, want) {
		t.Fatalf("SecretEnvNames = %q, want only the Slack names %q", got.SecretEnvNames, want)
	}
}
