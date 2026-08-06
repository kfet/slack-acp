package slackproto

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/slack-go/slack/slackevents"

	"github.com/kfet/slack-acp/internal/journal"
)

// TestMain diverts the ingest journal for the whole package. Every
// dispatch emits a record, which would otherwise bury real test output
// in JSON. Tests that care about the journal opt back in via
// captureJournal.
func TestMain(m *testing.M) {
	restore := journal.SetOutput(io.Discard)
	code := m.Run()
	restore()
	os.Exit(code)
}

// captureJournal collects the records emitted during a test.
func captureJournal(t *testing.T) func() []journal.Record {
	t.Helper()
	var buf bytes.Buffer
	t.Cleanup(journal.SetOutput(&buf))
	return func() []journal.Record {
		var recs []journal.Record
		for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
			if r, ok := journal.Parse(string(line)); ok {
				recs = append(recs, r)
			}
		}
		return recs
	}
}

// handlerFunc adapts a plain func to the Handler interface, for cases
// that only care about the journal record and not the delivered event.
type handlerFunc func(context.Context, Event)

func (f handlerFunc) Handle(ctx context.Context, ev Event) { f(ctx, ev) }

// TestIngestJournalRecordsEveryDecision is the contract test for the
// stream cmd/slack-acp-verify asserts on. Each case pins the exact
// (path, decision, reason) triple for one inbound shape — renaming any
// of these values breaks the verifier and any operator grep, so it is
// deliberately spelled out rather than derived.
func TestIngestJournalRecordsEveryDecision(t *testing.T) {
	const bot = "UBOT"

	cases := []struct {
		name     string
		inner    any
		path     journal.Path
		decision journal.Decision
		reason   string
	}{
		{
			name:     "human app_mention delivers",
			inner:    &slackevents.AppMentionEvent{User: "U1", Channel: "C1", TimeStamp: "1.0", Text: "<@UBOT> hi"},
			path:     journal.PathAppMention,
			decision: journal.DecisionDeliver,
			reason:   journal.ReasonMention,
		},
		{
			name:     "bot-authored app_mention hits the absolute guard",
			inner:    &slackevents.AppMentionEvent{User: bot, Channel: "C1", TimeStamp: "1.0", Text: "<@UBOT> hi"},
			path:     journal.PathAppMention,
			decision: journal.DecisionDrop,
			reason:   journal.ReasonBotAuthored,
		},
		{
			name:     "DM delivers",
			inner:    &slackevents.MessageEvent{User: "U1", Channel: "D1", ChannelType: "im", TimeStamp: "1.0", Text: "hi"},
			path:     journal.PathMessageIM,
			decision: journal.DecisionDeliver,
			reason:   journal.ReasonDM,
		},
		{
			name:     "edit / chat.update echo is a subtype drop",
			inner:    &slackevents.MessageEvent{User: "U1", Channel: "C1", TimeStamp: "1.0", Text: "hi", SubType: "message_changed"},
			path:     journal.PathMessage,
			decision: journal.DecisionDrop,
			reason:   journal.ReasonSubType,
		},
		{
			name:     "un-mentioned thread reply delivers as ambient",
			inner:    &slackevents.MessageEvent{User: "U1", Channel: "C1", TimeStamp: "2.0", ThreadTimeStamp: "1.0", Text: "and also"},
			path:     journal.PathMessage,
			decision: journal.DecisionDeliver,
			reason:   journal.ReasonAmbientThreadReply,
		},
		{
			name:     "top-level channel chatter is not a thread reply",
			inner:    &slackevents.MessageEvent{User: "U1", Channel: "C1", TimeStamp: "1.0", Text: "unrelated"},
			path:     journal.PathMessage,
			decision: journal.DecisionDrop,
			reason:   journal.ReasonNotThreadReply,
		},
		{
			name:     "mentioning thread reply defers to the app_mention copy",
			inner:    &slackevents.MessageEvent{User: "U1", Channel: "C1", TimeStamp: "2.0", ThreadTimeStamp: "1.0", Text: "<@UBOT> again"},
			path:     journal.PathMessage,
			decision: journal.DecisionDrop,
			reason:   journal.ReasonMentionDuplicate,
		},
		{
			name:     "bot echo without the sentinel is refused",
			inner:    &slackevents.MessageEvent{User: bot, Channel: "C1", TimeStamp: "2.0", ThreadTimeStamp: "1.0", Text: "my own reply"},
			path:     journal.PathMessage,
			decision: journal.DecisionDrop,
			reason:   journal.ReasonSelfDriveNotAccept,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := New("xoxb-x", "xapp-x", handlerFunc(func(context.Context, Event) {}))
			if err != nil {
				t.Fatal(err)
			}
			c.botUserID = bot
			records := captureJournal(t)

			c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
				Type:       slackevents.CallbackEvent,
				InnerEvent: slackevents.EventsAPIInnerEvent{Data: tc.inner},
			})

			recs := records()
			if len(recs) != 1 {
				t.Fatalf("want exactly 1 journal record, got %d: %+v", len(recs), recs)
			}
			got := recs[0]
			if got.Stage != journal.StageProto || got.Path != tc.path || got.Decision != tc.decision || got.Reason != tc.reason {
				t.Fatalf("got %+v, want stage=%s path=%s decision=%s reason=%s",
					got, journal.StageProto, tc.path, tc.decision, tc.reason)
			}
			if got.Channel == "" || got.TS == "" {
				t.Fatalf("record must always carry channel and ts: %+v", got)
			}
		})
	}
}

