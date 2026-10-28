package verify

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kfet/slack-acp/internal/journal"
)

// fakeOutage models the relay going down: while it is down the user's
// posts are not seen live, and Start replays them as the catch-up does.
// passthrough leaves the relay up throughout — a "stop" that did not
// actually stop anything.
type fakeOutage struct {
	j        *fakeJournal
	bot      *fakeSlack
	down     bool
	queued   []queuedPost
	stopErr  error
	startErr error
	// passthrough: Stop does not take the relay down.
	passthrough bool
	stops       int
	starts      int
}

type queuedPost struct{ channel, threadTS, ts, text string }

func (o *fakeOutage) Stop(context.Context) error {
	o.stops++
	if o.stopErr != nil {
		return o.stopErr
	}
	o.down = !o.passthrough
	return nil
}

func (o *fakeOutage) Start(context.Context) error {
	o.starts++
	if o.startErr != nil {
		return o.startErr
	}
	o.down = false
	for _, p := range o.queued {
		proto := journal.Record{Stage: journal.StageProto, Path: journal.PathAppMention, Decision: journal.DecisionDeliver,
			Reason: journal.ReasonMention, Channel: p.channel, TS: p.ts, CatchUp: true}
		handler := journal.Record{Stage: journal.StageHandler, Path: journal.PathAppMention, Decision: journal.DecisionRun,
			Reason: journal.ReasonPrompt, Channel: p.channel, TS: p.ts, CatchUp: true}
		if strings.Contains(p.text, "> !status") {
			handler.Decision, handler.Reason = journal.DecisionDrop, journal.ReasonCatchUpCommand
			o.j.add(proto, handler)
			continue
		}
		o.j.add(proto, handler)
		o.bot.botReply(p.threadTS)
	}
	o.queued = nil
	return nil
}

// withOutage routes the user's posts through o while it is down.
func withOutage(j *fakeJournal, bot, user *fakeSlack) *fakeOutage {
	o := &fakeOutage{j: j, bot: bot}
	live := user.onPost
	user.onPost = func(f *fakeSlack, channel, threadTS, ts, text string) {
		if o.down {
			o.queued = append(o.queued, queuedPost{channel, threadTS, ts, text})
			return
		}
		live(f, channel, threadTS, ts, text)
	}
	return o
}

func catchUpResults(t *testing.T, o Outage, j *fakeJournal, bot, user *fakeSlack) (Result, Result) {
	t.Helper()
	r, err := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait, Outage: o})
	if err != nil {
		t.Fatal(err)
	}
	r.botUserID = "UBOT"
	res := r.checkCatchUp(context.Background())
	if len(res) != 2 || res[0].Name != "catch_up_mention_answered" || res[1].Name != "catch_up_command_skipped" {
		t.Fatalf("results = %+v", res)
	}
	return res[0], res[1]
}

func TestCatchUpChecksPass(t *testing.T) {
	j := &fakeJournal{}
	bot, user := scriptedRelay(j, "UBOT")
	o := withOutage(j, bot, user)
	answered, skipped := catchUpResults(t, o, j, bot, user)
	if answered.Status != StatusPass || skipped.Status != StatusPass {
		t.Fatalf("answered=%+v\nskipped=%+v", answered, skipped)
	}
	if o.stops != 1 || o.starts != 1 || o.down {
		t.Fatalf("outage not undone: %+v", o)
	}
}

func TestCatchUpCheckFailsWhenTheRelayWasNotDown(t *testing.T) {
	j := &fakeJournal{}
	bot, user := scriptedRelay(j, "UBOT")
	o := withOutage(j, bot, user)
	o.passthrough = true
	answered, _ := catchUpResults(t, o, j, bot, user)
	if answered.Status != StatusFail || !strings.Contains(answered.Detail, "not by the catch-up") {
		t.Fatalf("answered = %+v", answered)
	}
}

func TestCatchUpCheckSkips(t *testing.T) {
	j := &fakeJournal{}
	bot, user := scriptedRelay(j, "UBOT")
	a, b := catchUpResults(t, nil, j, bot, user)
	if a.Status != StatusSkip || b.Status != StatusSkip || !strings.Contains(a.Detail, "outage") {
		t.Fatalf("no outage: %+v %+v", a, b)
	}
	r, _ := New(Config{Bot: bot, Journal: j, PublicChannel: "C_PUB", Wait: immediateWait, Outage: &fakeOutage{}})
	if res := r.checkCatchUp(context.Background()); res[0].Status != StatusSkip || res[0].Detail != skipNoUserToken {
		t.Fatalf("no user token: %+v", res)
	}
}

func TestCatchUpCheckOutageErrors(t *testing.T) {
	j := &fakeJournal{}
	bot, user := scriptedRelay(j, "UBOT")

	o := withOutage(j, bot, user)
	o.stopErr = errors.New("no such unit")
	a, b := catchUpResults(t, o, j, bot, user)
	if a.Status != StatusFail || b.Status != StatusFail || !strings.Contains(a.Detail, "stop the relay") || o.starts != 0 {
		t.Fatalf("stop error: %+v %+v", a, b)
	}

	o = withOutage(j, bot, user)
	o.startErr = errors.New("failed")
	a, _ = catchUpResults(t, o, j, bot, user)
	if a.Status != StatusFail || !strings.Contains(a.Detail, "MAY STILL BE DOWN") {
		t.Fatalf("start error: %+v", a)
	}
}

func TestCatchUpCheckRestartsEvenWhenPostingFails(t *testing.T) {
	j := &fakeJournal{}
	bot, user := scriptedRelay(j, "UBOT")
	o := withOutage(j, bot, user)
	user.postErr = errors.New("boom")
	a, _ := catchUpResults(t, o, j, bot, user)
	if a.Status != StatusFail || !strings.Contains(a.Detail, "boom") || o.starts != 1 {
		t.Fatalf("got %+v (starts %d)", a, o.starts)
	}
}

func TestNewShellOutage(t *testing.T) {
	if o, err := NewShellOutage(" ", ""); o != nil || err != nil {
		t.Fatalf("none: %v %v", o, err)
	}
	for _, pair := range [][2]string{{"true", ""}, {"", "true"}} {
		if _, err := NewShellOutage(pair[0], pair[1]); err == nil {
			t.Fatalf("%q: a half-configured outage must be refused", pair)
		}
	}
	o, err := NewShellOutage("true", "echo nope >&2; exit 3")
	if err != nil {
		t.Fatal(err)
	}
	if err := o.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := o.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("start = %v", err)
	}
}

func TestFirstErr(t *testing.T) {
	boom := errors.New("boom")
	if firstErr(nil, nil) != nil || firstErr(nil, boom) != boom {
		t.Fatal("firstErr")
	}
}
