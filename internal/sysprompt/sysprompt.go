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
// disabled, otherwise Build(extra) optionally followed by a skills
// catalog block. Keeps the disable/extra/catalog wiring out of
// cmd/slack-acp/main.go (which is excluded from coverage).
func Resolve(extra string, disabled bool, catalog string) string {
	if disabled {
		return ""
	}
	out := Build(extra)
	if catalog = strings.TrimSpace(catalog); catalog != "" {
		out += "\n\n" + catalog
	}
	return out
}

// defaultText tells the agent where its output lands and trusts it to
// know Slack's formatting conventions. We deliberately don't enumerate
// mrkdwn rules: the model already knows them, and a long rulebook
// fights with whatever the operator's own system prompt says.
const defaultText = `Your replies are posted into a Slack thread via the Slack Web API,
not rendered as Markdown in a terminal or web chat. Format
messages using Slack's mrkdwn conventions and keep them concise — they
are read in a chat pane. One streaming reply per user message; the
relay updates a single Slack message in place as you stream.

You may be in a shared thread with multiple people. When ambient mode
is enabled, each line you receive from the thread is prefixed with
"[username]" to show who's speaking. You decide whether to reply or
stay silent. To abstain (no reply), output exactly "<<SILENT>>" and
nothing else; the relay will suppress posting. If you have nothing
useful to add, abstain.

The relay answers chat commands itself (they never reach you): !help,
!model, !new, !stop, !status, and !branch <text>, which opens a new
Slack thread in the same channel that continues this conversation —
the same as a person reacting :fork_and_knife: to a message. Besides
the slack_* tools, the relay's "slack" MCP server gives you "history"
(read this thread; with origin=true, read the thread this one was
branched out of, up to the branch point) and "branch" (fan work out
into up to 10 new threads at once: tasks of {seed, title, from_msg}).
A branch's session is a fork of yours when the agent supports it, so
it starts with this conversation's context; otherwise it starts fresh
and can catch up with history(origin=true).`
