package handler

import (
	"context"

	"github.com/kfet/acp-kit/client"
	"github.com/kfet/slack-acp/internal/journal"
	"github.com/kfet/slack-acp/internal/router"
	"strings"
	"testing"

	"github.com/kfet/slack-acp/internal/slackproto"
)

func TestAdminVerb(t *testing.T) {
	for text, want := range map[string]string{
		"!fir auth refresh": "fir", "!RUN x": "run", "/skills": "skills",
		"!update --check": "update", "!login anthropic": "login", ".logins": "logins",
		"!cancel-login": "cancel-login", "  !logout": "logout",
		"!login\tanthropic": "login", "!fir\nx": "fir",
	} {
		if v, ok := adminVerb(text); !ok || v != want {
			t.Errorf("adminVerb(%q) = %q,%v", text, v, ok)
		}
	}
	for _, text := range []string{"hi", "!help", "!model", "!firx", "fir", "!", "!!login"} {
		if v, ok := adminVerb(text); ok {
			t.Errorf("adminVerb(%q) = %q, want none", text, v)
		}
	}
}

func TestOwnerGate(t *testing.T) {
	owners := map[string]struct{}{"UOWN": {}}
	cases := []struct {
		name    string
		cfg     Config
		ev      slackproto.Event
		refused bool
	}{
		{"non-owner refused", Config{OwnerUserIDs: owners}, slackproto.Event{UserID: "U1", Text: "!login"}, true},
		{"no owners fails closed", Config{}, slackproto.Event{UserID: "U1", Text: "!update"}, true},
		{"self-drive refused", Config{OwnerUserIDs: owners, SelfDrive: slackproto.NewSelfDrive("!!drive!!"), SelfDrivePerMinute: 4}, slackproto.Event{UserID: "UOWN", SelfDrive: true, Text: "!fir x"}, true},
		{"owner passes", Config{OwnerUserIDs: owners}, slackproto.Event{UserID: "UOWN", Text: "!login"}, false},
		{"non-admin passes", Config{OwnerUserIDs: owners}, slackproto.Event{UserID: "U1", Text: "!help"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			recs := captureJournal(t)
			e := newBranchEnv(t, c.cfg)
			c.ev.ChannelID, c.ev.ThreadTS, c.ev.TS = "C1", "1.0", "1.1"
			e.send(t, c.ev)
			refused := false
			for _, p := range e.bs.inThread("1.0") {
				if strings.Contains(p.text, "owner-only") {
					refused = true
				}
			}
			if refused != c.refused {
				t.Fatalf("refused = %v, posts %+v", refused, e.bs.inThread("1.0"))
			}
			journalled := false
			for _, r := range recs() {
				if r.Stage == journal.StageHandler && r.Decision == journal.DecisionDrop && r.Reason == journal.ReasonOwnerOnly {
					journalled = true
				}
			}
			if journalled != c.refused {
				t.Fatalf("owner_only journalled = %v", journalled)
			}
		})
	}
}

// fakeAgent is a command.Authenticator whose logins wait for a
// redirect URL.

func (f *fakeAgent) AuthMethods() []client.AuthMethod {
	return []client.AuthMethod{{ID: "oauth", Name: "OAuth", Type: "agent"}}
}

func (f *fakeAgent) Authenticate(_ context.Context, _, _, redirect string, _ bool) (client.AuthResult, error) {
	if redirect == "" {
		return client.AuthResult{State: "needs_redirect", ID: "p1", URL: "https://example.com/auth"}, nil
	}
	f.mu.Lock()
	f.redirects = append(f.redirects, redirect)
	f.mu.Unlock()
	return client.AuthResult{State: "ok"}, nil
}

func TestNonOwnerCannotCompleteOwnerLogin(t *testing.T) {
	e := newBranchEnv(t, Config{OwnerUserIDs: map[string]struct{}{"UOWN": {}}})
	at := func(user, ts, text string) {
		e.send(t, slackproto.Event{UserID: user, ChannelID: "C1", ThreadTS: "1.0", TS: ts, Text: text})
	}
	at("UOWN", "1.1", "!login oauth")
	if !e.h.convo.Broker().HasPending(router.ConvKey{ChannelID: "C1", ThreadTS: "1.0"}.String()) {
		t.Fatal("owner login not pending")
	}
	at("U1", "1.2", "https://evil.example.com/cb")
	at("U1", "1.21", "!help")
	if p := e.bs.inThread("1.0"); !strings.Contains(p[len(p)-1].text, "!login") {
		t.Fatalf("non-owner !help during a pending login = %+v", p[len(p)-1])
	}
	e.fa.mu.Lock()
	n := len(e.fa.redirects)
	e.fa.mu.Unlock()
	if n != 0 {
		t.Fatal("non-owner text completed the login")
	}
	if got := e.promptsOf("sid"); len(got) != 1 || !strings.Contains(got[0], "evil.example.com") {
		t.Fatalf("non-owner text not forwarded to the agent: %v", got)
	}
	at("UOWN", "1.3", "https://example.com/cb?code=x")
	e.fa.mu.Lock()
	defer e.fa.mu.Unlock()
	if len(e.fa.redirects) != 1 {
		t.Fatalf("owner redirect = %v", e.fa.redirects)
	}
}

func TestOwnerRefusalPostFailureStillHandled(t *testing.T) {
	e := newBranchEnv(t, Config{})
	e.bs.failReply = true
	e.send(t, slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.1", Text: "!login"})
	if got := e.promptsOf("sid"); len(got) != 0 {
		t.Fatalf("refused command reached the agent: %v", got)
	}
}
