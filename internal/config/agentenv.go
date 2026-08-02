package config

import (
	"io"

	"github.com/kfet/acp-kit/client"
)

// slackSecretEnvNames are the environment variables `slack-acp init`
// writes and the supervisor units export. They authenticate the relay
// to Slack; the spawned ACP agent has no business holding them.
//
// The threat model is Slack-specific and worth keeping in view: the
// agent is a general-purpose tool-using process driven, in ambient
// threads, by text from people who are not the operator. If it can read
// the bot token it can post as the bot, read channel history, and
// re-scope its own reach — none of which any prompt-level guard can take
// back. Withholding the secret is the only durable defence, and it costs
// nothing: the agent never needs to call Slack itself, because the relay
// owns that side of the wire.
var slackSecretEnvNames = []string{
	"SLACK_BOT_TOKEN",
	"SLACK_APP_TOKEN",
}

// AgentClientConfig assembles the acp-kit client.Config used to spawn
// the ACP agent, declaring the Slack credentials as secrets so
// client.Start scrubs them from the child's environment.
//
// Two removal rules, because operators do not all use our names:
// SecretEnvNames drops the variables `slack-acp init` writes, and
// Secrets drops the live token values whatever variable carries them
// (a token exported under a bespoke name, or copied into a second
// variable). The agent still inherits everything else — including its
// own provider keys (ANTHROPIC_API_KEY etc.), which it legitimately
// needs.
//
// The scrub, and the nil-Env footgun it closes (a nil Env means
// "inherit os.Environ()" to both client.Config and exec.Cmd, so failing
// to materialise and filter it would hand the agent the full
// environment, tokens included), now live in acp-kit's client.Start —
// applied unconditionally inside Start rather than at a call site that
// could be forgotten. See acp-kit client.Config's Env/SecretEnvNames/
// Secrets documentation for the details.
//
// This assembly lives in internal/ rather than in cmd/slack-acp/main.go
// on purpose: main.go is excluded from the coverage gate by .covignore
// (entry-point shims are bare assembly), so keeping the wiring here puts
// it under the 100% gate where TestAgentClientConfigDeclaresCredentials
// pins that the right secrets are declared.
//
// stderr is normally os.Stderr; it is a parameter so the assembly is
// testable without touching process state. Env is left nil so
// client.Start materialises and scrubs os.Environ() itself.
func (c *Config) AgentClientConfig(stderr io.Writer) client.Config {
	return client.Config{
		Command:        c.AgentCmd,
		Cwd:            c.StateDir,
		SecretEnvNames: slackSecretEnvNames,
		Secrets:        []string{c.BotToken, c.AppToken},
		Stderr:         stderr,
	}
}
