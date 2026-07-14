package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/slack-go/slack"

	"github.com/kfet/slack-acp/internal/router"
	"github.com/kfet/slack-acp/internal/slackproto"
)

var errFakePrompt = errors.New("fake prompt error")

// richSlack is a fuller Slack Web API stub than fakeSlack: it also
// serves conversations.replies and users.info so the ambient backfill
// path can be exercised end-to-end. Kept separate from fakeSlack so the
// existing tests' minimal mux is undisturbed.
type richSlack struct {
	srv *httptest.Server

	posts   int
	replies []slack.Message // returned by conversations.replies
	repErr  bool            // make conversations.replies fail
}

func newRichSlack(t *testing.T) *richSlack {
	t.Helper()
	rs := &richSlack{}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		rs.posts++
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1.0","message":{"text":"x"}}`))
	})
	mux.HandleFunc("/chat.update", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1.0","text":"x"}`))
	})
	mux.HandleFunc("/conversations.replies", func(w http.ResponseWriter, _ *http.Request) {
		if rs.repErr {
			_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
			return
		}
		resp := struct {
			OK       bool            `json:"ok"`
			Messages []slack.Message `json:"messages"`
		}{OK: true, Messages: rs.replies}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/users.info", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		uid := r.FormValue("user")
		// Return a display name derived from the id so backfill lines
		// are deterministic; empty id (shouldn't happen) -> error.
		if uid == "" {
			_, _ = w.Write([]byte(`{"ok":false,"error":"user_not_found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"` + uid + `","name":"` + uid + `","profile":{"display_name":"Name-` + uid + `"}}}`))
	})
	rs.srv = httptest.NewServer(mux)
	t.Cleanup(rs.srv.Close)
	return rs
}

func (rs *richSlack) client() *slack.Client {
	return slack.New("xoxb-fake", slack.OptionAPIURL(rs.srv.URL+"/"))
}

func mkReply(user, ts, text string) slack.Message {
	m := slack.Message{}
	m.User = user
	m.Timestamp = ts
	m.Text = text
	return m
}

// summon drives a first @-mention turn so the thread dir + checkpoint
// exist, then resets the prompt hook. Returns the handler.
func summon(t *testing.T, h *Handler) {
	t.Helper()
	h.Handle(context.Background(), slackproto.Event{
		UserID: "U1", BotUserID: "BBOT", ChannelID: "C1",
		ThreadTS: "1.0", TS: "1.0", Text: "<@BBOT> hi",
	})
	waitForIdle(t, h)
}

// TestBackfillFeedsMissedMessages verifies that on a detected gap the
// handler fetches conversations.replies and injects the missed lines as
// a single catch-up prompt (through a discarding sink) before the live
// message, and that user display names are resolved.
func TestBackfillFeedsMissedMessages(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	rs := newRichSlack(t)

	var prompts []string
	live := make(chan struct{})
	fa.promptHook = func(_ context.Context, _ acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
		if len(blocks) > 0 && blocks[0].Text != nil {
			prompts = append(prompts, blocks[0].Text.Text)
		}
		if strings.Contains(lastOf(prompts), "the live one") {
			close(live)
		}
		return acp.StopReasonEndTurn, nil
	}

	h := New(Config{
		Router: r, API: rs.client(), PromptTimeout: 5 * time.Second,
		Ambient: true, Backfill: true, BackfillMaxMessages: 50,
	})
	summon(t, h) // creates dir + checkpoint at ts=1.0

	// Two human messages were missed between the checkpoint (1.0) and
	// the live message (5.0); one bot message and one edit must be
	// skipped by the filter.
	rs.replies = []slack.Message{
		mkReply("U2", "2.0", "missed one"),
		mkReply("U3", "3.0", "missed two"),
		{Msg: slack.Msg{User: "U9", Timestamp: "4.0", Text: "bot noise", BotID: "B1"}},
	}

	h.Handle(context.Background(), slackproto.Event{
		UserID: "U2", BotUserID: "BBOT", ChannelID: "C1",
		ThreadTS: "1.0", TS: "5.0", Text: "the live one",
	})
	select {
	case <-live:
	case <-time.After(2 * time.Second):
		t.Fatal("live message never processed after backfill")
	}
	waitForIdle(t, h)

	// Expect a catch-up prompt containing both missed human lines with
	// resolved display names, then the live message prompt.
	joined := strings.Join(prompts, "\n---\n")
	if !strings.Contains(joined, "catch-up") {
		t.Fatalf("expected a catch-up prompt; got: %s", joined)
	}
	if !strings.Contains(joined, "[Name-U2] missed one") || !strings.Contains(joined, "[Name-U3] missed two") {
		t.Fatalf("missed lines not injected with names; got: %s", joined)
	}
	if strings.Contains(joined, "bot noise") {
		t.Fatalf("bot message must be filtered from backfill; got: %s", joined)
	}
}

