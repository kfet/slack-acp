package handler

import (
	"context"
	"testing"
	"time"

	"github.com/kfet/slack-acp/internal/slackproto"
)

func TestAllowed(t *testing.T) {
	h := &Handler{cfg: Config{
		AllowedUserIDs:    map[string]struct{}{"U1": {}},
		AllowedChannelIDs: map[string]struct{}{"C1": {}},
		PromptTimeout:     time.Second,
	}}
	if !h.allowed(slackproto.Event{UserID: "U1", ChannelID: "C1"}) {
		t.Fatal("expected allowed")
	}
	if h.allowed(slackproto.Event{UserID: "U2", ChannelID: "C1"}) {
		t.Fatal("user not in allowlist should be denied")
	}
	if h.allowed(slackproto.Event{UserID: "U1", ChannelID: "C2"}) {
		t.Fatal("channel not in allowlist should be denied")
	}
	// Empty allowlist = open.
	h2 := &Handler{cfg: Config{}}
	if !h2.allowed(slackproto.Event{UserID: "anyone", ChannelID: "any"}) {
		t.Fatal("empty allowlist should allow all")
	}
}

// Compile-time assertion that *Handler satisfies slackproto.Handler.
var _ slackproto.Handler = (*Handler)(nil)

func TestNewDefaultsTimeout(t *testing.T) {
	h := New(Config{})
	if h.cfg.PromptTimeout == 0 {
		t.Fatal("default timeout not set")
	}
	_ = context.Background()
}
