package slackproto

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/slack-go/slack/slackevents"

	"github.com/kfet/slack-acp/internal/journal"
)

const testSentinel = "!!drive!!"

func newSelfDriveClient(t *testing.T, h Handler, sentinel string) *Client {
	t.Helper()
	c, err := New("xoxb-x", "xapp-x", h, WithSelfDrive(NewSelfDrive(sentinel)))
	if err != nil {
		t.Fatal(err)
	}
	c.botUserID = "Ubot"
	return c
}

// ---- 3a: app_mention never accepts a bot-authored event ----

func TestAppMentionRejectsBotAuthoredAlways(t *testing.T) {
	// This guard has no exception. In particular the self-drive
	// sentinel must NOT open app_mention: the relay's own replies are
	// posted by this same bot, so a bot-authored app_mention that could
	// re-trigger is the exact shape of an infinite loop.
	cases := []struct {
		name string
		ev   *slackevents.AppMentionEvent
	}{
		{"bot_id set", &slackevents.AppMentionEvent{BotID: "B1", User: "U1", Channel: "C1", TimeStamp: "1.0", Text: "<@Ubot> hi"}},
		{"our own user id", &slackevents.AppMentionEvent{User: "Ubot", Channel: "C1", TimeStamp: "1.0", Text: "<@Ubot> hi"}},
		{"empty user", &slackevents.AppMentionEvent{User: "", Channel: "C1", TimeStamp: "1.0", Text: "<@Ubot> hi"}},
		{"edited message", &slackevents.AppMentionEvent{User: "U1", Channel: "C1", TimeStamp: "1.0", Text: "<@Ubot> hi", Edited: &slackevents.Edited{User: "U1", TimeStamp: "1.1"}}},
		{"bot with sentinel prefix", &slackevents.AppMentionEvent{BotID: "B1", User: "Ubot", Channel: "C1", TimeStamp: "1.0", Text: testSentinel + " <@Ubot> hi"}},
		{"our user id with sentinel prefix", &slackevents.AppMentionEvent{User: "Ubot", Channel: "C1", TimeStamp: "1.0", Text: testSentinel + " <@Ubot> hi"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Run with the hatch ENABLED — it must make no difference here.
			h := &stubHandler{}
			c := newSelfDriveClient(t, h, testSentinel)
			c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
				Type:       slackevents.CallbackEvent,
				InnerEvent: slackevents.EventsAPIInnerEvent{Data: tc.ev},
			})
			if got := h.seen(); len(got) != 0 {
				t.Fatalf("app_mention accepted a bot-authored event: %+v", got)
			}
		})
	}
}

func TestAppMentionStillAcceptsHumans(t *testing.T) {
	h := &stubHandler{}
	c := newSelfDriveClient(t, h, testSentinel)
	c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.AppMentionEvent{User: "U1", Channel: "C1", TimeStamp: "1.0", Text: "<@Ubot> hi"},
		},
	})
	got := h.seen()
	if len(got) != 1 || got[0].Text != "hi" || got[0].SelfDrive {
		t.Fatalf("got %+v", got)
	}
}

// ---- 3b: the hatch, on the MessageEvent path only ----

