package config

import (
	"io"
	"strings"

	"github.com/kfet/acp-kit/client"
)

// slackSecretEnvNames are the environment variables `slack-acp init`
// writes and the supervisor units export. They authenticate the relay
// to Slack; the spawned ACP agent has no business holding them.
var slackSecretEnvNames = []string{
	"SLACK_BOT_TOKEN",
	"SLACK_APP_TOKEN",
}

// ScrubbedEnv returns environ with every Slack credential removed, for
// use as the spawned agent's environment.
//
// The agent is a general-purpose tool-using process driven, in ambient
// threads, by text from people who are not the operator. If it can read
// the bot token it can post as the bot, read channel history, and
// re-scope its own reach — none of which any prompt-level guard can
// take back. Withholding the secret is the only durable defence, and it
// costs nothing: the agent never needs to call Slack itself, because
// the relay owns that side of the wire.
//
// Two removal rules, because operators do not all use our names:
//
//   - by name — the variables `slack-acp init` writes (SLACK_BOT_TOKEN,
//     SLACK_APP_TOKEN);
//   - by value — any variable, whatever it is called, whose value is
//     one of the live tokens passed in. This catches a token exported
//     under a bespoke name, or duplicated into a second variable.
//
// Empty strings in secrets are ignored (a config-file deployment may
// legitimately have no token in the environment at all). The input
// slice is not modified.
//
// The result is always non-nil, and that is load-bearing rather than
// incidental. acp-kit's client.Config treats a nil Env as "inherit
// os.Environ()" (as does exec.Cmd); only a non-nil slice — empty or
// not — is taken literally. Returning nil would therefore not mean "no
// variables" but "hand the agent everything, tokens included", turning
// the scrub into a silent no-op in precisely the case where it removed
// the most. Keep the make() below; do not reduce it to a var
// declaration. TestScrubbedEnvNeverNil guards this.
func ScrubbedEnv(environ []string, secrets ...string) []string {
	drop := make(map[string]bool, len(slackSecretEnvNames))
	for _, n := range slackSecretEnvNames {
		drop[n] = true
	}
	values := make(map[string]bool, len(secrets))
	for _, s := range secrets {
		if s != "" {
			values[s] = true
		}
	}

	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if ok && (drop[name] || values[value]) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// AgentClientConfig assembles the acp-kit client.Config used to spawn
// the ACP agent, with the Slack credentials scrubbed from its
// environment.
//
// This lives in internal/ rather than in cmd/slack-acp/main.go on
// purpose. main.go is excluded from the coverage gate by .covignore
// (entry-point shims are bare assembly), so while the scrub itself was
// tested, the *call site* that applies it was not: deleting the Env
// line there would have silently restored full-environment
// inheritance — handing the agent the Slack tokens — with every test
// still green. Assembling the config here puts the wiring back under
// the 100% gate, where TestAgentClientConfigScrubsCredentials pins it.
//
// environ is normally os.Environ(); stderr is normally os.Stderr. Both
// are parameters so the assembly is testable without touching process
// state.
func (c *Config) AgentClientConfig(environ []string, stderr io.Writer) client.Config {
	return client.Config{
		Command: c.AgentCmd,
		Cwd:     c.StateDir,
		Env:     ScrubbedEnv(environ, c.BotToken, c.AppToken),
		Stderr:  stderr,
	}
}
