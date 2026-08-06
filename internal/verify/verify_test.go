package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kfet/slack-acp/internal/journal"
)

// ---- fakes ----

// workspace is the shared message store behind the two fakeSlack
// clients. Both tokens see one Slack, under one lock — modelling two
// clients over separate maps would let a check pass on state the other
// token could never observe.
type workspace struct {
	mu      sync.Mutex
	seq     int
	threads map[string][]Message
}

func (w *workspace) add(threadTS string, m Message) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.threads[threadTS] = append(w.threads[threadTS], m)
}

func (w *workspace) nextTS(prefix string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seq++
	return fmt.Sprintf("%s%04d.000000", prefix, w.seq)
}

func (w *workspace) replies(threadTS string) []Message {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]Message(nil), w.threads[threadTS]...)
}

// fakeSlack is one token's view of the workspace: it records posts,
// serves thread replies, and can simulate the relay answering.
type fakeSlack struct {
	ws       *workspace
	userID   string
	isBot    bool
	authErr  error
	postErr  error
	openErr  error
	replyErr error
	deleted  []string
	dmID     string

	// onPost is invoked after every successful post, letting a test
	// script the relay's reaction (e.g. "the bot replies").
	onPost func(f *fakeSlack, channel, threadTS, ts, text string)
}

// newWorkspace builds a bot client and a human client over one shared
// workspace — the pairing every test needs.
func newWorkspace() (bot, user *fakeSlack) {
	ws := &workspace{threads: map[string][]Message{}}
	return &fakeSlack{ws: ws, userID: "UBOT", isBot: true, dmID: "D_TEST"},
		&fakeSlack{ws: ws, userID: "UHUMAN", dmID: "D_TEST"}
}

func (f *fakeSlack) AuthTest(context.Context) (string, error) { return f.userID, f.authErr }

func (f *fakeSlack) Post(_ context.Context, channel, threadTS, text string) (string, error) {
	if f.postErr != nil {
		return "", f.postErr
	}
	ts := f.ws.nextTS("100")
	if threadTS == "" {
		threadTS = ts
	}
	m := Message{TS: ts, User: f.userID, Text: text}
	if f.isBot {
		m.BotID = "B1"
	}
	f.ws.add(threadTS, m)
	if f.onPost != nil {
		f.onPost(f, channel, threadTS, ts, text)
	}
	return ts, nil
}

// botReply injects a relay answer into a thread.
func (f *fakeSlack) botReply(threadTS string) string {
	ts := f.ws.nextTS("200")
	f.ws.add(threadTS, Message{TS: ts, User: "UBOT", BotID: "B1", Text: "answer"})
	return ts
}

func (f *fakeSlack) Delete(_ context.Context, _, ts string) error {
	f.deleted = append(f.deleted, ts)
	return nil
}

func (f *fakeSlack) Replies(_ context.Context, _, threadTS string) ([]Message, error) {
	if f.replyErr != nil {
		return nil, f.replyErr
	}
	return f.ws.replies(threadTS), nil
}

func (f *fakeSlack) OpenDM(context.Context, string) (string, error) { return f.dmID, f.openErr }

// fakeJournal serves records, optionally generating them from posts.
type fakeJournal struct {
	mu   sync.Mutex
	recs []journal.Record
	err  error
}

func (j *fakeJournal) Records(context.Context) ([]journal.Record, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return nil, j.err
	}
	return append([]journal.Record(nil), j.recs...), nil
}

func (j *fakeJournal) add(recs ...journal.Record) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.recs = append(j.recs, recs...)
}

// delivered appends the pair of records a fully-processed message
// produces.
func (j *fakeJournal) delivered(channel, ts string, path journal.Path, reason string) {
	j.add(
		journal.Record{Stage: journal.StageProto, Path: path, Decision: journal.DecisionDeliver, Reason: reason, Channel: channel, TS: ts},
		journal.Record{Stage: journal.StageHandler, Path: path, Decision: journal.DecisionRun, Reason: journal.ReasonPrompt, Channel: channel, TS: ts},
	)
}

func (j *fakeJournal) dropped(channel, ts string, path journal.Path, reason string) {
	j.add(journal.Record{Stage: journal.StageProto, Path: path, Decision: journal.DecisionDrop, Reason: reason, Channel: channel, TS: ts})
}

