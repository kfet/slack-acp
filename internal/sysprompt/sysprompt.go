// Package sysprompt builds the durable system-prompt text the relay
// injects into every ACP session so the agent knows its replies land in
// Slack and must use Slack's mrkdwn dialect rather than standard
// CommonMark.
//
// The relay passes the result to router.Config.SystemPrompt. Delivery
// path (session/new._meta blocks vs first-prompt inline prefix) is the
// router's concern; this package only owns the *content*.
package sysprompt

import "strings"

// Default returns the built-in Slack-formatting instructions. Operators
// can override via Config.SystemPrompt or extend via Build.
func Default() string { return defaultText }

// Build composes the final system prompt. If extra is empty, returns
// Default(). Otherwise concatenates Default() + extra so the Slack
// formatting contract always leads and an operator's additions follow.
func Build(extra string) string {
	extra = strings.TrimSpace(extra)
	if extra == "" {
		return defaultText
	}
	return defaultText + "\n\n" + extra
}

// Resolve picks the right prompt for operator config: empty string when
// disabled, otherwise Build(extra). Keeps the disable/extra wiring out
// of cmd/slack-acp/main.go (which is excluded from coverage).
func Resolve(extra string, disabled bool) string {
	if disabled {
		return ""
	}
	return Build(extra)
}

// defaultText tells the agent where its output lands and trusts it to
// know Slack's formatting conventions. We deliberately don't enumerate
// mrkdwn rules: the model already knows them, and a long rulebook
// fights with whatever the operator's own system prompt says.
const defaultText = `Your replies are posted into a Slack thread via the Slack Web API,
not rendered as Markdown in a terminal or web chat. Format
messages using Slack's mrkdwn conventions and keep them concise — they
are read in a chat pane. One streaming reply per user message; the
relay updates a single Slack message in place as you stream.`