func TestSelfDriveHatchMatrix(t *testing.T) {
	cases := []struct {
		name         string
		sentinel     string
		ev           *slackevents.MessageEvent
		wantAccepted bool
		wantText     string
		wantThreadTS string
	}{
		{
			name:     "bot with sentinel prefix, top-level, starts a thread",
			sentinel: testSentinel,
			ev: &slackevents.MessageEvent{
				BotID: "B1", User: "Ubot", Channel: "C1", TimeStamp: "100.0",
				Text: testSentinel + " do the thing",
			},
			wantAccepted: true, wantText: "do the thing", wantThreadTS: "100.0",
		},
		{
			name:     "bot with sentinel prefix, thread reply",
			sentinel: testSentinel,
			ev: &slackevents.MessageEvent{
				BotID: "B1", User: "Ubot", Channel: "C1", TimeStamp: "101.0", ThreadTimeStamp: "100.0",
				Text: testSentinel + " follow up",
			},
			wantAccepted: true, wantText: "follow up", wantThreadTS: "100.0",
		},
		{
			name:     "sentinel also mentioning the bot is still accepted here",
			sentinel: testSentinel,
			ev: &slackevents.MessageEvent{
				BotID: "B1", User: "Ubot", Channel: "C1", TimeStamp: "100.0",
				Text: testSentinel + " <@Ubot> hello",
			},
			// The mentionsBot suppression must NOT apply to the hatch:
			// 3a guarantees the app_mention twin is dead, so dropping
			// here too would silently lose the message on both paths.
			wantAccepted: true, wantText: "hello", wantThreadTS: "100.0",
		},
		{
			name:     "bot without sentinel is dropped",
			sentinel: testSentinel,
			ev: &slackevents.MessageEvent{
				BotID: "B1", User: "Ubot", Channel: "C1", TimeStamp: "100.0", Text: "just a normal reply",
			},
		},
		{
			name:     "sentinel mid-text is not a prefix, dropped",
			sentinel: testSentinel,
			ev: &slackevents.MessageEvent{
				BotID: "B1", User: "Ubot", Channel: "C1", TimeStamp: "100.0",
				Text: "as you asked, " + testSentinel + " was the trigger",
			},
			// This is the realistic loop path: the agent echoing the
			// trigger back inside its reply.
		},
		{
			name:     "message_changed with sentinel is dropped",
			sentinel: testSentinel,
			ev: &slackevents.MessageEvent{
				BotID: "B1", User: "Ubot", Channel: "C1", TimeStamp: "100.0",
				SubType: "message_changed", Text: testSentinel + " edited",
			},
			// Streaming replies are chat.update edits; accepting these
			// would re-trigger on every throttled update.
		},
		{
			name:     "own user id without sentinel is dropped",
			sentinel: testSentinel,
			ev: &slackevents.MessageEvent{
				User: "Ubot", Channel: "C1", TimeStamp: "100.0", ThreadTimeStamp: "99.0", Text: "hi",
			},
		},
		{
			name:     "hatch disabled drops sentinel-prefixed bot message",
			sentinel: "",
			ev: &slackevents.MessageEvent{
				BotID: "B1", User: "Ubot", Channel: "C1", TimeStamp: "100.0",
				Text: testSentinel + " do the thing",
			},
			// Fail closed: empty sentinel means the hatch is OFF.
		},
		{
			name:     "empty user with sentinel, DM channel",
			sentinel: testSentinel,
			ev: &slackevents.MessageEvent{
				BotID: "B1", User: "Ubot", Channel: "D1", ChannelType: "im", TimeStamp: "100.0",
				Text: testSentinel + " dm drive",
			},
			wantAccepted: true, wantText: "dm drive", wantThreadTS: "100.0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &stubHandler{}
			c := newSelfDriveClient(t, h, tc.sentinel)
			c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
				Type:       slackevents.CallbackEvent,
				InnerEvent: slackevents.EventsAPIInnerEvent{Data: tc.ev},
			})
			got := h.seen()
			if !tc.wantAccepted {
				if len(got) != 0 {
					t.Fatalf("expected drop, got %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("expected 1 event, got %+v", got)
			}
			if !got[0].SelfDrive {
				t.Error("SelfDrive not set on hatch-accepted event")
			}
			if got[0].Text != tc.wantText {
				t.Errorf("Text = %q, want %q (sentinel must be stripped)", got[0].Text, tc.wantText)
			}
			if got[0].ThreadTS != tc.wantThreadTS {
				t.Errorf("ThreadTS = %q, want %q", got[0].ThreadTS, tc.wantThreadTS)
			}
			if strings.Contains(got[0].Text, tc.sentinel) {
				t.Errorf("sentinel leaked into forwarded text: %q", got[0].Text)
			}
		})
	}
}

func TestHumanMessagesUnaffectedByHatch(t *testing.T) {
	// The hatch must not change any existing human path.
	h := &stubHandler{}
	c := newSelfDriveClient(t, h, testSentinel)
	c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{
				User: "U1", Channel: "C1", TimeStamp: "101.0", ThreadTimeStamp: "100.0", Text: "ambient reply",
			},
		},
	})
	got := h.seen()
	if len(got) != 1 || got[0].SelfDrive || got[0].Text != "ambient reply" {
		t.Fatalf("got %+v", got)
	}
}

// ---- loop guard 2: self-posted ts suppression ----

func TestSelfPostedTSSuppression(t *testing.T) {
	h := &stubHandler{}
	d := NewSelfDrive(testSentinel)
	c, err := New("xoxb-x", "xapp-x", h, WithSelfDrive(d))
	if err != nil {
		t.Fatal(err)
	}
	c.botUserID = "Ubot"

	d.RecordTS("100.0")
	c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{
				BotID: "B1", User: "Ubot", Channel: "C1", TimeStamp: "100.0",
				Text: testSentinel + " echo of our own post",
			},
		},
	})
	if got := h.seen(); len(got) != 0 {
		t.Fatalf("self-posted ts should be suppressed, got %+v", got)
	}
}

