package handler

import (
	"context"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/origin"

	"github.com/kfet/slack-acp/internal/router"
	"github.com/kfet/slack-acp/internal/slackproto"
)

func TestOriginOf(t *testing.T) {
	ev := slackproto.Event{TeamID: "T1", UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.1", IsDM: true, IsMention: true}
	want := origin.Origin{Platform: "slack", TeamID: "T1", UserID: "U1", ChannelID: "C1", ThreadID: "1.0", MessageID: "1.1", IsDM: true, IsMention: true}
	if got := originOf(ev); got != want {
		t.Fatalf("originOf = %+v", got)
	}
}

// TestPromptCarriesOriginNotIdentityText: the sender reaches the agent
// in the origin _meta only — never as a "[name]" prefix in the text,
// even in an ambient shared thread.
func TestPromptCarriesOriginNotIdentityText(t *testing.T) {
	e := newBranchEnv(t, Config{Ambient: true})
	var got origin.Origin
	e.fa.promptHook = func(ctx context.Context, sid acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
		got, _ = origin.From(ctx)
		e.mu.Lock()
		e.prompts[sid] = append(e.prompts[sid], blocks[0].Text.Text)
		e.mu.Unlock()
		return acp.StopReasonEndTurn, nil
	}
	e.send(t, slackproto.Event{UserID: "U1", BotUserID: "UBOT", TeamID: "T1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.1", Text: "hello", IsMention: true})
	if p := e.promptsOf("sid"); len(p) != 1 || p[0] != "hello" {
		t.Fatalf("prompt = %q", p)
	}
	if got.UserID != "U1" || got.Platform != "slack" || got.MessageID != "1.1" || got.TeamID != "T1" {
		t.Fatalf("origin = %+v", got)
	}
}

func TestThreadUserScope(t *testing.T) {
	e := newBranchEnv(t, Config{PerUserSessions: true})
	e.fa.sessionModels = []string{"a/m", "b/m"}
	e.send(t, slackproto.Event{UserID: "UA", ChannelID: "C1", ThreadTS: "1.0", TS: "1.1", Text: "from a"})
	e.send(t, slackproto.Event{UserID: "UB", ChannelID: "C1", ThreadTS: "1.0", TS: "1.2", Text: "from b"})
	if a, b := e.promptsOf("sid-0"), e.promptsOf("sid-1"); len(a) != 1 || a[0] != "from a" || len(b) != 1 || b[0] != "from b" {
		t.Fatalf("sessions: a=%q b=%q", a, b)
	}
	if e.r.Len() != 2 {
		t.Fatalf("live sessions = %d", e.r.Len())
	}
	posts := e.bs.inThread("1.0")
	var sawA, sawB bool
	for _, p := range posts {
		sawA = sawA || strings.HasPrefix(p.text, "<@UA> ")
		sawB = sawB || strings.HasPrefix(p.text, "<@UB> ")
	}
	if !sawA || !sawB {
		t.Fatalf("replies do not address their users: %+v", posts)
	}
	// A command reply addresses its sender too.
	e.send(t, slackproto.Event{UserID: "UA", ChannelID: "C1", ThreadTS: "1.0", TS: "1.3", Text: "!help"})
	if p := e.bs.inThread("1.0"); !strings.HasPrefix(p[len(p)-1].text, "<@UA> ") {
		t.Fatalf("command reply = %q", p[len(p)-1].text)
	}
	// A DM has one human: no mention.
	e.send(t, slackproto.Event{UserID: "UA", ChannelID: "D1", ThreadTS: "5.0", TS: "5.0", Text: "!help", IsDM: true})
	if p := e.bs.inThread("5.0"); len(p) == 0 || strings.HasPrefix(p[0].text, "<@") {
		t.Fatalf("DM reply = %+v", p)
	}
}

// TestThreadUserSupersedeIsPerUser: a user's new message supersedes only
// their own running turn, never another user's in the same thread.
func TestThreadUserSupersedeIsPerUser(t *testing.T) {
	e := newBranchEnv(t, Config{PerUserSessions: true})
	e.fa.sessionModels = []string{"a/m", "b/m"}
	aCtx := make(chan context.Context, 1)
	ran := make(chan string, 4)
	e.fa.promptHook = func(ctx context.Context, _ acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
		text := blocks[0].Text.Text
		if text != "a1" {
			ran <- text
			return acp.StopReasonEndTurn, nil
		}
		aCtx <- ctx
		<-ctx.Done()
		return acp.StopReasonCancelled, ctx.Err()
	}
	bg := context.Background()
	e.h.Handle(bg, slackproto.Event{UserID: "UA", ChannelID: "C1", ThreadTS: "1.0", TS: "1.1", Text: "a1"})
	a := <-aCtx
	e.h.Handle(bg, slackproto.Event{UserID: "UB", ChannelID: "C1", ThreadTS: "1.0", TS: "1.2", Text: "b1"})
	if got := <-ran; got != "b1" {
		t.Fatalf("ran %q", got)
	}
	if a.Err() != nil {
		t.Fatal("another user's message superseded A's turn")
	}
	e.h.Handle(bg, slackproto.Event{UserID: "UA", ChannelID: "C1", ThreadTS: "1.0", TS: "1.3", Text: "a2"})
	if got := <-ran; got != "a2" {
		t.Fatalf("ran %q", got)
	}
	if a.Err() == nil {
		t.Fatal("A's follow-up did not supersede A's turn")
	}
	waitForIdle(t, e.h)
}

// TestAgentCwdLoginIsTheSenders: under cwd_mode "agent" anyone may log
// in, and a pending login belongs to its sender — another user's next
// message in the thread is a prompt, not that login's redirect.
func TestAgentCwdLoginIsTheSenders(t *testing.T) {
	e := newBranchEnv(t, Config{AgentCwd: true})
	at := func(user, ts, text string) {
		e.send(t, slackproto.Event{UserID: user, ChannelID: "C1", ThreadTS: "1.0", TS: ts, Text: text})
	}
	at("UA", "1.1", "!login oauth")
	for _, p := range e.bs.inThread("1.0") {
		if strings.Contains(p.text, "owner-only") {
			t.Fatalf("login refused: %+v", p)
		}
	}
	conv := router.ConvKey{ChannelID: "C1", ThreadTS: "1.0"}.String()
	if !e.h.convo.Broker().HasPending(conv + "#UA") {
		t.Fatal("login not pending for its sender")
	}
	at("UB", "1.2", "https://example.com/other")
	if got := e.promptsOf("sid"); len(got) != 1 || !strings.Contains(got[0], "example.com/other") {
		t.Fatalf("other user's text not a prompt: %v", got)
	}
	at("UA", "1.3", "https://example.com/cb?code=x")
	e.fa.mu.Lock()
	redirects := append([]string(nil), e.fa.redirects...)
	e.fa.mu.Unlock()
	if len(redirects) != 1 || !strings.Contains(redirects[0], "code=x") {
		t.Fatalf("redirects = %v", redirects)
	}
	// Other admin verbs stay owner-only.
	at("UA", "1.4", "!update")
	if p := e.bs.inThread("1.0"); !strings.Contains(p[len(p)-1].text, "owner-only") {
		t.Fatalf("!update = %+v", p[len(p)-1])
	}
	// Session commands still find the thread's conversation via the token.
	at("UA", "1.5", "!status")
	if p := e.bs.inThread("1.0"); !strings.Contains(p[len(p)-1].text, "sid") {
		t.Fatalf("!status = %q", p[len(p)-1].text)
	}
}

func TestTokenOf(t *testing.T) {
	h := &Handler{cfg: Config{AgentCwd: true, PerUserSessions: true}}
	if h.tokenOf("C/1", slackproto.Event{UserID: "U"}) != "" {
		t.Fatal("per-user conversation needs no sender token")
	}
	h.cfg.PerUserSessions = false
	if h.tokenOf("C/1", slackproto.Event{}) != "" {
		t.Fatal("no sender, no token")
	}
	if h.tokenOf("C/1", slackproto.Event{UserID: "U"}) != "C/1#U" {
		t.Fatal("sender token")
	}
	if c, ok := resolveToken("C/1#U"); !ok || c != "C/1" {
		t.Fatalf("resolveToken = %q", c)
	}
}