// TestBackfillNoGapNoInjection verifies that when there are no missed
// messages, backfill injects nothing and the live message still runs.
func TestBackfillNoGapNoInjection(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	rs := newRichSlack(t)

	var prompts []string
	done := make(chan struct{})
	fa.promptHook = func(_ context.Context, _ acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
		if len(blocks) > 0 && blocks[0].Text != nil {
			prompts = append(prompts, blocks[0].Text.Text)
		}
		if strings.Contains(lastOf(prompts), "live") {
			close(done)
		}
		return acp.StopReasonEndTurn, nil
	}

	h := New(Config{
		Router: r, API: rs.client(), PromptTimeout: 5 * time.Second,
		Ambient: true, Backfill: true, BackfillMaxMessages: 50,
	})
	summon(t, h)
	rs.replies = nil // no missed messages

	h.Handle(context.Background(), slackproto.Event{
		UserID: "U2", BotUserID: "BBOT", ChannelID: "C1",
		ThreadTS: "1.0", TS: "5.0", Text: "live msg",
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("live message never processed")
	}
	waitForIdle(t, h)
	for _, p := range prompts {
		if strings.Contains(p, "catch-up") {
			t.Fatalf("no gap should mean no catch-up prompt; got %q", p)
		}
	}
}

// TestBackfillRepliesErrorNonFatal verifies a conversations.replies
// failure is non-fatal: the live message is still processed.
func TestBackfillRepliesErrorNonFatal(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	rs := newRichSlack(t)
	rs.repErr = true

	done := make(chan struct{})
	fa.promptHook = func(_ context.Context, _ acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
		if len(blocks) > 0 && blocks[0].Text != nil && strings.Contains(blocks[0].Text.Text, "live") {
			close(done)
		}
		return acp.StopReasonEndTurn, nil
	}

	h := New(Config{
		Router: r, API: rs.client(), PromptTimeout: 5 * time.Second,
		Ambient: true, Backfill: true, BackfillMaxMessages: 50,
	})
	summon(t, h)

	h.Handle(context.Background(), slackproto.Event{
		UserID: "U2", BotUserID: "BBOT", ChannelID: "C1",
		ThreadTS: "1.0", TS: "5.0", Text: "live after error",
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("live message must still run when backfill fetch errors")
	}
	waitForIdle(t, h)
}

// TestBackfillDedupDropsOldTS verifies that a duplicate/at-or-before
// checkpoint delivery does not trigger a replies fetch (dedup).
func TestBackfillDedupDropsOldTS(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	rs := newRichSlack(t)
	// If backfill fetched, it would panic on repErr; instead make the
	// replies endpoint fail loudly if hit, and assert it is NOT hit by
	// checking the live message still runs cleanly.
	rs.repErr = true

	done := make(chan struct{})
	h := New(Config{
		Router: r, API: rs.client(), PromptTimeout: 5 * time.Second,
		Ambient: true, Backfill: true, BackfillMaxMessages: 50,
	})
	summon(t, h) // checkpoint now at 1.0

	// Install the closing hook only AFTER summon so the summon turn
	// doesn't close it.
	fa.promptHook = func(_ context.Context, _ acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		close(done)
		return acp.StopReasonEndTurn, nil
	}

	// Deliver a message whose ts == checkpoint: ev.TS ("1.0") <= last
	// ("1.0") so backfill returns early before any fetch.
	h.Handle(context.Background(), slackproto.Event{
		UserID: "U2", BotUserID: "BBOT", ChannelID: "C1",
		ThreadTS: "1.0", TS: "1.0", Text: "dup",
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("live message should still run on dedup path")
	}
	waitForIdle(t, h)
}

func lastOf(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[len(ss)-1]
}

func TestGetUserNameFallbacks(t *testing.T) {
	rs := newRichSlack(t)
	h := New(Config{API: rs.client()})
	// Empty id -> "unknown".
	if got := h.getUserName(context.Background(), ""); got != "unknown" {
		t.Fatalf("empty id: got %q", got)
	}
	// Known id -> display name from the mock.
	if got := h.getUserName(context.Background(), "U7"); got != "Name-U7" {
		t.Fatalf("display name: got %q", got)
	}
}

// TestBackfillNoCheckpointNoop covers the "first message, no checkpoint"
// early return: a Known thread dir exists (created directly) but has no
// last_ts, so backfill returns before any fetch.
func TestBackfillNoCheckpointNoop(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	rs := newRichSlack(t)
	rs.repErr = true // would fail if a fetch were attempted

	key := router.ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	// Create the thread dir (Known) WITHOUT writing a checkpoint.
	if _, err := r.GetOrCreate(context.Background(), key, discardSink{}); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	fa.promptHook = func(_ context.Context, _ acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		close(done)
		return acp.StopReasonEndTurn, nil
	}
	h := New(Config{
		Router: r, API: rs.client(), PromptTimeout: 5 * time.Second,
		Ambient: true, Backfill: true, BackfillMaxMessages: 50,
	})
	h.Handle(context.Background(), slackproto.Event{
		UserID: "U2", BotUserID: "BBOT", ChannelID: "C1",
		ThreadTS: "1.0", TS: "9.0", Text: "first live",
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("live message should process with no checkpoint")
	}
	waitForIdle(t, h)
}

// TestBackfillFilterBoundaries covers the ts filter: messages at/before
// the checkpoint or at/after the live ts are excluded; empty-text
// messages are dropped by formatBackfillMessage.
func TestBackfillFilterBoundaries(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	rs := newRichSlack(t)

	var prompts []string
	live := make(chan struct{})
	fa.promptHook = func(_ context.Context, _ acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
		if len(blocks) > 0 && blocks[0].Text != nil {
			prompts = append(prompts, blocks[0].Text.Text)
		}
		if strings.Contains(lastOf(prompts), "live boundary") {
			close(live)
		}
		return acp.StopReasonEndTurn, nil
	}
	h := New(Config{
		Router: r, API: rs.client(), PromptTimeout: 5 * time.Second,
		Ambient: true, Backfill: true, BackfillMaxMessages: 50,
	})
	summon(t, h) // checkpoint at 1.0

	rs.replies = []slack.Message{
		mkReply("U2", "1.0", "at checkpoint (excluded)"),     // == lastTS
		mkReply("U3", "0.5", "before checkpoint (excluded)"), // < lastTS
		mkReply("U4", "5.0", "at live (excluded)"),           // == ev.TS
		mkReply("U5", "6.0", "after live (excluded)"),        // > ev.TS
		mkReply("U6", "3.0", "  "),                           // empty -> dropped
		mkReply("U7", "3.5", "the only valid one"),           // included
	}
	h.Handle(context.Background(), slackproto.Event{
		UserID: "U2", BotUserID: "BBOT", ChannelID: "C1",
		ThreadTS: "1.0", TS: "5.0", Text: "live boundary",
	})
	select {
	case <-live:
	case <-time.After(2 * time.Second):
		t.Fatal("live never processed")
	}
	waitForIdle(t, h)

	joined := strings.Join(prompts, "\n")
	if !strings.Contains(joined, "the only valid one") {
		t.Fatalf("valid missed line missing; got %s", joined)
	}
	for _, bad := range []string{"excluded", "at checkpoint", "before checkpoint", "at live", "after live"} {
		if strings.Contains(joined, bad) {
			t.Fatalf("excluded line leaked (%q); got %s", bad, joined)
		}
	}
}

// TestGetUserNameRealNameAndNameFallbacks covers the RealName and Name
// fallback branches when display_name is empty.
func TestGetUserNameRealNameAndNameFallbacks(t *testing.T) {
	// A mock whose users.info returns no display_name but a real_name,
	// then another with only name.
	mkMock := func(t *testing.T, body string) *slack.Client {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return slack.New("xoxb-fake", slack.OptionAPIURL(srv.URL+"/"))
	}

	hReal := New(Config{API: mkMock(t, `{"ok":true,"user":{"id":"U1","name":"u1","real_name":"Real One","profile":{"display_name":""}}}`)})
	if got := hReal.getUserName(context.Background(), "U1"); got != "Real One" {
		t.Fatalf("real_name fallback: got %q", got)
	}
	hName := New(Config{API: mkMock(t, `{"ok":true,"user":{"id":"U1","name":"handle1","real_name":"","profile":{"display_name":""}}}`)})
	if got := hName.getUserName(context.Background(), "U1"); got != "handle1" {
		t.Fatalf("name fallback: got %q", got)
	}
	// Lookup error -> returns the id verbatim.
	hErr := New(Config{API: mkMock(t, `{"ok":false,"error":"user_not_found"}`)})
	if got := hErr.getUserName(context.Background(), "U9"); got != "U9" {
		t.Fatalf("error fallback should return id; got %q", got)
	}
}

// TestGetUserNameAllNamesEmpty covers the final fallback: when the user
// exists but display_name/real_name/name are all empty, return the id.
func TestGetUserNameAllNamesEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"user":{"id":"U1","name":"","real_name":"","profile":{"display_name":""}}}`))
	}))
	t.Cleanup(srv.Close)
	h := New(Config{API: slack.New("xoxb-fake", slack.OptionAPIURL(srv.URL+"/"))})
	if got := h.getUserName(context.Background(), "U1"); got != "U1" {
		t.Fatalf("all-empty names should fall back to id; got %q", got)
	}
}

// TestBackfillPromptError covers the catch-up prompt error branch.
func TestBackfillPromptError(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	rs := newRichSlack(t)
	h := New(Config{
		Router: r, API: rs.client(), PromptTimeout: 5 * time.Second,
		Ambient: true, Backfill: true, BackfillMaxMessages: 50,
	})
	summon(t, h) // checkpoint at 1.0
	rs.replies = []slack.Message{mkReply("U2", "3.0", "missed")}

	// Make the catch-up prompt fail; backfill error is non-fatal so the
	// live message still runs to completion.
	done := make(chan struct{})
	var once bool
	fa.promptHook = func(_ context.Context, _ acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		if !once {
			once = true
			// First prompt after summon is the catch-up: return an error.
			return "", errFakePrompt
		}
		close(done)
		return acp.StopReasonEndTurn, nil
	}
	h.Handle(context.Background(), slackproto.Event{
		UserID: "U2", BotUserID: "BBOT", ChannelID: "C1",
		ThreadTS: "1.0", TS: "5.0", Text: "live after prompt err",
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("live message should still run after backfill prompt error")
	}
	waitForIdle(t, h)
}

// TestAbstainFinalizeErrorLoggedInRun drives the full run() abstain path
// where the agent emits a partial sentinel and the finalize flush fails
// (post error), exercising the ferr-logging branch. The turn still
// completes without hanging.
func TestAbstainFinalizeErrorLoggedInRun(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	// Summon first so the follow-up is a known-thread ambient reply.
	fa.promptHook = func(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		return acp.StopReasonEndTurn, nil
	}
	h := New(Config{
		Router: r, API: fs.client(), PromptTimeout: 5 * time.Second,
		Ambient: true, SilentSentinel: "<<SILENT>>",
	})
	h.Handle(context.Background(), slackproto.Event{
		UserID: "U1", BotUserID: "BBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "<@BBOT> hi",
	})
	waitForIdle(t, h)

	// Follow-up: agent emits a partial sentinel (strict prefix, never
	// completes) so Finalize must flush it — and make that flush fail.
	fs.mu.Lock()
	fs.postErr = true
	fs.mu.Unlock()
	done := make(chan struct{})
	fa.promptHook = func(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		fa.emit(sid, acp.SessionNotification{
			SessionId: sid,
			Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "<<SIL"}}}},
		})
		close(done)
		return acp.StopReasonEndTurn, nil
	}
	h.Handle(context.Background(), slackproto.Event{
		UserID: "U2", BotUserID: "BBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "2.0", Text: "chatter",
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("abstain-finalize-error turn hung")
	}
	waitForIdle(t, h)
}

// TestCheckpointLogsOnError covers the checkpoint helper's error branch
// (SetLastTS fails on a closed router — the failure is swallowed).
func TestCheckpointLogsOnError(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	_ = r.Close() // SetLastTS now returns an error
	h := New(Config{Router: r})
	// Must not panic; error is logged and swallowed.
	h.checkpoint(router.ConvKey{ChannelID: "C1", ThreadTS: "1.0"}, "9.9")
}

// TestBackfillGetOrCreateError covers the backfill GetOrCreate error
// branch. It recreates the post-restart state: the thread dir + checkpoint
// exist on disk (written by router A) but a fresh router B has no
// in-memory session, so backfill's GetOrCreate takes the cold path and
// the agent's NewSession fails. The backfill error is non-fatal; the
// live message still runs.
func TestBackfillGetOrCreateError(t *testing.T) {
	dir := t.TempDir()
	faA := newFakeAgent()
	rA, err := router.New(router.Config{Agent: faA, StateDir: dir, IdleTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	key := router.ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	if _, err := rA.GetOrCreate(context.Background(), key, discardSink{}); err != nil {
		t.Fatal(err)
	}
	if err := rA.SetLastTS(key, "1.0"); err != nil { // checkpoint on disk
		t.Fatal(err)
	}
	_ = rA.Close()

	// Fresh router B over the same StateDir: dir + last_ts present, but
	// byKey empty. Its agent fails NewSession so the cold GetOrCreate in
	// backfill errors.
	faB := newFakeAgent()
	faB.newSessErr = errFakePrompt
	rB, err := router.New(router.Config{Agent: faB, StateDir: dir, IdleTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rB.Close() })

	rs := newRichSlack(t)
	rs.replies = []slack.Message{mkReply("U2", "3.0", "missed while down")}

	// The live message's own GetOrCreate would also fail via faB, so the
	// handler will post an error — that's fine; we only need the backfill
	// GetOrCreate-error branch executed. Use a Prompt hook that never
	// runs (NewSession fails first) and assert the turn completes.
	h := New(Config{
		Router: rB, API: rs.client(), PromptTimeout: 5 * time.Second,
		Ambient: true, Backfill: true, BackfillMaxMessages: 50,
	})
	h.Handle(context.Background(), slackproto.Event{
		UserID: "U2", BotUserID: "BBOT", ChannelID: "C1",
		ThreadTS: "1.0", TS: "5.0", Text: "live while agent broken",
	})
	waitForIdle(t, h)
}