// immediateWait evaluates cond exactly once — no wall clock in tests.
func immediateWait(ctx context.Context, cond func(context.Context) (bool, error)) error {
	ok, err := cond(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("condition not met")
	}
	return nil
}

// scriptedRelay wires the fakes together so that a post which *should*
// be processed produces the journal records and the bot reply a live
// relay would produce.
func scriptedRelay(j *fakeJournal, botID string) (bot, user *fakeSlack) {
	bot, user = newWorkspace()
	react := func(f *fakeSlack, channel, threadTS, ts, text string) {
		switch {
		case f == user && channel == bot.dmID:
			j.delivered(channel, ts, journal.PathMessageIM, journal.ReasonDM)
			bot.botReply(threadTS)
		case f == user && strings.Contains(text, "<@"+botID+">"):
			j.delivered(channel, ts, journal.PathAppMention, journal.ReasonMention)
			bot.botReply(threadTS)
		case f == user && threadTS != ts && strings.Contains(text, "and one more thing"):
			j.delivered(channel, ts, journal.PathMessage, journal.ReasonAmbientThreadReply)
			bot.botReply(threadTS)
		case f == user && threadTS != ts:
			j.dropped(channel, ts, journal.PathMessage, journal.ReasonSelfDriveNotAccept)
			j.add(journal.Record{Stage: journal.StageHandler, Path: journal.PathMessage,
				Decision: journal.DecisionDrop, Reason: journal.ReasonAmbientUnknownThrd, Channel: channel, TS: ts})
		case f == bot && strings.Contains(text, "<@"+botID+">"):
			j.dropped(channel, ts, journal.PathAppMention, journal.ReasonBotAuthored)
		case f == bot && strings.HasPrefix(text, "!!drive!!"):
			j.delivered(channel, ts, journal.PathSelfDrive, journal.ReasonSelfDrive)
			bot.botReply(threadTS)
		}
	}
	bot.onPost, user.onPost = react, react
	return bot, user
}

func resultsByName(t *testing.T, results []Result) map[string]Result {
	t.Helper()
	m := map[string]Result{}
	for _, r := range results {
		if _, dup := m[r.Name]; dup {
			t.Fatalf("duplicate check name %q", r.Name)
		}
		m[r.Name] = r
	}
	return m
}

// ---- tests ----

// TestRunAllChecksPass is the harness's own end-to-end: a scripted
// relay that behaves exactly as designed must turn every check green.
func TestRunAllChecksPass(t *testing.T) {
	j := &fakeJournal{}
	bot, user := scriptedRelay(j, "UBOT")

	r, err := New(Config{
		Bot: bot, User: user, Journal: j,
		PublicChannel: "C_PUB", PrivateChannel: "C_PRIV",
		SelfDriveSentinel: "!!drive!!",
		Nonce:             "nonce",
		Wait:              immediateWait,
	})
	if err != nil {
		t.Fatal(err)
	}
	results, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	byName := resultsByName(t, results)
	for _, want := range []string{
		"app_mention_public", "ambient_thread_reply_known", "app_mention_private",
		"dm", "ambient_thread_reply_unknown_dropped", "bot_echo_dropped", "self_drive_hatch",
	} {
		res, ok := byName[want]
		if !ok {
			t.Fatalf("check %q was never run; got %v", want, results)
		}
		if res.Status != StatusPass {
			t.Errorf("%s: want PASS, got %s — %s", want, res.Status, res.Detail)
		}
	}
	report, allOK := Summarise(results)
	if !allOK {
		t.Fatalf("Summarise says not ok:\n%s", report)
	}
	if !strings.Contains(report, "7 passed, 0 failed, 0 skipped") {
		t.Fatalf("unexpected summary:\n%s", report)
	}
}

