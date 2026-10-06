package verify

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kfet/slack-acp/internal/journal"
)

// branchRunner is a runner over a scripted relay with a live thread
// already established by a user post, as the public mention leaves.
func branchRunner(t *testing.T) (*Runner, *fakeSlack, *fakeSlack, *fakeJournal, string) {
	t.Helper()
	j := &fakeJournal{}
	bot, user := scriptedRelay(j, "UBOT")
	r, err := New(Config{Bot: bot, User: user, Journal: j, PublicChannel: "C_PUB", Nonce: "n", Wait: immediateWait})
	if err != nil {
		t.Fatal(err)
	}
	r.botUserID = "UBOT"
	thread, _ := user.Post(context.Background(), "C_PUB", "", "parent")
	return r, bot, user, j, thread
}

func TestBranchReactionPassesAndCleansUp(t *testing.T) {
	r, bot, _, _, thread := branchRunner(t)
	res := r.checkBranchReaction(context.Background(), thread)
	if res.Status != StatusPass {
		t.Fatalf("got %+v", res)
	}
	r.cleanup(context.Background())
	// The new thread's parent and the child's reply, plus the
	// announcement, are all deleted with the bot token.
	if len(bot.deleted) < 3 {
		t.Fatalf("bot deleted %v", bot.deleted)
	}
	var child bool
	for _, ts := range bot.deleted {
		child = child || strings.HasPrefix(ts, "300")
	}
	if !child {
		t.Fatalf("the branch thread was not cleaned up: %v", bot.deleted)
	}
}

func TestBranchReactionSkips(t *testing.T) {
	r, _, _, _, _ := branchRunner(t)
	if res := r.checkBranchReaction(context.Background(), ""); res.Status != StatusSkip || !strings.Contains(res.Detail, "did not establish") {
		t.Fatalf("got %+v", res)
	}
	r.cfg.User = nil
	if res := r.checkBranchReaction(context.Background(), "1.0"); res.Status != StatusSkip {
		t.Fatalf("got %+v", res)
	}
}

func TestBranchReactionFailures(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(r *Runner, bot, user *fakeSlack, j *fakeJournal)
		want  string
	}{
		{"post", func(_ *Runner, _, user *fakeSlack, _ *fakeJournal) { user.postErr = errors.New("boom") }, "post as user"},
		{"react", func(_ *Runner, _, user *fakeSlack, _ *fakeJournal) { user.reactErr = errors.New("boom") }, "reactions:write"},
		{"refused", func(_ *Runner, _, user *fakeSlack, j *fakeJournal) {
			user.onReact = func(channel, ts, _ string) {
				j.add(journal.Record{Stage: journal.StageHandler, Path: journal.PathReaction, Decision: journal.DecisionDrop, Reason: journal.ReasonAllowlist, Channel: channel, TS: ts})
			}
		}, "REFUSED the reaction with reason=\"allowlist\""},
		{"never seen", func(_ *Runner, _, user *fakeSlack, _ *fakeJournal) { user.onReact = nil }, "reactions:read"},
		{"journal", func(_ *Runner, _, _ *fakeSlack, j *fakeJournal) { j.err = errors.New("jr") }, "jr"},
		{"no proto deliver", func(_ *Runner, bot, user *fakeSlack, j *fakeJournal) {
			user.onReact = func(channel, ts, _ string) {
				j.add(journal.Record{Stage: journal.StageHandler, Path: journal.PathReaction, Decision: journal.DecisionRun, Reason: journal.ReasonBranch, Channel: channel, TS: ts})
				bot.branch(channel, ts, true)
			}
		}, "expected slackproto deliver"},
		{"no announcement", func(_ *Runner, _, user *fakeSlack, j *fakeJournal) {
			user.onReact = func(channel, ts, _ string) {
				j.add(
					journal.Record{Stage: journal.StageProto, Path: journal.PathReaction, Decision: journal.DecisionDeliver, Reason: journal.ReasonBranchReaction, Channel: channel, TS: ts},
					journal.Record{Stage: journal.StageHandler, Path: journal.PathReaction, Decision: journal.DecisionRun, Reason: journal.ReasonBranch, Channel: channel, TS: ts},
				)
			}
		}, "posted no link"},
		{"replies", func(_ *Runner, bot, _ *fakeSlack, _ *fakeJournal) { bot.replyErr = errors.New("rp") }, "rp"},
		{"no permalink", func(_ *Runner, bot, user *fakeSlack, j *fakeJournal) {
			user.onReact = func(channel, ts, _ string) {
				j.add(
					journal.Record{Stage: journal.StageProto, Path: journal.PathReaction, Decision: journal.DecisionDeliver, Reason: journal.ReasonBranchReaction, Channel: channel, TS: ts},
					journal.Record{Stage: journal.StageHandler, Path: journal.PathReaction, Decision: journal.DecisionRun, Reason: journal.ReasonBranch, Channel: channel, TS: ts},
				)
				bot.branch(channel, ts, false)
			}
		}, "no permalink"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, bot, user, j, thread := branchRunner(t)
			tc.setup(r, bot, user, j)
			res := r.checkBranchReaction(ctx, thread)
			if res.Status != StatusFail || !strings.Contains(res.Detail, tc.want) {
				t.Fatalf("got %+v, want %q", res, tc.want)
			}
		})
	}
}

func TestPermalinkTS(t *testing.T) {
	for in, want := range map[string]string{
		"<https://w.slack.com/archives/C1/p1700000000000100|x>": "1700000000.000100",
		"no link": "",
		"<https://w.slack.com/archives/C1/p12|x>":   "",
		"<https://example.com/p1700000000000100|x>": "",
	} {
		if got := permalinkTS(in); got != want {
			t.Errorf("permalinkTS(%q) = %q, want %q", in, got, want)
		}
	}
}
