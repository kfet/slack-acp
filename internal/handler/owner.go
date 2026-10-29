package handler

import (
	"context"
	"strings"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/convo"
	kitlog "github.com/kfet/acp-kit/log"

	"github.com/kfet/slack-acp/internal/slackproto"
)

// adminVerbs are the operator commands only an owner may run: they act
// on the relay host or the agent's credentials rather than on one
// conversation. Matched case-insensitively under any accepted sigil.
// The gate covers relay verbs only: commands the agent advertises and
// the broker passes through are the agent's own RBAC. Of these, this
// relay serves the login family; the rest are reserved, matching
// zulip-acp, so a future handler is born gated.
var adminVerbs = map[string]bool{
	"fir":          true,
	"run":          true,
	"skills":       true,
	"update":       true,
	"login":        true,
	"logins":       true,
	"cancel-login": true,
	"logout":       true,
}

// loginVerbs are the admin verbs that manage the agent's credentials.
// Under cwd_mode "agent" the agent keys credentials on the sender (from
// the origin _meta), so these are each sender's own and not owner-only.
var loginVerbs = map[string]bool{
	"login":        true,
	"logins":       true,
	"cancel-login": true,
	"logout":       true,
}

// adminVerb returns the owner-only verb text invokes, if any.
func (h *Handler) adminVerb(text string) (string, bool) {
	verb, ok := adminVerb(text)
	if ok && h.cfg.AgentCwd && loginVerbs[verb] {
		return verb, false
	}
	return verb, ok
}

// adminVerb returns the admin verb text invokes, if any.
func adminVerb(text string) (string, bool) {
	body, ok := command.StripSigil(text)
	if !ok {
		return "", false
	}
	f := strings.Fields(body)
	if len(f) == 0 {
		return "", false
	}
	verb := strings.ToLower(f[0])
	return verb, adminVerbs[verb]
}

// isOwner reports whether ev's author may run admin verbs. The
// self-drive hatch is bot-authored and never an owner.
func (h *Handler) isOwner(ev slackproto.Event) bool {
	_, ok := h.cfg.OwnerUserIDs[ev.UserID]
	return ok && !ev.SelfDrive
}

// ownerRefuses reports whether ownerFilter refuses text from ev.
func (h *Handler) ownerRefuses(ev slackproto.Event, text string) bool {
	_, admin := h.adminVerb(text)
	return admin && !h.isOwner(ev)
}

// ownerFilter refuses admin verbs from anyone not in OwnerUserIDs. It
// fails closed: with no owners configured, nobody may run them. Like
// zulip-acp's opsFilter, the refusal is a short reply and the message
// never reaches the broker or the agent.
func (h *Handler) ownerFilter(ctx context.Context, in *convo.In) convo.Verdict {
	ev := in.Meta.(slackproto.Event)
	owner := h.isOwner(ev)
	verb, ok := h.adminVerb(in.Text)
	if !ok {
		// An owner's pending login takes the thread's next message as
		// its redirect URL; a non-owner's message must not complete it.
		// Under cwd_mode "agent" a login is its sender's (tokenOf), so
		// nobody else's message can reach it.
		_, isCmd := command.StripSigil(in.Text)
		if !h.cfg.AgentCwd && !owner && !isCmd && h.convo.Broker().HasPending(in.Conv) {
			return convo.Forward
		}
		return convo.Pass
	}
	if owner {
		return convo.Pass
	}
	if err := h.reply(ctx, in, "⛔ `"+command.DisplaySigil+verb+"` is owner-only on this relay."); err != nil {
		kitlog.Debugf("handler: owner-only refusal in %s: %v", in.Conv, err)
	}
	return convo.Handled
}