// TestIngestJournalSelfDrivePaths covers the two hatch-specific
// records, which need a client with the hatch armed.
func TestIngestJournalSelfDrivePaths(t *testing.T) {
	const bot = "UBOT"

	t.Run("accepted sentinel delivers on the self_drive path", func(t *testing.T) {
		c := newSelfDriveClient(t, handlerFunc(func(context.Context, Event) {}), testSentinel)
		c.botUserID = bot
		records := captureJournal(t)

		c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
			Type: slackevents.CallbackEvent,
			InnerEvent: slackevents.EventsAPIInnerEvent{Data: &slackevents.MessageEvent{
				User: bot, Channel: "C1", TimeStamp: "1.0", Text: testSentinel + " go",
			}},
		})

		recs := records()
		if len(recs) != 1 || recs[0].Path != journal.PathSelfDrive ||
			recs[0].Decision != journal.DecisionDeliver || recs[0].Reason != journal.ReasonSelfDrive {
			t.Fatalf("got %+v", recs)
		}
	})

	t.Run("echo of our own ts is refused", func(t *testing.T) {
		d := NewSelfDrive(testSentinel)
		d.RecordTS("1.0")
		c, err := New("xoxb-x", "xapp-x", handlerFunc(func(context.Context, Event) {}), WithSelfDrive(d))
		if err != nil {
			t.Fatal(err)
		}
		c.botUserID = bot
		records := captureJournal(t)

		c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
			Type: slackevents.CallbackEvent,
			InnerEvent: slackevents.EventsAPIInnerEvent{Data: &slackevents.MessageEvent{
				User: bot, Channel: "C1", TimeStamp: "1.0", Text: testSentinel + " go",
			}},
		})

		recs := records()
		if len(recs) != 1 || recs[0].Decision != journal.DecisionDrop || recs[0].Reason != journal.ReasonSelfPostedTS {
			t.Fatalf("got %+v", recs)
		}
	})
}

// TestAPIAuthoredMessagesAreRefusedUnlessTheAuthorIsNamed is the
// regression test for the finding that made this whole guard precise:
// Slack stamps the posting app's bot_id onto EVERY API message,
// including a chat.postMessage sent with a user (xoxp-) token on
// behalf of a real person. Ground truth from a live workspace:
//
//	"user":   "U9EA2KLTH"    <- the human
//	"bot_id": "B0BNE4AUS9L"  <- the app's own bot id, set anyway
//
// So bot_id means "sent through an app", not "sent by a robot".
// Conflating the two made the app_mention path impossible to exercise
// automatically, which is why a mention bug reached release.
func TestAPIAuthoredMessagesAreRefusedUnlessTheAuthorIsNamed(t *testing.T) {
	const (
		bot   = "UBOT"
		human = "U9EA2KLTH"
	)
	mention := &slackevents.AppMentionEvent{
		User: human, BotID: "B0BNE4AUS9L", Channel: "C1", TimeStamp: "1.0", Text: "<@UBOT> hi",
	}

	t.Run("refused by default", func(t *testing.T) {
		c, err := New("xoxb-x", "xapp-x", handlerFunc(func(context.Context, Event) {}))
		if err != nil {
			t.Fatal(err)
		}
		c.botUserID = bot
		records := captureJournal(t)
		c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
			Type:       slackevents.CallbackEvent,
			InnerEvent: slackevents.EventsAPIInnerEvent{Data: mention},
		})
		recs := records()
		if len(recs) != 1 || recs[0].Decision != journal.DecisionDrop || recs[0].Reason != journal.ReasonAPIAuthored {
			t.Fatalf("got %+v", recs)
		}
	})

	t.Run("admitted when the author is named", func(t *testing.T) {
		var got Event
		c, err := New("xoxb-x", "xapp-x", handlerFunc(func(_ context.Context, ev Event) { got = ev }),
			WithHumanAuthors(map[string]struct{}{human: {}}))
		if err != nil {
			t.Fatal(err)
		}
		c.botUserID = bot
		records := captureJournal(t)
		c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
			Type:       slackevents.CallbackEvent,
			InnerEvent: slackevents.EventsAPIInnerEvent{Data: mention},
		})
		recs := records()
		if len(recs) != 1 || recs[0].Decision != journal.DecisionDeliver || recs[0].Reason != journal.ReasonMention {
			t.Fatalf("got %+v", recs)
		}
		if !got.IsMention || got.UserID != human {
			t.Fatalf("delivered event wrong: %+v", got)
		}
	})
}

