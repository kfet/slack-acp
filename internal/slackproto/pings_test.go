package slackproto

import "testing"

// The literal-string blocklist this replaced missed the labelled form
// Slack itself emits, and posts go out with escape=false so Slack parses
// them. Every form must go.
func TestStripBroadcastPings(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"<!channel> hey <!here> all <!everyone>", "hey  all"},
		{"<!here|@here> ping", "ping"},
		{"<!channel|@channel>x", "x"},
		{"<!subteam^S012|@oncall> up", "up"},
		{"<!subteam^S012> up", "up"},
		{"a <@U1> b", "a <@U1> b"}, // ordinary user mentions survive
	} {
		if got := StripBroadcastPings(tc.in); got != tc.want {
			t.Errorf("StripBroadcastPings(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
