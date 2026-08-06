package slackproto

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/kfet/slack-acp/internal/journal"
	"github.com/kfet/slack-acp/internal/ratelimit"
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
			}, "")

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
		}, "")

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
		}, "")

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
		}, "")
		recs := records()
		if len(recs) != 1 || recs[0].Decision != journal.DecisionDrop || recs[0].Reason != journal.ReasonAPIAuthored {
			t.Fatalf("got %+v", recs)
		}
	})

	t.Run("admitted when the author is named AND our app posted it", func(t *testing.T) {
		var got Event
		c, err := New("xoxb-x", "xapp-x", handlerFunc(func(_ context.Context, ev Event) { got = ev }),
			WithHumanAuthors(map[string]struct{}{human: {}}))
		if err != nil {
			t.Fatal(err)
		}
		c.botUserID = bot
		armHumanAuthors(c)
		records := captureJournal(t)
		c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
			Type:       slackevents.CallbackEvent,
			InnerEvent: slackevents.EventsAPIInnerEvent{Data: mention},
		}, ourAppID)
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
			}, "")
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
	}, "")
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
	armHumanAuthors(c)
	records := captureJournal(t)
	c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Data: &slackevents.MessageEvent{
			User: human, BotID: "B1", Channel: "C1", TimeStamp: "2.0", ThreadTimeStamp: "1.0", Text: "follow-up",
		}},
	}, ourAppID)
	if got.SelfDrive {
		t.Fatal("a named human is a human, not a self-drive event")
	}
	recs := records()
	if len(recs) != 1 || recs[0].Reason != journal.ReasonAmbientThreadReply {
		t.Fatalf("got %+v", recs)
	}
}

// ourAppID stands in for the app id Run() learns from bots.info.
const ourAppID = "A0OURAPP"

// armHumanAuthors completes what Run() would do for a client whose
// reclassification is configured: learn our own app id and arm the
// rate cap. Tests construct Clients directly, so without this the
// reclassification correctly stays shut.
func armHumanAuthors(c *Client) {
	c.appID = ourAppID
	c.humanAuthorRate = ratelimit.New(0, defaultHumanAuthorPerMinute, nil)
}

// TestHumanAuthorRequiresOurOwnApp closes the widest hole in the
// reclassification: naming a human must NOT hand trust to every app
// that person ever installed. A third-party app posting as the named
// user carries a different app_id and stays refused.
//
// This is not theoretical — measured against a live workspace, an
// app's user-token surface gets its own bot_id (B0BNE4AUS9L) distinct
// from its bot-token bot_id (B0B3VCV278U) while BOTH carry the same
// app_id. So app_id is the only field that identifies the poster, and
// bot_id equality would have been the wrong test.
func TestHumanAuthorRequiresOurOwnApp(t *testing.T) {
	const human = "U9EA2KLTH"
	mention := &slackevents.AppMentionEvent{
		User: human, BotID: "B_OTHER", Channel: "C1", TimeStamp: "1.0", Text: "<@UBOT> hi",
	}
	for name, appID := range map[string]string{
		"a third-party app posting as the named user": "A0SOMEONEELSE",
		"an envelope with no app id at all":           "",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := New("xoxb-x", "xapp-x", handlerFunc(func(context.Context, Event) {}),
				WithHumanAuthors(map[string]struct{}{human: {}}))
			if err != nil {
				t.Fatal(err)
			}
			c.botUserID = "UBOT"
			armHumanAuthors(c)
			records := captureJournal(t)
			c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
				Type:       slackevents.CallbackEvent,
				InnerEvent: slackevents.EventsAPIInnerEvent{Data: mention},
			}, appID)
			recs := records()
			if len(recs) != 1 || recs[0].Reason != journal.ReasonForeignApp {
				t.Fatalf("got %+v", recs)
			}
		})
	}
}

// TestHumanAuthorFailsClosedWithoutOurAppID: if bots.info failed at
// startup we never learned our own app id. The reclassification must
// then refuse everything rather than match on an empty string.
func TestHumanAuthorFailsClosedWithoutOurAppID(t *testing.T) {
	const human = "U9EA2KLTH"
	c, err := New("xoxb-x", "xapp-x", handlerFunc(func(context.Context, Event) {}),
		WithHumanAuthors(map[string]struct{}{human: {}}))
	if err != nil {
		t.Fatal(err)
	}
	c.botUserID = "UBOT"
	c.humanAuthorRate = ratelimit.New(0, defaultHumanAuthorPerMinute, nil)
	// c.appID deliberately left empty — bots.info did not answer.
	records := captureJournal(t)
	c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Data: &slackevents.AppMentionEvent{
			User: human, BotID: "B1", Channel: "C1", TimeStamp: "1.0", Text: "<@UBOT> hi",
		}},
	}, "")
	recs := records()
	if len(recs) != 1 || recs[0].Reason != journal.ReasonForeignApp {
		t.Fatalf("an unknown own-app-id must fail closed, got %+v", recs)
	}
}

