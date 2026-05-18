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

// inflightEntry wraps a per-call cancel func with a unique identity so
// clearInflight can tell its own entry from one a follow-up has since
// installed. Comparing the cancel funcs themselves via fmt.Sprintf("%p",
// ...) is not safe: two closures produced from the same source line
// share an underlying code pointer.
type inflightEntry struct {
	cancel context.CancelFunc
}

// Handler implements slackproto.Handler.
type Handler struct {
	cfg Config

	// inflight maps ConvKey → entry of the goroutine processing it,
	// so a follow-up message in the same thread can cancel the prior run.
	inflightMu   sync.Mutex
	inflightCond *sync.Cond // broadcast when inflight is mutated
	inflight     map[router.ConvKey]*inflightEntry
}

// New constructs a handler.
func New(cfg Config) *Handler {
	if cfg.PromptTimeout == 0 {
		cfg.PromptTimeout = 10 * time.Minute
	}
	h := &Handler{cfg: cfg, inflight: make(map[router.ConvKey]*inflightEntry)}
	h.inflightCond = sync.NewCond(&h.inflightMu)
	return h
}

// WaitIdle blocks until the handler has no in-flight prompts or ctx
// is done. Used by tests to synchronise on the inflight-empty
// transition without wall-clock polling; also useful for graceful
// shutdown paths.
//
// Implementation note: Cond.Wait can't accept a context, so we spawn
// a helper goroutine that broadcasts when ctx fires. The helper will
// exit on its own once ctx is cancelled — typically when the calling
// test's deferred cancel runs, so no real leak.
func (h *Handler) WaitIdle(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		h.inflightMu.Lock()
		h.inflightCond.Broadcast()
		h.inflightMu.Unlock()
	}()
	h.inflightMu.Lock()
	defer h.inflightMu.Unlock()
	for len(h.inflight) > 0 && ctx.Err() == nil {
		h.inflightCond.Wait()
	}
	return ctx.Err()
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
	entry := &inflightEntry{cancel: cancel}
	h.setInflight(key, entry)
	go func() {
		defer h.clearInflight(key, entry)
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
	e, ok := h.inflight[key]
	if ok {
		delete(h.inflight, key)
		h.inflightCond.Broadcast()
	}
	h.inflightMu.Unlock()
	if ok {
		e.cancel()
		// Also tell the agent to stop generating.
		h.cfg.Router.Cancel(ctx, key)
	}
}

func (h *Handler) setInflight(key router.ConvKey, e *inflightEntry) {
	h.inflightMu.Lock()
	h.inflight[key] = e
	h.inflightMu.Unlock()
}

func (h *Handler) clearInflight(key router.ConvKey, e *inflightEntry) {
	h.inflightMu.Lock()
	if cur, ok := h.inflight[key]; ok && cur == e {
		delete(h.inflight, key)
		h.inflightCond.Broadcast()
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

	promptText := text
	if prefix := h.cfg.Router.TakePendingSystemPrompt(sess); prefix != "" {
		promptText = prefix + "\n\n" + text
	}

	stop, err := h.cfg.Router.Agent().Prompt(ctx, sess.SessionID, []acp.ContentBlock{
		{Text: &acp.ContentBlockText{Text: promptText}},
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
	watchdogWithTick(ctx, s, time.Second)
}

// watchdogWithTick is the testable core: takes the tick duration as a
// parameter so tests don't need to wall-clock-poll for a 1-second
// flush.
func watchdogWithTick(ctx context.Context, s *slackproto.PostStreamer, tick time.Duration) {
	t := time.NewTicker(tick)
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
