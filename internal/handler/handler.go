// Package handler glues slackproto + router + acpclient: it turns inbound
// Slack events into ACP prompts and streams the agent's session updates
// back into a Slack thread message.
package handler

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/slack-go/slack"

	"github.com/kfet/slack-acp/internal/debuglog"
	"github.com/kfet/slack-acp/internal/router"
	"github.com/kfet/slack-acp/internal/slackproto"
)

// Config configures the handler.
type Config struct {
	Router            *router.Router
	API               *slack.Client
	AllowedUserIDs    map[string]struct{}
	AllowedChannelIDs map[string]struct{}
	// PromptTimeout caps the wall-clock for a single prompt. Default 10m.
	PromptTimeout time.Duration
}

// Handler implements slackproto.Handler.
type Handler struct {
	cfg Config

	// inflight maps ConvKey → cancel func of the goroutine processing it,
	// so a follow-up message in the same thread can cancel the prior run.
	inflightMu sync.Mutex
	inflight   map[router.ConvKey]context.CancelFunc
}

// New constructs a handler.
func New(cfg Config) *Handler {
	if cfg.PromptTimeout == 0 {
		cfg.PromptTimeout = 10 * time.Minute
	}
	return &Handler{cfg: cfg, inflight: make(map[router.ConvKey]context.CancelFunc)}
}

// SetAPI installs the Slack API client (used for posting/updating messages).
// Called by main after the slackproto.Client has been constructed.
func (h *Handler) SetAPI(api *slack.Client) { h.cfg.API = api }

// Handle is called by slackproto.Client for each inbound event.
func (h *Handler) Handle(ctx context.Context, ev slackproto.Event) {
	if !h.allowed(ev) {
		debuglog.Logf("handler: drop ev from user=%s channel=%s (not allowed)", ev.UserID, ev.ChannelID)
		return
	}
	text := strings.TrimSpace(ev.Text)
	if text == "" {
		return
	}
	key := router.ConvKey{ChannelID: ev.ChannelID, ThreadTS: ev.ThreadTS}

	// Cancel any in-flight prompt for this thread, then start a new one.
	h.cancelInflight(ctx, key)
	pctx, cancel := context.WithTimeout(context.Background(), h.cfg.PromptTimeout)
	h.setInflight(key, cancel)
	go func() {
		defer h.clearInflight(key, cancel)
		defer cancel()
		if err := h.run(pctx, ev, key, text); err != nil {
			debuglog.Logf("handler: prompt error: %v", err)
		}
	}()
}

func (h *Handler) allowed(ev slackproto.Event) bool {
	if len(h.cfg.AllowedUserIDs) > 0 {
		if _, ok := h.cfg.AllowedUserIDs[ev.UserID]; !ok {
			return false
		}
	}
	if len(h.cfg.AllowedChannelIDs) > 0 {
		if _, ok := h.cfg.AllowedChannelIDs[ev.ChannelID]; !ok {
			return false
		}
	}
	return true
}

func (h *Handler) cancelInflight(ctx context.Context, key router.ConvKey) {
	h.inflightMu.Lock()
	c, ok := h.inflight[key]
	if ok {
		delete(h.inflight, key)
	}
	h.inflightMu.Unlock()
	if ok {
		c()
		// Also tell the agent to stop generating.
		h.cfg.Router.Cancel(ctx, key)
	}
}

func (h *Handler) setInflight(key router.ConvKey, c context.CancelFunc) {
	h.inflightMu.Lock()
	h.inflight[key] = c
	h.inflightMu.Unlock()
}

func (h *Handler) clearInflight(key router.ConvKey, c context.CancelFunc) {
	h.inflightMu.Lock()
	if cur, ok := h.inflight[key]; ok && fmt.Sprintf("%p", cur) == fmt.Sprintf("%p", c) {
		delete(h.inflight, key)
	}
	h.inflightMu.Unlock()
}

// run handles a single prompt end-to-end.
func (h *Handler) run(ctx context.Context, ev slackproto.Event, key router.ConvKey, text string) error {
	stream := slackproto.NewPostStreamer(h.cfg.API, ev.ChannelID, ev.ThreadTS)
	sink := newStreamingSink(stream)

	// Watchdog: flush pending text every 1s while the prompt runs.
	wctx, wcancel := context.WithCancel(ctx)
	defer wcancel()
	go watchdog(wctx, stream)

	sess, err := h.cfg.Router.GetOrCreate(ctx, key, sink)
	if err != nil {
		_ = stream.Close(ctx, fmt.Sprintf("\n_error: %v_", err))
		return err
	}

	sess.Mu.Lock()
	defer sess.Mu.Unlock()
	h.cfg.Router.Touch(sess)

	stop, err := h.cfg.Router.Agent().Prompt(ctx, sess.SessionID, []acp.ContentBlock{
		{Text: &acp.ContentBlockText{Text: text}},
	})
	wcancel()
	if err != nil {
		_ = stream.Close(context.Background(), fmt.Sprintf("\n_error: %v_", err))
		return err
	}
	suffix := ""
	if stop != "" && stop != acp.StopReasonEndTurn {
		suffix = fmt.Sprintf("\n_(stopped: %s)_", stop)
	}
	return stream.Close(context.Background(), suffix)
}

func watchdog(ctx context.Context, s *slackproto.PostStreamer) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = s.FlushIfPending(context.Background())
		}
	}
}
