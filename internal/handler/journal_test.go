package handler

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/kfet/slack-acp/internal/journal"
	"github.com/kfet/slack-acp/internal/router"
	"github.com/kfet/slack-acp/internal/slackproto"
)

// TestMain diverts the ingest journal for the whole package; tests
// that assert on it opt back in via captureJournal.
func TestMain(m *testing.M) {
	restore := journal.SetOutput(io.Discard)
	code := m.Run()
	restore()
	os.Exit(code)
}

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

// TestHandlerJournalDecisions pins the handler-stage half of the
// ingest journal — the stage that applies the allowlists and the
// ambient known-thread gate. cmd/slack-acp-verify asserts on exactly
// these (path, decision, reason) triples, so they are spelled out
// literally rather than derived.
func TestHandlerJournalDecisions(t *testing.T) {
	newHandler := func(t *testing.T, cfg Config) (*Handler, *fakeSlack) {
		t.Helper()
		fa := newFakeAgent()
		fs := newFakeSlack()
		t.Cleanup(fs.close)
		cfg.Router = newTestRouter(t, fa)
		cfg.API = fs.client()
		cfg.PromptTimeout = 5 * time.Second
		return New(cfg), fs
	}

	t.Run("allowlist drop", func(t *testing.T) {
		h, _ := newHandler(t, Config{AllowedUserIDs: map[string]struct{}{"UOK": {}}})
		records := captureJournal(t)
		h.Handle(context.Background(), slackproto.Event{
			UserID: "UNOPE", BotUserID: "UBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0",
			Text: "hi", IsMention: true,
		})
		assertOneRecord(t, records(), journal.PathAppMention, journal.DecisionDrop, journal.ReasonAllowlist)
	})

	t.Run("empty text drop", func(t *testing.T) {
		h, _ := newHandler(t, Config{})
		records := captureJournal(t)
		h.Handle(context.Background(), slackproto.Event{
			UserID: "U1", BotUserID: "UBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0",
			Text: "   ", IsMention: true,
		})
		assertOneRecord(t, records(), journal.PathAppMention, journal.DecisionDrop, journal.ReasonEmptyText)
	})

	t.Run("ambient reply in an unknown thread drops", func(t *testing.T) {
		h, _ := newHandler(t, Config{Ambient: true})
		records := captureJournal(t)
		h.Handle(context.Background(), slackproto.Event{
			UserID: "U1", BotUserID: "UBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "2.0",
			Text: "an un-mentioned follow-up",
		})
		assertOneRecord(t, records(), journal.PathMessage, journal.DecisionDrop, journal.ReasonAmbientUnknownThrd)
	})

	t.Run("ambient reply in a known thread runs", func(t *testing.T) {
		h, _ := newHandler(t, Config{Ambient: true})
		key := router.ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
		// Summon into the thread first so the router knows it.
		if _, err := h.cfg.Router.GetOrCreate(context.Background(), key, nil); err != nil {
			t.Fatal(err)
		}
		records := captureJournal(t)
		h.Handle(context.Background(), slackproto.Event{
			UserID: "U1", BotUserID: "UBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "2.0",
			Text: "an un-mentioned follow-up",
		})
		waitForIdle(t, h)
		assertOneRecord(t, records(), journal.PathMessage, journal.DecisionRun, journal.ReasonPrompt)
	})

	t.Run("mention runs", func(t *testing.T) {
		h, _ := newHandler(t, Config{Ambient: true})
		records := captureJournal(t)
		h.Handle(context.Background(), slackproto.Event{
			UserID: "U1", BotUserID: "UBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0",
			Text: "hello", IsMention: true,
		})
		waitForIdle(t, h)
		assertOneRecord(t, records(), journal.PathAppMention, journal.DecisionRun, journal.ReasonPrompt)
	})

	t.Run("DM runs", func(t *testing.T) {
		h, _ := newHandler(t, Config{Ambient: true})
		records := captureJournal(t)
		h.Handle(context.Background(), slackproto.Event{
			UserID: "U1", BotUserID: "UBOT", ChannelID: "D1", ThreadTS: "1.0", TS: "1.0",
			Text: "hello", IsDM: true,
		})
		waitForIdle(t, h)
		assertOneRecord(t, records(), journal.PathMessageIM, journal.DecisionRun, journal.ReasonPrompt)
	})

	t.Run("self-drive runs", func(t *testing.T) {
		h, _ := newHandler(t, Config{Ambient: true, SelfDrive: slackproto.NewSelfDrive("!!drive!!"), SelfDrivePerMinute: 4})
		records := captureJournal(t)
		h.Handle(context.Background(), slackproto.Event{
			UserID: "UBOT", BotUserID: "UBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0",
			Text: "hello", SelfDrive: true,
		})
		waitForIdle(t, h)
		assertOneRecord(t, records(), journal.PathSelfDrive, journal.DecisionRun, journal.ReasonPrompt)
	})

	t.Run("self-drive rate cap drop", func(t *testing.T) {
		h, _ := newHandler(t, Config{Ambient: true, SelfDrive: slackproto.NewSelfDrive("!!drive!!"), SelfDrivePerMinute: 1})
		ev := slackproto.Event{
			UserID: "UBOT", BotUserID: "UBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0",
			Text: "hello", SelfDrive: true,
		}
		h.Handle(context.Background(), ev) // consumes the only token
		waitForIdle(t, h)

		records := captureJournal(t)
		ev.TS = "2.0"
		h.Handle(context.Background(), ev)
		waitForIdle(t, h)
		assertOneRecord(t, records(), journal.PathSelfDrive, journal.DecisionDrop, journal.ReasonSelfDriveRateCap)
	})
}

