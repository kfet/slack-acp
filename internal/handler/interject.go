package handler

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/kfet/acp-kit/convo"
	"github.com/kfet/acp-kit/interject"
	kitlog "github.com/kfet/acp-kit/log"
	"github.com/kfet/slack-acp/internal/router"
	"github.com/kfet/slack-acp/internal/slackproto"
)

// interjectTimeout bounds one fork's answer; the fork prompt has no
// bound of its own.
const interjectTimeout = 60 * time.Second

// errNoSession: the running turn has no live session to fork yet (it
// is still being created).
var errNoSession = errors.New("interject: no live session to fork")

// interjectable reports whether ev should be put to a fork of the
// running turn instead of superseding it. Commands (and an open login
// prompt) always go to convo; attachments do too, since the fork could
// not see them. An ambient reply is never put to a fork: its answer
// would bypass the abstain sentinel.
func (h *Handler) interjectable(ev slackproto.Event, conv, text string) bool {
	return h.ij != nil && addressed(ev) && len(ev.Files) == 0 && !strings.HasPrefix(text, "!") &&
		!h.convo.Broker().HasPending(h.tokenOf(conv, ev)) && h.convo.Active().Running(conv)
}

// interject asks a fork of key's session the mid-turn message, posts
// its answer, and acts on its verdict. Any failure falls back to the
// old behaviour: the message supersedes the running turn.
func (h *Handler) interject(ctx context.Context, ev slackproto.Event, key router.ConvKey, text string) {
	conv := key.String()
	r, err := h.ask(ctx, ev, key, text)
	if err != nil {
		kitlog.Debugf("handler: interject in %s: %v; superseding", conv, err)
		h.dispatch(ctx, ev, key, text, handlerRecord(ev))
		return
	}
	if h.cfg.Ambient {
		h.checkpoint(key, ev.TS)
	}
	if r.Text != "" {
		if err := h.reply(ctx, &convo.In{Conv: conv, Meta: ev}, r.Text+"\n\n"+r.Tag()); err != nil {
			kitlog.Debugf("handler: interject reply in %s: %v", conv, err)
		}
	}
	switch {
	case r.Action == interject.Inform && !r.Delivered:
		// The agent could not take it mid-turn: supersede, as before.
		h.dispatch(ctx, ev, key, text, handlerRecord(ev))
	case r.Action == interject.Abort:
		h.convo.Active().Stop(ctx, conv)
	}
}

func (h *Handler) ask(ctx context.Context, ev slackproto.Event, key router.ConvKey, text string) (interject.Reply, error) {
	sid, cwd, ok := h.cfg.Router.ForkTarget(key)
	if !ok {
		return interject.Reply{}, errNoSession
	}
	actx, cancel := context.WithTimeout(withOrigin(ctx, ev), interjectTimeout)
	defer cancel()
	return h.ij.Ask(actx, key.String(), cwd, sid, text)
}