// TestHumanAuthorIsRateCapped pins the loop backstop. If every other
// guard somehow failed, this is what bounds the damage to a handful of
// wasted prompts instead of a runaway spiral.
func TestHumanAuthorIsRateCapped(t *testing.T) {
	const human = "U9EA2KLTH"
	delivered := 0
	c, err := New("xoxb-x", "xapp-x", handlerFunc(func(context.Context, Event) { delivered++ }),
		WithHumanAuthors(map[string]struct{}{human: {}}), WithHumanAuthorRate(2))
	if err != nil {
		t.Fatal(err)
	}
	c.botUserID = "UBOT"
	c.appID = ourAppID
	frozen := time.Now()
	c.humanAuthorRate = ratelimit.New(2, defaultHumanAuthorPerMinute, func() time.Time { return frozen })

	records := captureJournal(t)
	for i := 0; i < 4; i++ {
		c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
			Type: slackevents.CallbackEvent,
			InnerEvent: slackevents.EventsAPIInnerEvent{Data: &slackevents.AppMentionEvent{
				User: human, BotID: "B1", Channel: "C1", TimeStamp: fmt.Sprintf("%d.0", i), Text: "<@UBOT> hi",
			}},
		}, ourAppID)
	}
	if delivered != 2 {
		t.Fatalf("cap of 2 must admit exactly 2, got %d", delivered)
	}
	capped := 0
	for _, r := range records() {
		if r.Reason == journal.ReasonHumanAuthorRateCap {
			capped++
		}
	}
	if capped != 2 {
		t.Fatalf("the two refusals must be journalled as rate-capped, got %d", capped)
	}
}

// TestSelfAuthorshipBeatsEveryOtherGate is the security test the
// advisor asked for explicitly: the relay's OWN reply must be refused
// even when the reclassification list is populated AND the downstream
// user allowlist is populated. Clause 1 is evaluated first, in
// slackproto, which runs strictly before the handler's allowlist — so
// no configuration anywhere can reach it.
func TestSelfAuthorshipBeatsEveryOtherGate(t *testing.T) {
	const bot = "UBOT"
	for name, ev := range map[string]any{
		"app_mention": &slackevents.AppMentionEvent{
			User: bot, BotID: "B1", Channel: "C1", TimeStamp: "1.0", Text: "<@UBOT> loop",
		},
		"message": &slackevents.MessageEvent{
			User: bot, BotID: "B1", Channel: "C1", TimeStamp: "2.0", ThreadTimeStamp: "1.0", Text: "<@UBOT> loop",
		},
		"edited app_mention": &slackevents.AppMentionEvent{
			User: "UHUMAN", BotID: "", Channel: "C1", TimeStamp: "3.0", Text: "<@UBOT> loop",
			Edited: &slackevents.Edited{User: "UHUMAN", TimeStamp: "3.1"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			delivered := 0
			c, err := New("xoxb-x", "xapp-x",
				handlerFunc(func(context.Context, Event) { delivered++ }),
				// Every override switched ON at once, including the
				// bot's own id in the human list.
				WithHumanAuthors(map[string]struct{}{bot: {}, "UHUMAN": {}}),
				WithSelfDrive(NewSelfDrive(testSentinel)))
			if err != nil {
				t.Fatal(err)
			}
			c.botUserID = bot
			armHumanAuthors(c)
			records := captureJournal(t)
			c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
				Type:       slackevents.CallbackEvent,
				InnerEvent: slackevents.EventsAPIInnerEvent{Data: ev},
			}, ourAppID)
			if delivered != 0 {
				t.Fatal("no configuration may let the relay act on its own message")
			}
			recs := records()
			if len(recs) != 1 || recs[0].Decision != journal.DecisionDrop {
				t.Fatalf("got %+v", recs)
			}
		})
	}
}