func TestSelfDriveTSRingIsBounded(t *testing.T) {
	d := NewSelfDrive(testSentinel)
	// Overflow the ring; the oldest entries must be evicted rather than
	// growing without bound in a long-running process.
	for i := 0; i < selfTSCapacity*2; i++ {
		d.RecordTS(tsFor(i))
	}
	if got := d.Len(); got != selfTSCapacity {
		t.Fatalf("ring holds %d entries, want cap %d", got, selfTSCapacity)
	}
	if d.SeenTS(tsFor(0)) {
		t.Error("oldest ts should have been evicted")
	}
	if !d.SeenTS(tsFor(selfTSCapacity*2 - 1)) {
		t.Error("newest ts should be remembered")
	}
	// Re-recording an existing ts must not double-count.
	before := d.Len()
	d.RecordTS(tsFor(selfTSCapacity*2 - 1))
	if d.Len() != before {
		t.Fatalf("duplicate RecordTS changed size: %d → %d", before, d.Len())
	}
}

func tsFor(i int) string {
	return strings.Repeat("0", 1) + "." + string(rune('a'+i%26)) + strings.Repeat("x", i/26+1)
}

// ---- loop guard 1: outbound scrub ----

func TestSelfDriveScrub(t *testing.T) {
	d := NewSelfDrive(testSentinel)
	cases := []struct{ in, want string }{
		{testSentinel + " leading", "[sentinel] leading"},
		{"trailing " + testSentinel, "trailing [sentinel]"},
		{"two " + testSentinel + " and " + testSentinel, "two [sentinel] and [sentinel]"},
		{"clean text", "clean text"},
	}
	for _, tc := range cases {
		if got := d.Scrub(tc.in); got != tc.want {
			t.Errorf("Scrub(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if strings.Contains(d.Scrub(tc.in), testSentinel) {
			t.Errorf("Scrub left the sentinel in %q", tc.in)
		}
	}
}

func TestSelfDriveNilIsSafeAndClosed(t *testing.T) {
	// Hatch off (nil) must be inert everywhere, never panic, and never
	// accept anything.
	var d *SelfDrive
	if d.Enabled() {
		t.Error("nil SelfDrive reports enabled")
	}
	if _, ok := d.Accept(testSentinel + " x"); ok {
		t.Error("nil SelfDrive accepted a sentinel message")
	}
	if got := d.Scrub("text " + testSentinel); got != "text "+testSentinel {
		t.Errorf("nil Scrub mutated text: %q", got)
	}
	if d.SeenTS("1.0") {
		t.Error("nil SeenTS returned true")
	}
	d.RecordTS("1.0") // must not panic
	if d.Len() != 0 {
		t.Error("nil Len non-zero")
	}

	// An empty sentinel is likewise off.
	e := NewSelfDrive("")
	if e.Enabled() {
		t.Error("empty sentinel reports enabled")
	}
	if _, ok := e.Accept("anything"); ok {
		t.Error("empty sentinel accepted")
	}
	if got := e.Scrub("untouched"); got != "untouched" {
		t.Errorf("empty-sentinel Scrub mutated text: %q", got)
	}
}

func TestSelfDriveAcceptStripsAndTrims(t *testing.T) {
	d := NewSelfDrive(testSentinel)
	if got, ok := d.Accept(testSentinel + "   spaced   "); !ok || got != "spaced" {
		t.Fatalf("Accept = %q,%v", got, ok)
	}
	if got, ok := d.Accept(testSentinel); !ok || got != "" {
		t.Fatalf("bare sentinel: Accept = %q,%v", got, ok)
	}
	if _, ok := d.Accept(" " + testSentinel + " leading space"); ok {
		t.Error("prefix must be anchored at position 0")
	}
}

// ---- loop guard 1 end-to-end: nothing we post carries the sentinel ----

func TestOutboundNeverPostsTheSentinel(t *testing.T) {
	// The agent echoing its trigger is the realistic loop path, so
	// drive every outbound route — placeholder post, placeholder
	// update, streamed content, and the final Close flush — with text
	// that contains the sentinel, and assert none of it reaches Slack.
	fs := newFakeSlackSrv()
	defer fs.close()

	d := NewSelfDrive(testSentinel)
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	s.SetSelfDrive(d)
	s.minInterval = 0
	ctx := context.Background()

	if err := s.Start(ctx, testSentinel+" thinking"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdatePlaceholder(ctx, "still "+testSentinel+" working"); err != nil {
		t.Fatal(err)
	}
	s.FirstChunk()
	if err := s.Append(ctx, "you asked "+testSentinel+" and here it is"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx, "\n"+testSentinel); err != nil {
		t.Fatal(err)
	}

	fs.mu.Lock()
	bodies := append([]string(nil), fs.bodies...)
	fs.mu.Unlock()

	if len(bodies) == 0 {
		t.Fatal("no outbound bodies captured")
	}
	for i, b := range bodies {
		if strings.Contains(b, testSentinel) {
			t.Errorf("outbound body %d leaked the sentinel: %q", i, b)
		}
	}

	// And the ts values we wrote are remembered, so Slack's echo of
	// our own message is skipped on the way back in.
	if !d.SeenTS("1.0") {
		t.Error("self-posted ts not recorded")
	}
}

func TestOutboundScrubInertWhenHatchOff(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	s.SetSelfDrive(nil) // hatch off
	if err := s.Append(context.Background(), "plain "+testSentinel+" text"); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.bodies) != 1 || !strings.Contains(fs.bodies[0], testSentinel) {
		t.Fatalf("hatch-off must not rewrite bodies, got %q", fs.bodies)
	}
}

// ---- ergonomics: diagnose a non-prefix sentinel loudly ----

func TestSelfDriveWarnsWhenSentinelIsNotAPrefix(t *testing.T) {
	// The failure this guards against was invisible: the operator's
	// sentinel arrived Slack-escaped, HasPrefix never matched, and the
	// hatch did nothing with no output at any level. A visible warning
	// on "contains but does not start with" turns that into a
	// one-second diagnosis.
	var buf bytes.Buffer
	restore := captureWarnings(&buf)
	defer restore()

	h := &stubHandler{}
	c := newSelfDriveClient(t, h, testSentinel)
	c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{
				BotID: "B1", User: "Ubot", Channel: "C1", TimeStamp: "100.0",
				Text: "you asked " + testSentinel + " so here it is",
			},
		},
	})
	if got := h.seen(); len(got) != 0 {
		t.Fatalf("mid-text sentinel must still be dropped, got %+v", got)
	}
	out := buf.String()
	if !strings.Contains(strings.ToLower(out), "prefix") {
		t.Errorf("warning should explain the prefix requirement, got: %q", out)
	}
	if !strings.Contains(out, "C1") || !strings.Contains(out, "100.0") {
		t.Errorf("warning should identify channel and ts, got: %q", out)
	}
}