func assertOneRecord(t *testing.T, recs []journal.Record, path journal.Path, decision journal.Decision, reason string) {
	t.Helper()
	if len(recs) != 1 {
		t.Fatalf("want exactly 1 journal record, got %d: %+v", len(recs), recs)
	}
	got := recs[0]
	if got.Stage != journal.StageHandler || got.Path != path || got.Decision != decision || got.Reason != reason {
		t.Fatalf("got %+v, want stage=%s path=%s decision=%s reason=%s",
			got, journal.StageHandler, path, decision, reason)
	}
	if got.Channel == "" || got.TS == "" {
		t.Fatalf("record must always carry channel and ts: %+v", got)
	}
}

// TestSelfAuthoredIsRefusedEvenWithTheAllowlistPopulated pins the
// ordering the whole loop-safety argument rests on: the relay's own
// message is refused BEFORE allowed_user_ids is consulted, so no
// allowlist configuration can reorder or widen it — not even one that
// explicitly names the bot itself.
//
// slackproto already refuses this upstream; this is the defence-in-depth
// backstop, so the invariant survives a future ingest path that forgets.
func TestSelfAuthoredIsRefusedEvenWithTheAllowlistPopulated(t *testing.T) {
	for name, allowed := range map[string]map[string]struct{}{
		"no allowlist":                    nil,
		"allowlist naming a human":        {"UHUMAN": {}},
		"allowlist naming the bot itself": {"UBOT": {}},
	} {
		t.Run(name, func(t *testing.T) {
			fa := newFakeAgent()
			fs := newFakeSlack()
			t.Cleanup(fs.close)
			h := New(Config{
				Router: newTestRouter(t, fa), API: fs.client(),
				PromptTimeout: 5 * time.Second, Ambient: true,
				AllowedUserIDs: allowed,
			})
			records := captureJournal(t)
			h.Handle(context.Background(), slackproto.Event{
				UserID: "UBOT", BotUserID: "UBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0",
				Text: "my own reply, quoted back", IsMention: true,
			})
			waitForIdle(t, h)

			recs := records()
			if len(recs) != 1 || recs[0].Decision != journal.DecisionDrop ||
				recs[0].Reason != journal.ReasonBotAuthored {
				t.Fatalf("want a single bot_authored drop, got %+v", recs)
			}
			if fs.posts != 0 {
				t.Fatal("the relay replied to itself — the loop guard failed")
			}
		})
	}
}

// TestSelfDriveIsStillAdmittedByTheSelfAuthorshipBackstop guards the
// other direction: the backstop above must not break the one
// sanctioned bot-authored path, or the hatch becomes dead code.
func TestSelfDriveIsStillAdmittedByTheSelfAuthorshipBackstop(t *testing.T) {
	fa := newFakeAgent()
	fs := newFakeSlack()
	t.Cleanup(fs.close)
	h := New(Config{
		Router: newTestRouter(t, fa), API: fs.client(),
		PromptTimeout: 5 * time.Second, Ambient: true,
		SelfDrive: slackproto.NewSelfDrive("!!drive!!"), SelfDrivePerMinute: 4,
	})
	records := captureJournal(t)
	h.Handle(context.Background(), slackproto.Event{
		UserID: "UBOT", BotUserID: "UBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0",
		Text: "do the thing", SelfDrive: true,
	})
	waitForIdle(t, h)
	assertOneRecord(t, records(), journal.PathSelfDrive, journal.DecisionRun, journal.ReasonPrompt)
}
