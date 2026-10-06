package slackproto

import (
	"regexp"
	"strings"
)

// broadcastPing matches every form of Slack's channel-wide notification
// escape — bare (<!here>), labelled (<!here|@here>, which is the form
// Slack itself emits and happily re-parses), and user-group pings
// (<!subteam^S012|@oncall>).
//
// A literal-string blocklist looked sufficient and was not: posts go out
// with escape=false, so Slack parses the text, and the labelled form
// sailed straight through. An agent has no business emitting any of
// these, and "post @channel in #general saying…" is exactly the abuse
// this blocks. Stripped rather than rejected so a benign message still
// gets through.
var broadcastPing = regexp.MustCompile(`<!(?:here|channel|everyone|subteam\^[^>|]*)(?:\|[^>]*)?>`)

// StripBroadcastPings removes @channel/@here/@everyone and user-group
// escapes in every syntactic form Slack accepts. Applied to everything
// the agent can make the relay post outside its own reply: slack_post
// and branch openings.
func StripBroadcastPings(text string) string {
	return strings.TrimSpace(broadcastPing.ReplaceAllString(text, ""))
}