// TestAppIDOfRawEnvelope covers the raw-payload extraction, which
// exists because slack-go does not surface app_id on the typed event
// structs even though Slack delivers it.
func TestAppIDOfRawEnvelope(t *testing.T) {
	if got := appIDOf(nil); got != "" {
		t.Errorf("nil request: got %q", got)
	}
	if got := appIDOf(&socketmode.Request{Payload: []byte(`{"event":{"app_id":"A0OURAPP"}}`)}); got != ourAppID {
		t.Errorf("got %q", got)
	}
	if got := appIDOf(&socketmode.Request{Payload: []byte(`{not json`)}); got != "" {
		t.Errorf("unparseable payload must fail closed, got %q", got)
	}
	if got := appIDOf(&socketmode.Request{Payload: []byte(`{"event":{}}`)}); got != "" {
		t.Errorf("absent app_id: got %q", got)
	}
}

// runWithFakeSlack drives Run against a fake Web API until socketmode
// gives up, so the startup path (auth.test → bots.info) is exercised
// for real rather than by poking fields.
func runWithFakeSlack(t *testing.T, botsInfo string, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"user":"botname","user_id":"Ubot","team":"T","team_id":"T1","bot_id":"B_OURS"}`))
		case "/bots.info":
			_, _ = w.Write([]byte(botsInfo))
		case "/apps.connections.open":
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	api := slack.New("xoxb-x", slack.OptionAPIURL(srv.URL+"/"), slack.OptionAppLevelToken("xapp-x"))
	c := &Client{api: api, sm: socketmode.New(api), handler: &stubHandler{}}
	for _, o := range opts {
		o(c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Run(ctx); err == nil {
		t.Fatal("expected socketmode to fail so Run returns")
	}
	return c
}

// TestRunLearnsOurOwnAppID pins the startup half of the app_id
// narrowing: the relay must discover which app it IS before it can
// require that a reclassified message came from that app.
func TestRunLearnsOurOwnAppID(t *testing.T) {
	c := runWithFakeSlack(t, `{"ok":true,"bot":{"id":"B_OURS","app_id":"A0OURAPP","user_id":"Ubot"}}`,
		WithHumanAuthors(map[string]struct{}{"U9EA2KLTH": {}}))
	if c.appID != ourAppID {
		t.Fatalf("app id not learned: %q", c.appID)
	}
	if c.humanAuthorRate == nil {
		t.Fatal("the rate cap must be armed alongside")
	}
}

// TestRunFailsClosedWhenBotsInfoFails: if we cannot learn our own app
// id, the reclassification must be INERT rather than matching
// anything. A guard that cannot verify its precondition must refuse.
func TestRunFailsClosedWhenBotsInfoFails(t *testing.T) {
	c := runWithFakeSlack(t, `{"ok":false,"error":"missing_scope"}`,
		WithHumanAuthors(map[string]struct{}{"U9EA2KLTH": {}}))
	if c.appID != "" {
		t.Fatalf("app id must stay empty: %q", c.appID)
	}
	if got := c.refuseAuthor("U9EA2KLTH", "B1", "A0OURAPP", false); got != journal.ReasonForeignApp {
		t.Fatalf("must refuse with no known app id, got %q", got)
	}
}

// TestRunSkipsBotsInfoWhenNotConfigured: the default deployment must
// not make an extra API call (or need the scope) for a feature it is
// not using.
func TestRunSkipsBotsInfoWhenNotConfigured(t *testing.T) {
	c := runWithFakeSlack(t, `{"ok":false,"error":"should_not_be_called"}`)
	if c.appID != "" || c.humanAuthorRate != nil {
		t.Fatalf("nothing should have been armed: appID=%q rate=%v", c.appID, c.humanAuthorRate)
	}
}

func TestHumanAuthorPerMinuteOrDefault(t *testing.T) {
	c := &Client{}
	if got := c.humanAuthorPerMinuteOrDefault(); got != defaultHumanAuthorPerMinute {
		t.Errorf("got %d", got)
	}
	c.humanAuthorPerMinute = 3
	if got := c.humanAuthorPerMinuteOrDefault(); got != 3 {
		t.Errorf("got %d", got)
	}
}

func TestSortedKeysIsStable(t *testing.T) {
	got := sortedKeys(map[string]struct{}{"UB": {}, "UA": {}, "UC": {}})
	if len(got) != 3 || got[0] != "UA" || got[1] != "UB" || got[2] != "UC" {
		t.Fatalf("got %v", got)
	}
	if len(sortedKeys(nil)) != 0 {
		t.Fatal("nil set must render empty")
	}
}