// TestRunCleansUpEverythingItPosted pins the cleanup contract: no test
// debris is left in a shared channel, including the relay's replies.
func TestRunCleansUpEverythingItPosted(t *testing.T) {
	j := &fakeJournal{}
	bot, user := scriptedRelay(j, "UBOT")

	r, _ := New(Config{
		Bot: bot, User: user, Journal: j,
		PublicChannel: "C_PUB", Nonce: "nonce", Wait: immediateWait,
	})
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(user.deleted) == 0 {
		t.Error("user-authored test messages were not deleted")
	}
	if len(bot.deleted) == 0 {
		t.Error("bot-authored messages (posts and relay replies) were not deleted")
	}
	if len(r.posted) != 0 {
		t.Errorf("cleanup must clear the bookkeeping, still holding %d", len(r.posted))
	}
}

// TestChecksSkipWithoutUserToken is the honesty test: with no user
// token, every human-authored path must report SKIP with a reason —
// never PASS, and never silently substituted with a bot post (which
// cannot clear the app_mention guard).
func TestChecksSkipWithoutUserToken(t *testing.T) {
	j := &fakeJournal{}
	bot, _ := scriptedRelay(j, "UBOT")

	r, _ := New(Config{Bot: bot, Journal: j, PublicChannel: "C_PUB", PrivateChannel: "C_PRIV", Nonce: "n", Wait: immediateWait})
	results, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := resultsByName(t, results)
	for _, name := range []string{"app_mention_public", "app_mention_private", "dm", "ambient_thread_reply_known", "ambient_thread_reply_unknown_dropped"} {
		res := byName[name]
		if res.Status != StatusSkip {
			t.Errorf("%s: want SKIP without a user token, got %s (%s)", name, res.Status, res.Detail)
		}
		if res.Detail == "" {
			t.Errorf("%s: a SKIP must carry its reason", name)
		}
	}
	if byName["bot_echo_dropped"].Status != StatusPass {
		t.Errorf("the bot-authored check needs no user token: %+v", byName["bot_echo_dropped"])
	}
	report, ok := Summarise(results)
	if !ok {
		t.Fatalf("skips must not fail the run:\n%s", report)
	}
	if !strings.Contains(report, "skipped checks are UNVERIFIED") {
		t.Fatalf("the summary must say skips are not passes:\n%s", report)
	}
}

func TestSelfDriveSkippedWhenSentinelUnset(t *testing.T) {
	j := &fakeJournal{}
	bot, user := scriptedRelay(j, "UBOT")
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	results, _ := r.Run(context.Background())
	res := resultsByName(t, results)["self_drive_hatch"]
	if res.Status != StatusSkip || !strings.Contains(res.Detail, "hatch is off") {
		t.Fatalf("got %+v", res)
	}
}

func TestPrivateMentionSkippedWithoutChannel(t *testing.T) {
	j := &fakeJournal{}
	bot, user := scriptedRelay(j, "UBOT")
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	results, _ := r.Run(context.Background())
	res := resultsByName(t, results)["app_mention_private"]
	if res.Status != StatusSkip {
		t.Fatalf("got %+v", res)
	}
}

// TestDropCheckFailsWhenTheRelayNeverSawTheMessage is the reason the
// journal half exists: silence is not evidence of a drop.
func TestDropCheckFailsWhenTheRelayNeverSawTheMessage(t *testing.T) {
	j := &fakeJournal{} // never records anything — relay is "down"
	bot, user := newWorkspace()

	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	res := r.checkAmbientUnknownThread(context.Background())
	if res.Status != StatusFail {
		t.Fatalf("a silent relay must FAIL the drop check, got %+v", res)
	}
	if !strings.Contains(res.Detail, "NOT the same as dropping it") {
		t.Fatalf("the failure must explain why silence is not a pass: %s", res.Detail)
	}
}

// TestDropCheckFailsWhenTheRelayRepliedAnyway catches the inverse: a
// drop was journalled but a second path let the message through.
func TestDropCheckFailsWhenTheRelayRepliedAnyway(t *testing.T) {
	j := &fakeJournal{}
	bot, user := newWorkspace()
	user.onPost = func(_ *fakeSlack, channel, threadTS, ts, _ string) {
		if threadTS != ts {
			j.dropped(channel, ts, journal.PathMessage, journal.ReasonAmbientUnknownThrd)
			bot.botReply(threadTS) // …but it answered anyway
		}
	}

	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	res := r.checkAmbientUnknownThread(context.Background())
	if res.Status != StatusFail || !strings.Contains(res.Detail, "still replied") {
		t.Fatalf("got %+v", res)
	}
}

