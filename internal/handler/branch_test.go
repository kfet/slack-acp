package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/slack-go/slack"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/slack-acp/internal/journal"
	"github.com/kfet/slack-acp/internal/router"
	"github.com/kfet/slack-acp/internal/slackmcp"
	"github.com/kfet/slack-acp/internal/slackproto"
)

// branchSlack is a Slack Web API stub for the branch paths: it numbers
// the ts of every post, serves permalinks and conversations.replies,
// and records where each post went.
type branchSlack struct {
	srv *httptest.Server

	mu      sync.Mutex
	next    int
	posts   []sentPost
	replies []slack.Message
	repErr  bool
	// failTop fails top-level posts (no thread_ts); failLinks fails
	// chat.getPermalink; badTS makes the next top-level ts unusable as
	// a thread directory name.
	failTop   bool
	failReply bool // fails posts into a thread
	failLinks bool
	badTS     bool
}

type sentPost struct{ channel, threadTS, text, ts string }

func newBranchSlack(t *testing.T) *branchSlack {
	t.Helper()
	bs := &branchSlack{next: 100}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		bs.mu.Lock()
		defer bs.mu.Unlock()
		p := sentPost{channel: r.FormValue("channel"), threadTS: r.FormValue("thread_ts"), text: r.FormValue("text")}
		if (p.threadTS == "" && bs.failTop) || (p.threadTS != "" && bs.failReply) {
			_, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel"}`))
			return
		}
		bs.next++
		p.ts = fmt.Sprintf("%d.0", bs.next)
		if p.threadTS == "" && bs.badTS {
			p.ts = ".bad"
		}
		bs.posts = append(bs.posts, p)
		_, _ = fmt.Fprintf(w, `{"ok":true,"channel":%q,"ts":%q}`, p.channel, p.ts)
	})
	mux.HandleFunc("/chat.update", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1.0","text":"x"}`))
	})
	mux.HandleFunc("/chat.getPermalink", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		bs.mu.Lock()
		fail := bs.failLinks
		bs.mu.Unlock()
		if fail {
			_, _ = w.Write([]byte(`{"ok":false,"error":"message_not_found"}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"channel":%q,"permalink":"https://perma/%s/%s"}`,
			r.FormValue("channel"), r.FormValue("channel"), r.FormValue("message_ts"))
	})
	mux.HandleFunc("/conversations.replies", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		bs.mu.Lock()
		defer bs.mu.Unlock()
		if bs.repErr {
			_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
			return
		}
		// Like Slack: the thread the ts names, parent first, within
		// oldest/latest — except that the parent is ALWAYS included —
		// and cut at limit.
		ts, oldest, latest := r.FormValue("ts"), r.FormValue("oldest"), r.FormValue("latest")
		thread := ts
		for _, m := range bs.replies {
			if m.Timestamp == ts {
				thread = threadOf(m)
			}
		}
		var out []slack.Message
		for _, m := range bs.replies {
			if threadOf(m) != thread {
				continue
			}
			inRange := (oldest == "" || m.Timestamp >= oldest) && (latest == "" || m.Timestamp <= latest)
			if m.Timestamp == thread || inRange {
				out = append(out, m)
			}
		}
		if n, _ := strconv.Atoi(r.FormValue("limit")); n > 0 && len(out) > n {
			out = out[:n]
		}
		_ = json.NewEncoder(w).Encode(struct {
			OK       bool            `json:"ok"`
			Messages []slack.Message `json:"messages"`
		}{true, out})
	})
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"user_not_found"}`))
	})
	bs.srv = httptest.NewServer(mux)
	t.Cleanup(bs.srv.Close)
	return bs
}

func (bs *branchSlack) client() *slack.Client {
	return slack.New("xoxb-fake", slack.OptionAPIURL(bs.srv.URL+"/"))
}

func (bs *branchSlack) sent() []sentPost {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return append([]sentPost(nil), bs.posts...)
}

// topLevel returns the posts that opened a new thread.
func (bs *branchSlack) topLevel() []sentPost {
	var out []sentPost
	for _, p := range bs.sent() {
		if p.threadTS == "" {
			out = append(out, p)
		}
	}
	return out
}

func (bs *branchSlack) inThread(ts string) []sentPost {
	var out []sentPost
	for _, p := range bs.sent() {
		if p.threadTS == ts {
			out = append(out, p)
		}
	}
	return out
}

func msg(ts, threadTS, user, botID, text string) slack.Message {
	var m slack.Message
	m.Timestamp, m.ThreadTimestamp, m.User, m.BotID, m.Text = ts, threadTS, user, botID, text
	return m
}

// branchEnv is a handler wired to a fake agent that records every
// prompt by session.
type branchEnv struct {
	h  *Handler
	fa *fakeAgent
	r  *router.Router
	bs *branchSlack

	mu      sync.Mutex
	prompts map[acp.SessionId][]string
}

func newBranchEnv(t *testing.T, cfg Config) *branchEnv {
	t.Helper()
	e := &branchEnv{fa: newFakeAgent(), bs: newBranchSlack(t), prompts: map[acp.SessionId][]string{}}
	e.r = newTestRouter(t, e.fa)
	e.fa.promptHook = func(_ context.Context, sid acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
		e.mu.Lock()
		e.prompts[sid] = append(e.prompts[sid], blocks[0].Text.Text)
		e.mu.Unlock()
		return acp.StopReasonEndTurn, nil
	}
	cfg.Router, cfg.API, cfg.NoProgressTimeout = e.r, e.bs.client(), 5*time.Second
	e.h = New(cfg)
	return e
}

func (e *branchEnv) promptsOf(sid acp.SessionId) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.prompts[sid]
}

func (e *branchEnv) send(t *testing.T, ev slackproto.Event) {
	t.Helper()
	e.h.Handle(context.Background(), ev)
	waitForIdle(t, e.h)
}

func TestIsBranchCommand(t *testing.T) {
	for _, tc := range []struct {
		in, arg string
		ok      bool
	}{
		{"!branch", "", true},
		{"  !branch  do it  ", "do it", true},
		{"!branch\nline two", "line two", true},
		{"!branches", "", false},
		{"hello !branch", "", false},
	} {
		arg, ok := isBranchCommand(tc.in)
		if arg != tc.arg || ok != tc.ok {
			t.Errorf("isBranchCommand(%q) = %q, %v", tc.in, arg, ok)
		}
	}
}

func TestBranchCommandOpensThreadWithFreshSession(t *testing.T) {
	e := newBranchEnv(t, Config{})
	e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.2", Text: "!branch try the other approach"})

	top := e.bs.topLevel()
	if len(top) != 1 {
		t.Fatalf("want one new top-level message, got %+v", e.bs.sent())
	}
	head := top[0]
	for _, want := range []string{
		"*try the other approach*",
		"<https://perma/C1/1.2|this message>",
		"by <@U1>",
		"> try the other approach",
	} {
		if !strings.Contains(head.text, want) {
			t.Errorf("opening message %q lacks %q", head.text, want)
		}
	}
	child := router.ConvKey{ChannelID: "C1", ThreadTS: head.ts}
	announce := e.bs.inThread("1.0")
	if len(announce) != 1 || !strings.Contains(announce[0].text, "<https://perma/C1/"+head.ts+"|try the other approach>") {
		t.Fatalf("origin thread announcement = %+v", announce)
	}
	if o, ok := e.r.OriginOf(child); !ok || o != (router.Origin{ChannelID: "C1", ThreadTS: "1.0", BranchTS: "1.2"}) {
		t.Fatalf("origin = %+v, %v", o, ok)
	}
	if o, ok := e.h.Origin(child.String()); !ok || o.BranchTS != "1.2" || o.ThreadTS != "1.0" {
		t.Fatalf("Handler.Origin = %+v, %v", o, ok)
	}
	// The origin has no session and the agent cannot list sessions:
	// the child starts fresh and is told how to reach the origin.
	if len(e.fa.forks) != 0 {
		t.Fatalf("nothing to fork, got %+v", e.fa.forks)
	}
	ps := e.promptsOf("sid")
	if len(ps) != 1 || !strings.Contains(ps[0], "history") || !strings.HasSuffix(ps[0], "try the other approach") {
		t.Fatalf("child prompts = %q", ps)
	}
	if len(e.bs.inThread(head.ts)) == 0 {
		t.Fatal("the child's turn must reply in the new thread")
	}
}

func TestBranchCommandForksLiveOrigin(t *testing.T) {
	e := newBranchEnv(t, Config{})
	e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "hello"})
	e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.3", Text: "!branch dig deeper"})

	top := e.bs.topLevel()
	if len(top) != 1 {
		t.Fatalf("posts = %+v", e.bs.sent())
	}
	if len(e.fa.forks) != 1 {
		t.Fatalf("forks = %+v", e.fa.forks)
	}
	f := e.fa.forks[0]
	if f.parent != "sid" {
		t.Fatalf("forked %q, want the origin's session", f.parent)
	}
	if want := filepath.Join(e.r.StateDir(), "threads", "C1", top[0].ts); f.cwd != want {
		t.Fatalf("fork filed in %q, want the CHILD's cwd %q", f.cwd, want)
	}
	ps := e.promptsOf("fork-1")
	if len(ps) != 1 || !strings.Contains(ps[0], "fork of that conversation") || !strings.HasSuffix(ps[0], "dig deeper") {
		t.Fatalf("forked child prompts = %q", ps)
	}
	if got := e.promptsOf("sid"); len(got) != 1 {
		t.Fatalf("the origin must not get the branch's prompt: %q", got)
	}
}

func TestBranchFallsBackWhenForkFails(t *testing.T) {
	for _, ferr := range []error{client.ErrForkUnsupported, errors.New("rpc broke")} {
		e := newBranchEnv(t, Config{})
		e.fa.forkErr = ferr
		e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "hello"})
		e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.3", Text: "!branch other"})
		if ps := e.promptsOf("sid"); len(ps) != 2 || !strings.Contains(ps[1], "You start without its context") {
			t.Fatalf("%v: fresh-session fallback prompts = %q", ferr, ps)
		}
	}
}

func TestBranchCommandUsageAndFailure(t *testing.T) {
	e := newBranchEnv(t, Config{})
	e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.1", Text: "!branch"})
	if p := e.bs.inThread("1.0"); len(p) != 1 || !strings.Contains(p[0].text, "Usage: `!branch <text>`") {
		t.Fatalf("usage reply = %+v", p)
	}
	e.bs.failTop = true
	e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.2", Text: "!branch x"})
	if p := e.bs.inThread("1.0"); len(p) != 2 || !strings.Contains(p[1].text, "Could not branch: posting the new thread") {
		t.Fatalf("failure reply = %+v", p)
	}
}

func TestBranchWithoutPermalinksOrUsableTS(t *testing.T) {
	e := newBranchEnv(t, Config{})
	e.bs.failLinks = true
	e.bs.badTS = true
	e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.2", Text: "!branch x"})
	top := e.bs.topLevel()
	if len(top) != 1 || !strings.Contains(top[0].text, "Branched from this message by") {
		t.Fatalf("opening message without a permalink = %+v", top)
	}
	if p := e.bs.inThread("1.0"); len(p) != 1 || !strings.HasSuffix(p[0].text, "Branched to x") {
		t.Fatalf("announcement without a permalink = %+v", p)
	}
}

func TestBranchSurvivesFailedAnnouncement(t *testing.T) {
	e := newBranchEnv(t, Config{})
	e.bs.failReply = true
	e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.2", Text: "!branch x"})
	if top := e.bs.topLevel(); len(top) != 1 {
		t.Fatalf("branch must still open: %+v", e.bs.sent())
	}
	if ps := e.promptsOf("sid"); len(ps) != 1 {
		t.Fatalf("child must still run: %q", ps)
	}
}

func TestBranchStripsBroadcastPings(t *testing.T) {
	e := newBranchEnv(t, Config{})
	e.bs.replies = []slack.Message{msg("1.0", "1.0", "U1", "", "parent")}
	res, _ := e.h.BranchTasks(context.Background(), "C1/1.0", []slackmcp.BranchTask{
		{Seed: "<!channel> look <!here|@here>", Title: "<!everyone> T > x"},
		{Seed: "<!channel>"},
	})
	waitForIdle(t, e.h)
	top := e.bs.topLevel()
	if len(top) != 1 || strings.Contains(top[0].text, "<!") || !strings.Contains(top[0].text, "> look") {
		t.Fatalf("opening = %+v", top)
	}
	if !strings.Contains(res[1].Error, "no text") {
		t.Fatalf("a seed of nothing but pings must fail: %+v", res)
	}
	if ann := e.bs.inThread("1.0"); len(ann) != 1 || !strings.Contains(ann[0].text, "|T &gt; x>") {
		t.Fatalf("link label must be escaped: %+v", ann)
	}
}

func TestWaitIdleHonoursContextWhileBranching(t *testing.T) {
	h := New(Config{})
	h.bg.Add(1)
	defer h.bg.Done()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.WaitIdle(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("WaitIdle = %v", err)
	}
}

func TestBranchTasksRateCap(t *testing.T) {
	e := newBranchEnv(t, Config{AgentPostsPerMinute: 1})
	e.bs.replies = []slack.Message{msg("1.0", "1.0", "U1", "", "parent")}
	res, _ := e.h.BranchTasks(context.Background(), "C1/1.0", []slackmcp.BranchTask{{Seed: "a"}, {Seed: "b"}})
	waitForIdle(t, e.h)
	if res[0].Error != "" || !strings.Contains(res[1].Error, "rate cap") {
		t.Fatalf("res = %+v", res)
	}
}

func TestMarkSet(t *testing.T) {
	var s markSet
	if !s.add("a") || s.add("a") || !s.add("b") || !s.add("c") {
		t.Fatal("add")
	}
	s.remove("b")
	s.remove("zz")
	if !s.add("b") {
		t.Fatal("a removed mark is new again")
	}
	for i := range maxBranchedMarks {
		s.add(fmt.Sprint(i))
	}
	if !s.add("a") {
		t.Fatal("the oldest mark must be forgotten past the bound")
	}
	if len(s.order) != maxBranchedMarks || len(s.set) != maxBranchedMarks {
		t.Fatalf("size %d/%d", len(s.order), len(s.set))
	}
}

func TestCommandJournalledAsCommand(t *testing.T) {
	recs := captureJournal(t)
	e := newBranchEnv(t, Config{})
	e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.1", Text: "!help"})
	e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.2", Text: "hi"})
	var got []string
	for _, r := range recs() {
		if r.Stage == journal.StageHandler {
			got = append(got, r.Reason)
		}
	}
	if strings.Join(got, ",") != journal.ReasonCommand+","+journal.ReasonPrompt {
		t.Fatalf("reasons = %v", got)
	}
}

func TestBranchHelpListed(t *testing.T) {
	e := newBranchEnv(t, Config{})
	e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.1", Text: "!help"})
	if p := e.bs.inThread("1.0"); len(p) != 1 || !strings.Contains(p[0].text, "!branch <text>") {
		t.Fatalf("!help = %+v", p)
	}
}

func TestBranchReaction(t *testing.T) {
	recs := captureJournal(t)
	e := newBranchEnv(t, Config{AllowedUserIDs: map[string]struct{}{"U1": {}}})
	e.bs.replies = []slack.Message{msg("1.0", "1.0", "U2", "", "parent"), msg("1.4", "1.0", "U2", "", "what about caching?")}
	react := func(user string) {
		e.h.HandleReaction(context.Background(), slackproto.Reaction{UserID: user, ChannelID: "C1", TS: "1.4"})
		waitForIdle(t, e.h)
	}
	react("U1")
	top := e.bs.topLevel()
	if len(top) != 1 || !strings.Contains(top[0].text, "*what about caching?*") || !strings.Contains(top[0].text, "<https://perma/C1/1.4|this message> by <@U1>") {
		t.Fatalf("reaction branch = %+v", e.bs.sent())
	}
	if o, _ := e.r.OriginOf(router.ConvKey{ChannelID: "C1", ThreadTS: top[0].ts}); o.ThreadTS != "1.0" || o.BranchTS != "1.4" {
		t.Fatalf("origin = %+v", o)
	}
	react("U1")     // re-added: no second branch
	react("intrud") // not allowed
	if len(e.bs.topLevel()) != 1 {
		t.Fatal("a message is branched once")
	}
	want := []string{journal.ReasonBranch, journal.ReasonBranchDuplicate, journal.ReasonAllowlist}
	var got []string
	for _, r := range recs() {
		if r.Path == journal.PathReaction {
			got = append(got, r.Reason)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("journal reasons = %v, want %v", got, want)
	}
}

func TestBranchReactionFailuresCanBeRetried(t *testing.T) {
	recs := captureJournal(t)
	e := newBranchEnv(t, Config{})
	react := func(ts string) {
		e.h.HandleReaction(context.Background(), slackproto.Reaction{UserID: "U1", ChannelID: "C1", TS: ts})
		waitForIdle(t, e.h)
	}
	e.bs.repErr = true
	react("1.4") // unreadable
	e.bs.repErr = false
	react("1.4") // not found (no replies configured) — retry was allowed
	e.bs.replies = []slack.Message{msg("1.4", "", "U2", "", "  ")}
	react("1.4") // empty text: branch fails
	e.bs.replies = []slack.Message{msg("1.4", "", "U2", "", "top-level idea")}
	react("1.4") // finally works; an unthreaded message is its own thread
	var got []string
	for _, r := range recs() {
		if r.Path == journal.PathReaction {
			got = append(got, r.Reason)
		}
	}
	want := []string{journal.ReasonBranchUnreadable, journal.ReasonBranchUnreadable, journal.ReasonBranchFailed, journal.ReasonBranch}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("journal reasons = %v, want %v", got, want)
	}
	if p := e.bs.inThread("1.4"); len(p) != 1 || !strings.Contains(p[0].text, "Branched to") {
		t.Fatalf("announcement in the reacted message's own thread = %+v", p)
	}
}

func TestBranchTasksTool(t *testing.T) {
	e := newBranchEnv(t, Config{Ambient: true})
	e.bs.replies = []slack.Message{
		msg("1.0", "1.0", "U1", "", "parent"),
		msg("1.1", "1.0", "U1", "", "first idea"),
		msg("1.2", "1.0", "UB", "B1", "relay reply"),
		msg("2.5", "2.0", "U1", "", "elsewhere"),
	}
	res, err := e.h.BranchTasks(context.Background(), "C1/1.0", []slackmcp.BranchTask{
		{Title: "From latest"},                              // latest human message, its text as seed
		{FromMsg: "1.0", Seed: "explicit seed", Title: "T"}, // explicit point and seed
		{FromMsg: "2.5"},                                    // other thread: refused
		{FromMsg: "9.9"},                                    // unknown
	})
	waitForIdle(t, e.h)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Error != "" || res[1].Error != "" || res[0].ThreadTS == "" || res[0].Link == "" || res[0].Channel != "C1" {
		t.Fatalf("results = %+v", res)
	}
	if !strings.Contains(res[2].Error, "not a message in this thread") || !strings.Contains(res[3].Error, "not found") {
		t.Fatalf("results = %+v", res)
	}
	top := e.bs.topLevel()
	if len(top) != 2 || !strings.Contains(top[0].text, "> first idea") || !strings.Contains(top[1].text, "> explicit seed") {
		t.Fatalf("opened = %+v", top)
	}
	if strings.Contains(top[0].text, " by <@") {
		t.Fatal("an agent-made branch names no person")
	}
	if o, _ := e.r.OriginOf(router.ConvKey{ChannelID: "C1", ThreadTS: top[0].ts}); o.BranchTS != "1.1" {
		t.Fatalf("latest-human branch point = %+v", o)
	}
	// One batched announcement, not one per branch.
	ann := e.bs.inThread("1.0")
	if len(ann) != 1 || strings.Count(ann[0].text, "\n• ") != 2 {
		t.Fatalf("announcement = %+v", ann)
	}
	// Ambient mode: an agent-made branch's prompt carries no "[user]".
	for _, sid := range []acp.SessionId{"sid"} {
		for _, p := range e.promptsOf(sid) {
			if strings.HasPrefix(p, "[") {
				t.Fatalf("prompt %q has a sender prefix", p)
			}
		}
	}
}

func TestBranchTasksToolFailures(t *testing.T) {
	e := newBranchEnv(t, Config{})
	if _, err := e.h.BranchTasks(context.Background(), "garbage", nil); err == nil {
		t.Fatal("a non-thread session key must fail")
	}
	e.bs.replies = []slack.Message{msg("1.2", "1.0", "UB", "B1", "only the relay")}
	res, _ := e.h.BranchTasks(context.Background(), "C1/1.0", []slackmcp.BranchTask{{}})
	if !strings.Contains(res[0].Error, "no message from a person") {
		t.Fatalf("res = %+v", res)
	}
	e.bs.repErr = true
	res, _ = e.h.BranchTasks(context.Background(), "C1/1.0", []slackmcp.BranchTask{{}})
	if !strings.Contains(res[0].Error, "reading this thread") {
		t.Fatalf("res = %+v", res)
	}
	if len(e.bs.sent()) != 0 {
		t.Fatalf("nothing made, nothing posted: %+v", e.bs.sent())
	}
}

func TestBranchTextHelpers(t *testing.T) {
	long := strings.Repeat("é", maxDerivedTitle+5)
	if got := deriveTitle("  a   b \nrest"); got != "a b" {
		t.Fatalf("deriveTitle = %q", got)
	}
	if got := []rune(deriveTitle(long)); len(got) != maxDerivedTitle || got[len(got)-1] != '…' {
		t.Fatalf("deriveTitle long = %q", string(got))
	}
	if got := quote("a\nb"); got != "> a\n> b" {
		t.Fatalf("quote = %q", got)
	}
	if slackLink("", "x") != "x" || slackLink("u", "x") != "<u|x>" {
		t.Fatal("slackLink")
	}
	if firstNonEmpty("", "b") != "b" || firstNonEmpty("a", "b") != "a" {
		t.Fatal("firstNonEmpty")
	}
}

func TestBranchForksAtContainingTurn(t *testing.T) {
	e := newBranchEnv(t, Config{})
	e.fa.leafFn = func(_ acp.SessionId, blocks []acp.ContentBlock) string {
		return "leaf:" + blocks[len(blocks)-1].Text.Text
	}
	origin := router.ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "one"})
	e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "101.5", Text: "two"})

	replies := e.bs.inThread("1.0")
	if len(replies) == 0 {
		t.Fatal("no replies")
	}
	// Fake bot posts have ts 101.0, 102.0, …; prompts interleave.
	// The first reply belongs to the first turn.
	if got := e.r.TurnLeaf(origin, replies[0].ts); !strings.HasSuffix(got, "one") {
		t.Fatalf("leaf at first reply = %q", got)
	}
	// A message after the second turn maps to it.
	e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "150.0", Text: "!branch dig deeper"})
	if len(e.fa.forks) != 1 || !strings.HasSuffix(e.fa.forks[0].at, "two") {
		t.Fatalf("forks = %+v, want at the second turn's leaf", e.fa.forks)
	}
}

func TestTurnLeafWriteFailureIsLogged(t *testing.T) {
	e := newBranchEnv(t, Config{})
	e.fa.leafFn = func(acp.SessionId, []acp.ContentBlock) string { return "L" }
	// A directory where turns.json belongs makes the write fail.
	if err := os.MkdirAll(filepath.Join(e.r.StateDir(), "threads", "C1", "1.0", "turns.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "one"})
	if len(e.bs.inThread("1.0")) == 0 {
		t.Fatal("the turn must still reply")
	}
	if got := e.r.TurnLeaf(router.ConvKey{ChannelID: "C1", ThreadTS: "1.0"}, "1.0"); got != "" {
		t.Fatalf("leaf = %q", got)
	}
}
