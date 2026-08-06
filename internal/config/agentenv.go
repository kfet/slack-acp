package config

import (
	"io"
	"os"

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
//
// SLACK_USER_TOKEN belongs here even though the relay never reads it.
// `slack-acp verify` needs it, and the deployment carries it in the same
// env file the supervisor exports — so it lands in the relay's
// environment and would be inherited by the agent. That is sharper than
// it looks now that human_author_user_ids exists: a message posted with
// that token, through this app, on behalf of a named human is
// deliberately reclassified as human-authored, so an agent holding it
// could post as that human and summon ITSELF — a reply → trigger → reply
// loop, and one the blanket bot_id refusal used to make impossible.
var slackSecretEnvNames = []string{
	"SLACK_BOT_TOKEN",
	"SLACK_APP_TOKEN",
	"SLACK_USER_TOKEN",
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
		Secrets:        c.secretValues(),
		Stderr:         stderr,
	}
}

// secretValues collects the live token values to strip by value,
// whatever variable carries them.
//
// Empty values are dropped, and that is not cosmetic: tokens usually
// arrive via the environment rather than the config file, so
// c.BotToken/c.AppToken are routinely "" — and an empty string is a
// substring of every value in the environment. Declaring one invites a
// scrub that either matches everything or is silently skipped,
// depending on the implementation on the other side. Say only what we
// actually mean.
//
// The user token is read from the environment because the relay
// deliberately does not carry it in Config — it is harness-only — but
// it still must not reach the agent. It is never logged.
func (c *Config) secretValues() []string {
	var out []string
	for _, v := range []string{c.BotToken, c.AppToken, os.Getenv("SLACK_USER_TOKEN")} {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
