package slackproto

import "testing"

func TestStripMention(t *testing.T) {
	cases := []struct{ in, bot, want string }{
		{"<@U1> hello", "U1", "hello"},
		{"  <@U1>   hi there  ", "U1", "hi there"},
		{"hey <@U1> what about <@U1> this", "U1", "hey  what about  this"},
		{"plain text", "U1", "plain text"},
		{"<@U2> hi", "U1", "<@U2> hi"},
	}
	for _, c := range cases {
		if got := stripMention(c.in, c.bot); got != c.want {
			t.Errorf("stripMention(%q,%q)=%q want %q", c.in, c.bot, got, c.want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if firstNonEmpty("", "b") != "b" {
		t.Fail()
	}
	if firstNonEmpty("a", "b") != "a" {
		t.Fail()
	}
}