// TestDropCheckFailsWhenTheRunArrivesAfterTheDrop covers the race the
// re-read exists for: Slack delivers a tagged message as two
// independent envelopes, so a `run` on the second path can land after
// the harness has already observed the `drop` on the first. Waiting
// alone would have declared this a PASS.
func TestDropCheckFailsWhenTheRunArrivesAfterTheDrop(t *testing.T) {
	j := &fakeJournal{}
	bot, user := newWorkspace()
	var lateChannel, lateTS string
	user.onPost = func(_ *fakeSlack, channel, threadTS, ts, _ string) {
		if threadTS != ts {
			j.dropped(channel, ts, journal.PathMessage, journal.ReasonAmbientUnknownThrd)
			lateChannel, lateTS = channel, ts
		}
	}
	// The wait sees only the drop; the run lands between the wait
	// returning and the re-read.
	lateWait := func(ctx context.Context, cond func(context.Context) (bool, error)) error {
		err := immediateWait(ctx, cond)
		if lateTS != "" {
			j.add(journal.Record{Stage: journal.StageHandler, Decision: journal.DecisionRun,
				Reason: journal.ReasonPrompt, Channel: lateChannel, TS: lateTS})
		}
		return err
	}
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: lateWait})
	res := r.checkAmbientUnknownThread(context.Background())
	if res.Status != StatusFail || !strings.Contains(res.Detail, "dropped AND run") {
		t.Fatalf("got %+v", res)
	}
}

// TestRefusalIgnoresNonDropRecords pins that a deliver/run record never
// reads as a refusal.
func TestRefusalIgnoresNonDropRecords(t *testing.T) {
	recs := []journal.Record{
		{Stage: journal.StageProto, Decision: journal.DecisionDeliver, Reason: journal.ReasonMention},
		{Stage: journal.StageProto, Decision: journal.DecisionDrop, Reason: journal.ReasonMentionDuplicate},
	}
	if got := refusal(recs); got != "" {
		t.Fatalf("neither a delivery nor the mention twin is a refusal, got %q", got)
	}
	if got := refusal(append(recs, journal.Record{Stage: journal.StageHandler,
		Decision: journal.DecisionDrop, Reason: journal.ReasonAllowlist})); got != journal.ReasonAllowlist {
		t.Fatalf("a handler-stage drop is always terminal, got %q", got)
	}
}

// TestRunCheckFailsWhenJournalledButNoReply catches the silent-agent
// failure: the relay decided to answer and then didn't.
func TestRunCheckFailsWhenJournalledButNoReply(t *testing.T) {
	j := &fakeJournal{}
	bot, user := newWorkspace()
	user.onPost = func(_ *fakeSlack, channel, _, ts, _ string) {
		j.delivered(channel, ts, journal.PathAppMention, journal.ReasonMention)
	}
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	r.botUserID = "UBOT"
	res, _ := r.checkMention(context.Background(), "app_mention_public", "C_PUB")
	if res.Status != StatusFail || !strings.Contains(res.Detail, "no bot reply appeared") {
		t.Fatalf("got %+v", res)
	}
}

// TestRunCheckFailsOnWrongDeliveryPath catches the class of bug this
// whole exercise started from: the message was processed, but it
// arrived on the wrong surface (e.g. classified as ambient rather than
// as a mention).
func TestRunCheckFailsOnWrongDeliveryPath(t *testing.T) {
	j := &fakeJournal{}
	bot, user := newWorkspace()
	user.onPost = func(_ *fakeSlack, channel, threadTS, ts, _ string) {
		j.delivered(channel, ts, journal.PathMessage, journal.ReasonAmbientThreadReply)
		bot.botReply(threadTS)
	}
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	r.botUserID = "UBOT"
	res, _ := r.checkMention(context.Background(), "app_mention_public", "C_PUB")
	if res.Status != StatusFail || !strings.Contains(res.Detail, "expected slackproto deliver on path=app_mention") {
		t.Fatalf("got %+v", res)
	}
}