func TestSelfDriveNoWarningWhenSentinelAbsentOrHatchOff(t *testing.T) {
	cases := []struct {
		name     string
		sentinel string
		text     string
	}{
		{"no sentinel anywhere", testSentinel, "an ordinary bot reply"},
		{"hatch off, sentinel present", "", "contains " + testSentinel + " but hatch is off"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			restore := captureWarnings(&buf)
			defer restore()

			h := &stubHandler{}
			c := newSelfDriveClient(t, h, tc.sentinel)
			c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
				Type: slackevents.CallbackEvent,
				InnerEvent: slackevents.EventsAPIInnerEvent{
					Data: &slackevents.MessageEvent{
						BotID: "B1", User: "Ubot", Channel: "C1", TimeStamp: "100.0", Text: tc.text,
					},
				},
			})
			if buf.Len() != 0 {
				t.Errorf("unexpected warning: %q", buf.String())
			}
		})
	}
}

func TestSelfDriveAcceptedPrefixDoesNotWarn(t *testing.T) {
	var buf bytes.Buffer
	restore := captureWarnings(&buf)
	defer restore()

	h := &stubHandler{}
	c := newSelfDriveClient(t, h, testSentinel)
	c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{
				BotID: "B1", User: "Ubot", Channel: "C1", TimeStamp: "100.0",
				Text: testSentinel + " go",
			},
		},
	})
	if len(h.seen()) != 1 {
		t.Fatal("prefix message should have been accepted")
	}
	if buf.Len() != 0 {
		t.Errorf("accepted message must not warn: %q", buf.String())
	}
}

func TestSelfDriveContainsButNotPrefix(t *testing.T) {
	d := NewSelfDrive(testSentinel)
	if !d.containsButNotPrefix("mid " + testSentinel + " text") {
		t.Error("mid-text sentinel not detected")
	}
	if d.containsButNotPrefix(testSentinel + " leading") {
		t.Error("prefix must not be reported as mid-text")
	}
	if d.containsButNotPrefix("no sentinel here") {
		t.Error("false positive on clean text")
	}
	var off *SelfDrive
	if off.containsButNotPrefix("mid " + testSentinel + " text") {
		t.Error("disabled hatch must not report anything")
	}
}

// captureWarnings redirects the standard logger into buf, returning a
// restore func. Used to assert on operator-visible output.
//
// The ingest journal is diverted to io.Discard for the duration: it
// shares the standard logger in production (journald wants one stream)
// but it is a *separate* stream conceptually, and these tests assert
// on the presence/absence of operator warnings only. Without the
// diversion every warning assertion would also have to know the
// journal's line format.
func captureWarnings(buf *bytes.Buffer) func() {
	prevOut, prevFlags, prevPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(buf)
	log.SetFlags(0)
	restoreJournal := journal.SetOutput(io.Discard)
	return func() {
		restoreJournal()
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	}
}