// TestNamingTheBotCannotOpenTheSelfLoop is the security test for the
// override. The self-authorship clause is evaluated FIRST and has no
// override, so an operator who lists the relay's own bot user id — by
// accident or otherwise — changes nothing. A reply → trigger → reply
// loop stays structurally impossible.
func TestNamingTheBotCannotOpenTheSelfLoop(t *testing.T) {
	const bot = "UBOT"
	for name, ev := range map[string]any{
		"app_mention": &slackevents.AppMentionEvent{
			User: bot, BotID: "B1", Channel: "C1", TimeStamp: "1.0", Text: "<@UBOT> loop",
		},
		"message": &slackevents.MessageEvent{
			User: bot, BotID: "B1", Channel: "C1", TimeStamp: "2.0", ThreadTimeStamp: "1.0", Text: "<@UBOT> loop",
		},
	} {
		t.Run(name, func(t *testing.T) {
			delivered := 0
			c, err := New("xoxb-x", "xapp-x",
				handlerFunc(func(context.Context, Event) { delivered++ }),
				// The operator has (wrongly) named the bot itself.
				WithHumanAuthors(map[string]struct{}{bot: {}}))
			if err != nil {
				t.Fatal(err)
			}
			c.botUserID = bot
			records := captureJournal(t)
			c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
				Type:       slackevents.CallbackEvent,
				InnerEvent: slackevents.EventsAPIInnerEvent{Data: ev},
			})
			if delivered != 0 {
				t.Fatal("the relay must never act on its own message, whatever the config says")
			}
			recs := records()
			if len(recs) != 1 || recs[0].Decision != journal.DecisionDrop {
				t.Fatalf("got %+v", recs)
			}
		})
	}
}

// TestAuthorlessMessagesAreAlwaysRefused covers webhooks and classic
// bots, which carry no user at all. No list entry can match "".
func TestAuthorlessMessagesAreAlwaysRefused(t *testing.T) {
	c, err := New("xoxb-x", "xapp-x", handlerFunc(func(context.Context, Event) {}),
		WithHumanAuthors(map[string]struct{}{"": {}}))
	if err != nil {
		t.Fatal(err)
	}
	c.botUserID = "UBOT"
	records := captureJournal(t)
	c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Data: &slackevents.AppMentionEvent{
			BotID: "B1", Channel: "C1", TimeStamp: "1.0", Text: "<@UBOT> hi",
		}},
	})
	recs := records()
	if len(recs) != 1 || recs[0].Reason != journal.ReasonBotAuthored {
		t.Fatalf("got %+v", recs)
	}
}

// TestNamedHumanOnTheMessagePathIsAmbientNotSelfDrive pins that the
// override lands the event on the *ambient* path, not the self-drive
// hatch: a named human's un-tagged thread reply must behave exactly
// like any other human's.
func TestNamedHumanOnTheMessagePathIsAmbientNotSelfDrive(t *testing.T) {
	const human = "U9EA2KLTH"
	var got Event
	c, err := New("xoxb-x", "xapp-x", handlerFunc(func(_ context.Context, ev Event) { got = ev }),
		WithHumanAuthors(map[string]struct{}{human: {}}))
	if err != nil {
		t.Fatal(err)
	}
	c.botUserID = "UBOT"
	records := captureJournal(t)
	c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Data: &slackevents.MessageEvent{
			User: human, BotID: "B1", Channel: "C1", TimeStamp: "2.0", ThreadTimeStamp: "1.0", Text: "follow-up",
		}},
	})
	if got.SelfDrive {
		t.Fatal("a named human is a human, not a self-drive event")
	}
	recs := records()
	if len(recs) != 1 || recs[0].Reason != journal.ReasonAmbientThreadReply {
		t.Fatalf("got %+v", recs)
	}
}