func TestKnownThreadCheckSkipsWithoutAThread(t *testing.T) {
	j := &fakeJournal{}
	bot, user := newWorkspace()
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	res := r.checkAmbientKnownThread(context.Background(), "")
	if res.Status != StatusSkip || !strings.Contains(res.Detail, "no known thread") {
		t.Fatalf("got %+v", res)
	}
}

// ---- error propagation ----

func TestPostErrorsSurfaceAsFailures(t *testing.T) {
	boom := errors.New("boom")
	j := &fakeJournal{}
	bot, user := newWorkspace()
	user.postErr, bot.postErr = boom, boom

	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", PrivateChannel: "C_PRIV",
		SelfDriveSentinel: "!!drive!!", Nonce: "n", Wait: immediateWait})
	results, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range results {
		// The known-thread check depends on the mention check having
		// established a thread; when posting is broken there is no
		// thread, and SKIP (with a reason) is the honest answer.
		if res.Name == "ambient_thread_reply_known" {
			if res.Status != StatusSkip {
				t.Errorf("%s: want SKIP with no thread to reply into, got %s", res.Name, res.Status)
			}
			continue
		}
		if res.Status != StatusFail {
			t.Errorf("%s: want FAIL when posting is broken, got %s", res.Name, res.Status)
		}
	}
}

// TestAmbientUnknownReplyPostErrorFails covers the second post in the
// unknown-thread check (the parent succeeds, the reply fails).
func TestAmbientUnknownReplyPostErrorFails(t *testing.T) {
	j := &fakeJournal{}
	bot, base := newWorkspace()
	user := &failSecondPost{fakeSlack: base}
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	res := r.checkAmbientUnknownThread(context.Background())
	if res.Status != StatusFail || !strings.Contains(res.Detail, "post reply as user") {
		t.Fatalf("got %+v", res)
	}
}

