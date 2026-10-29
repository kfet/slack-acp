package handler

import (
	"context"
	"strings"

	"github.com/kfet/acp-kit/origin"

	"github.com/kfet/slack-acp/internal/router"
	"github.com/kfet/slack-acp/internal/slackproto"
)

// Platform is the origin _meta platform this relay reports.
const Platform = "slack"

// originOf is the origin _meta entry for the message ev: the only place
// the sender's identity reaches the agent (never the prompt text).
func originOf(ev slackproto.Event) origin.Origin {
	return origin.Origin{
		Platform:  Platform,
		TeamID:    ev.TeamID,
		UserID:    ev.UserID,
		ChannelID: ev.ChannelID,
		ThreadID:  ev.ThreadTS,
		MessageID: ev.TS,
		IsDM:      ev.IsDM,
		IsMention: ev.IsMention,
	}
}

// withOrigin returns ctx carrying ev's origin, so acp-kit's client sends
// it on every session call the turn makes.
func withOrigin(ctx context.Context, ev slackproto.Event) context.Context {
	return origin.With(ctx, originOf(ev))
}

// scoped narrows the thread key to ev's sender under session_scope
// "thread_user". A message with no sender (a branch seeded by the agent's
// own tool) stays on the thread-wide session.
func (h *Handler) scoped(key router.ConvKey, ev slackproto.Event) router.ConvKey {
	if h.cfg.PerUserSessions {
		key.UserID = ev.UserID
	}
	return key
}

// keyOf is the session key ev is routed to.
func (h *Handler) keyOf(ev slackproto.Event) router.ConvKey {
	return h.scoped(router.ConvKey{ChannelID: ev.ChannelID, ThreadTS: ev.ThreadTS}, ev)
}

// senderSep separates a conversation id from the sender in a broker
// token (see tokenOf).
const senderSep = "#"

// tokenOf is ev's broker token. Under cwd_mode "agent" each sender logs
// in for themselves, so a pending login must belong to its sender, not
// to the whole thread: the token is "<conv>#<user>". Under
// session_scope "thread_user" the conversation already is the sender's,
// and otherwise the token is the conversation ("").
func (h *Handler) tokenOf(conv string, ev slackproto.Event) string {
	if !h.cfg.AgentCwd || h.cfg.PerUserSessions || ev.UserID == "" {
		return ""
	}
	return conv + senderSep + ev.UserID
}

// resolveToken maps a broker token back to its conversation.
func resolveToken(token string) (string, bool) {
	conv, _, _ := strings.Cut(token, senderSep)
	return conv, true
}

// addressee is the user a reply in ev's conversation @-mentions:
// the sender, under session_scope "thread_user" in a shared (non-DM)
// thread. "" otherwise — a DM has one human, and a thread-wide session
// answers the thread.
func (h *Handler) addressee(ev slackproto.Event) string {
	if !h.cfg.PerUserSessions || ev.IsDM {
		return ""
	}
	return ev.UserID
}