// TestBotEchoReplyPostErrorFails covers the same for the bot echo.
func TestBotEchoReplyPostErrorFails(t *testing.T) {
	j := &fakeJournal{}
	base, _ := newWorkspace()
	bot := &failSecondPost{fakeSlack: base}
	r, _ := New(Config{Bot: bot, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	res := r.checkBotEcho(context.Background())
	if res.Status != StatusFail || !strings.Contains(res.Detail, "post as bot") {
		t.Fatalf("got %+v", res)
	}
}

type failSecondPost struct {
	*fakeSlack
	n int
}

func (f *failSecondPost) Post(ctx context.Context, channel, threadTS, text string) (string, error) {
	f.n++
	if f.n >= 2 {
		return "", errors.New("boom")
	}
	return f.fakeSlack.Post(ctx, channel, threadTS, text)
}

func TestDMOpenErrorFails(t *testing.T) {
	j := &fakeJournal{}
	bot, user := newWorkspace()
	bot.openErr = errors.New("missing_scope")
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	res := r.checkDM(context.Background())
	if res.Status != StatusFail || !strings.Contains(res.Detail, "im:write") {
		t.Fatalf("the failure must name the missing scope: %+v", res)
	}
}

func TestJournalReadErrorFailsTheCheck(t *testing.T) {
	j := &fakeJournal{err: errors.New("journalctl: no such unit")}
	bot, user := newWorkspace()
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	r.botUserID = "UBOT"
	res, _ := r.checkMention(context.Background(), "app_mention_public", "C_PUB")
	if res.Status != StatusFail || !strings.Contains(res.Detail, "read ingest journal") {
		t.Fatalf("got %+v", res)
	}
}

func TestRepliesErrorFailsTheDropCheck(t *testing.T) {
	j := &fakeJournal{}
	bot, user := newWorkspace()
	user.onPost = func(_ *fakeSlack, channel, threadTS, ts, _ string) {
		if threadTS != ts {
			j.dropped(channel, ts, journal.PathMessage, journal.ReasonAmbientUnknownThrd)
			bot.replyErr = errors.New("channel_not_found")
		}
	}
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	res := r.checkAmbientUnknownThread(context.Background())
	if res.Status != StatusFail || !strings.Contains(res.Detail, "conversations.replies") {
		t.Fatalf("got %+v", res)
	}
}

func TestRunCheckRepliesErrorFails(t *testing.T) {
	j := &fakeJournal{}
	bot, user := newWorkspace()
	bot.replyErr = errors.New("channel_not_found")
	user.onPost = func(_ *fakeSlack, channel, _, ts, _ string) {
		j.delivered(channel, ts, journal.PathAppMention, journal.ReasonMention)
	}
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	r.botUserID = "UBOT"
	res, _ := r.checkMention(context.Background(), "app_mention_public", "C_PUB")
	if res.Status != StatusFail {
		t.Fatalf("got %+v", res)
	}
}

// ---- construction and identity ----

func TestNewValidates(t *testing.T) {
	j := &fakeJournal{}
	bot, _ := newWorkspace()
	for name, cfg := range map[string]Config{
		"no bot client":  {Journal: j, PublicChannel: "C"},
		"no journal":     {Bot: bot, PublicChannel: "C"},
		"no public chan": {Bot: bot, Journal: j},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	r, err := New(Config{Bot: bot, Journal: j, PublicChannel: "C"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Nonce() == "" || !strings.HasPrefix(r.Nonce(), "slack-acp-verify-") {
		t.Fatalf("a run must carry a distinguishing nonce, got %q", r.Nonce())
	}
	if r.cfg.Wait == nil || r.cfg.Now == nil {
		t.Fatal("defaults were not installed")
	}
}

// TestUserTokenMustNotBeTheBot is the anti-self-deception check: a
// user token that resolves to the bot itself would make every
// "human-authored" post bot-authored, and every app_mention check
// would fail confusingly rather than loudly.
func TestUserTokenMustNotBeTheBot(t *testing.T) {
	j := &fakeJournal{}
	bot, user := newWorkspace()
	user.userID = bot.userID // the token resolves to the bot itself
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C", Nonce: "n", Wait: immediateWait})
	_, err := r.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot exercise the app_mention guard") {
		t.Fatalf("got %v", err)
	}
}

func TestAuthErrorsAbortTheRun(t *testing.T) {
	j := &fakeJournal{}
	t.Run("bot", func(t *testing.T) {
		bot, _ := newWorkspace()
		bot.authErr = errors.New("invalid_auth")
		r, _ := New(Config{Bot: bot, Journal: j, PublicChannel: "C", Nonce: "n", Wait: immediateWait})
		if _, err := r.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "bot auth.test") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("user", func(t *testing.T) {
		bot, user := newWorkspace()
		user.authErr = errors.New("invalid_auth")
		r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C", Nonce: "n", Wait: immediateWait})
		if _, err := r.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "user auth.test") {
			t.Fatalf("got %v", err)
		}
	})
}

// ---- helpers ----

func TestMessageAuthored(t *testing.T) {
	if !(Message{BotID: "B1"}).authored("UBOT") {
		t.Error("a bot_id marks a bot message")
	}
	if !(Message{User: "UBOT"}).authored("UBOT") {
		t.Error("our own user id marks a bot message")
	}
	if (Message{User: "UHUMAN"}).authored("UBOT") {
		t.Error("a human message must not be attributed to the bot")
	}
	if (Message{User: "UBOT"}).authored("") {
		t.Error("with no known bot id, a plain user message is not ours")
	}
}

func TestBotRepliedIgnoresOurOwnPostsAndOlderMessages(t *testing.T) {
	r := &Runner{botUserID: "UBOT", posted: []posted{{ts: "1.0"}}}
	msgs := []Message{
		{TS: "1.0", BotID: "B1"},  // posted by this run
		{TS: "0.5", User: "UBOT"}, // older than the cutoff
	}
	if r.botReplied(msgs, "0.9") {
		t.Fatal("neither message counts as a relay reply")
	}
	if !r.botReplied(append(msgs, Message{TS: "2.0", BotID: "B1"}), "0.9") {
		t.Fatal("a newer bot message is a relay reply")
	}
}

func TestCleanupUsesAFreshContextWhenCancelled(t *testing.T) {
	bot, _ := newWorkspace()
	r := &Runner{cfg: Config{Bot: bot}, posted: []posted{{slack: bot, channel: "C", ts: "1.0"}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.cleanup(ctx)
	if len(bot.deleted) != 1 {
		t.Fatal("cleanup must still run after the run context is cancelled")
	}
}

func TestSummariseCountsFailures(t *testing.T) {
	report, ok := Summarise([]Result{
		{Name: "a", Status: StatusPass},
		{Name: "b", Status: StatusFail, Detail: "nope"},
	})
	if ok {
		t.Error("a failure must fail the run")
	}
	if !strings.Contains(report, "1 passed, 1 failed, 0 skipped") {
		t.Fatalf("got %s", report)
	}
}

// ---- PollWaiter ----

func TestPollWaiterReturnsOnFirstTrue(t *testing.T) {
	calls := 0
	err := PollWaiter(time.Millisecond, time.Second)(context.Background(), func(context.Context) (bool, error) {
		calls++
		return calls == 3, nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestPollWaiterPropagatesErrors(t *testing.T) {
	boom := errors.New("boom")
	err := PollWaiter(time.Millisecond, time.Second)(context.Background(), func(context.Context) (bool, error) {
		return false, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
}

func TestPollWaiterTimesOut(t *testing.T) {
	err := PollWaiter(time.Millisecond, 5*time.Millisecond)(context.Background(), func(context.Context) (bool, error) {
		return false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("got %v", err)
	}
}

func TestKnownThreadCheckFailsWhenThePostFails(t *testing.T) {
	j := &fakeJournal{}
	bot, user := newWorkspace()
	user.postErr = errors.New("boom")
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	res := r.checkAmbientKnownThread(context.Background(), "1000001.000000")
	if res.Status != StatusFail || !strings.Contains(res.Detail, "post as user") {
		t.Fatalf("got %+v", res)
	}
}

// TestDropCheckFailsWhenTheJournalIsUnreadable: an unreadable journal
// must be a loud failure, never an empty record set that reads as
// "nothing was seen".
func TestDropCheckFailsWhenTheJournalIsUnreadable(t *testing.T) {
	j := &fakeJournal{err: errors.New("journalctl: no such unit")}
	bot, user := newWorkspace()
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	res := r.checkAmbientUnknownThread(context.Background())
	if res.Status != StatusFail || !strings.Contains(res.Detail, "read ingest journal") {
		t.Fatalf("got %+v", res)
	}
}

// TestRunCheckFailsFastOnTerminalRefusal is the regression test for the
// diagnosis failure that followed the first live run: a mention the
// relay refused outright still burned the FULL wait budget, and then
// reported whatever error the dying poll produced ("context deadline
// exceeded" from the journal read) instead of the refusal sitting in
// the journal. Five such checks turned a 20-second run into 15 minutes
// and sent an operator after a slow journald that was answering in
// 36ms. The check must now stop the moment the relay has decided, and
// say what it decided.
func TestRunCheckFailsFastOnTerminalRefusal(t *testing.T) {
	j := &fakeJournal{}
	bot, user := newWorkspace()
	user.onPost = func(_ *fakeSlack, channel, _, ts, _ string) {
		j.dropped(channel, ts, journal.PathAppMention, journal.ReasonAPIAuthored)
	}

	polls := 0
	countingWait := func(ctx context.Context, cond func(context.Context) (bool, error)) error {
		for {
			polls++
			ok, err := cond(ctx)
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
			if polls > 5 {
				t.Fatal("the check kept waiting after the relay had already refused the message")
			}
		}
	}

	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: countingWait})
	r.botUserID = "UBOT"
	res, _ := r.checkMention(context.Background(), "app_mention_public", "C_PUB")

	if res.Status != StatusFail {
		t.Fatalf("got %+v", res)
	}
	if !strings.Contains(res.Detail, "REFUSED") || !strings.Contains(res.Detail, journal.ReasonAPIAuthored) {
		t.Fatalf("the failure must name the relay's actual decision, got: %s", res.Detail)
	}
	if polls != 1 {
		t.Fatalf("want a single poll before the verdict, got %d", polls)
	}
}

// TestRunCheckDoesNotMistakeTheMentionTwinForARefusal is the other half
// of the fail-fast contract, and the subtle one. Slack delivers a
// tagged message as TWO envelopes, so a perfectly healthy mention
// legitimately produces a drop *alongside* its delivery:
//
//	app_mention     deliver/mention
//	message_channel drop/not_thread_reply
//
// Treating that drop as terminal would fail a check that is about to
// pass — turning a passing relay red.
func TestRunCheckDoesNotMistakeTheMentionTwinForARefusal(t *testing.T) {
	j := &fakeJournal{}
	bot, user := newWorkspace()
	user.onPost = func(_ *fakeSlack, channel, threadTS, ts, _ string) {
		// The twin drop lands FIRST, exactly as it does live.
		j.dropped(channel, ts, journal.PathMessage, journal.ReasonNotThreadReply)
		j.delivered(channel, ts, journal.PathAppMention, journal.ReasonMention)
		bot.botReply(threadTS)
	}

	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	r.botUserID = "UBOT"
	res, _ := r.checkMention(context.Background(), "app_mention_public", "C_PUB")
	if res.Status != StatusPass {
		t.Fatalf("the app_mention twin must not be read as a refusal: %+v", res)
	}
}

// TestDropCheckFailsFastWhenRun mirrors the above for negative checks.
func TestDropCheckFailsFastWhenRun(t *testing.T) {
	j := &fakeJournal{}
	bot, user := newWorkspace()
	user.onPost = func(_ *fakeSlack, channel, _, ts, _ string) {
		j.delivered(channel, ts, journal.PathMessage, journal.ReasonAmbientThreadReply)
	}
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	res := r.checkAmbientUnknownThread(context.Background())
	if res.Status != StatusFail || !strings.Contains(res.Detail, "was RUN, not dropped") {
		t.Fatalf("got %+v", res)
	}
}

// TestRunCheckFailsWhenTheDeliveryRecordIsMissingEntirely covers the
// case where the handler ran but slackproto never journalled a
// delivery — an impossible state that would mean the two stages
// disagree, and must be reported rather than passed.
func TestRunCheckFailsWhenTheDeliveryRecordIsMissingEntirely(t *testing.T) {
	j := &fakeJournal{}
	bot, user := newWorkspace()
	user.onPost = func(_ *fakeSlack, channel, threadTS, ts, _ string) {
		j.add(journal.Record{Stage: journal.StageHandler, Path: journal.PathAppMention,
			Decision: journal.DecisionRun, Reason: journal.ReasonPrompt, Channel: channel, TS: ts})
		bot.botReply(threadTS)
	}
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	r.botUserID = "UBOT"
	res, _ := r.checkMention(context.Background(), "app_mention_public", "C_PUB")
	if res.Status != StatusFail || !strings.Contains(res.Detail, "expected slackproto deliver") {
		t.Fatalf("got %+v", res)
	}
}

// TestRunCheckKeepsWaitingWhileUndecided covers the ordinary case the
// fail-fast must not break: the relay has not journalled anything for
// this ts yet, so the check keeps polling rather than concluding
// either way. Getting this wrong in the other direction would make
// every check a race against the relay's first log line.
func TestRunCheckKeepsWaitingWhileUndecided(t *testing.T) {
	j := &fakeJournal{}
	bot, user := newWorkspace()
	var pending func()
	user.onPost = func(_ *fakeSlack, channel, threadTS, ts, _ string) {
		// Deliberately journal NOTHING yet — the relay is still busy.
		pending = func() {
			j.delivered(channel, ts, journal.PathAppMention, journal.ReasonMention)
			bot.botReply(threadTS)
		}
	}
	polls := 0
	twoPhaseWait := func(ctx context.Context, cond func(context.Context) (bool, error)) error {
		for {
			polls++
			ok, err := cond(ctx)
			if err != nil {
				return err
			}
			if ok {
				return nil
			}
			if pending != nil {
				pending()
				pending = nil
			}
			if polls > 10 {
				return errors.New("gave up")
			}
		}
	}
	r, _ := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: twoPhaseWait})
	r.botUserID = "UBOT"
	res, _ := r.checkMention(context.Background(), "app_mention_public", "C_PUB")
	if res.Status != StatusPass {
		t.Fatalf("an undecided relay must be waited for, not failed: %+v", res)
	}
	if polls < 2 {
		t.Fatalf("expected the check to poll more than once, got %d", polls)
	}
}
