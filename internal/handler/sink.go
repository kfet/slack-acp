package handler

import (
	"context"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"

	kitlog "github.com/kfet/acp-kit/log"
	"github.com/kfet/slack-acp/internal/slackproto"
	"github.com/kfet/slack-acp/internal/statusline"
)

// discardSink is a no-op SessionUpdateSink used for turns whose output
// must not reach Slack (e.g. backfill catch-up). It never dereferences a
// PostStreamer, so it can never panic or block on I/O.
type discardSink struct{}

func (discardSink) OnUpdate(context.Context, acp.SessionNotification) error { return nil }

// abstainSink wraps a streamingSink and buffers output. If the full
// output equals the silent sentinel, it suppresses posting to Slack
// entirely. Otherwise it forwards all chunks to the wrapped sink.
type abstainSink struct {
	wrapped  *streamingSink
	sentinel string

	mu     sync.Mutex
	chunks []string // buffered message/thought chunks
	sent   bool     // true after the first non-sentinel write
}

func newAbstainSink(wrapped *streamingSink, sentinel string) *abstainSink {
	return &abstainSink{wrapped: wrapped, sentinel: sentinel}
}

func (a *abstainSink) SetModelInfo(emoji, model string) {
	a.wrapped.SetModelInfo(emoji, model)
}

func (a *abstainSink) Status() statusline.Status {
	return a.wrapped.Status()
}

func (a *abstainSink) OnUpdate(ctx context.Context, n acp.SessionNotification) error {
	// Keep the wrapped sink's mood/plan warm via its own method (no
	// direct field access, so the two sinks can't drift on locking).
	a.wrapped.cacheMeta(n)

	// Thoughts are ALWAYS suppressed on the abstain path, regardless of
	// hide_thinking. The sentinel decision below compares the agent's
	// full buffered output against the sentinel string; an italicised
	// thought line would diverge from it and force a post, so an
	// abstaining agent that happened to think out loud could never stay
	// silent. Only message chunks may decide the sentinel.
	chunk := renderChunk(n, true)
	if chunk == "" {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Buffer the chunk.
	a.chunks = append(a.chunks, chunk)

	// Already committed to sending — forward immediately.
	if a.sent {
		return a.wrapped.stream.Append(ctx, a.wrapped.noteFirstChunk(chunk))
	}

	// Not yet committed. While the buffered output is still a prefix of
	// the sentinel, keep buffering (it may turn out to be the sentinel).
	full := strings.TrimSpace(strings.Join(a.chunks, ""))
	if full == a.sentinel {
		return nil // exact sentinel so far — stay silent, keep buffering
	}
	if strings.HasPrefix(a.sentinel, full) && len(full) < len(a.sentinel) {
		return nil // still a strict prefix — could still become the sentinel
	}

	// Diverged from the sentinel: commit to sending and flush the buffer.
	a.sent = true
	kitlog.Debugf("abstain: output diverged from sentinel, flushing buffer")
	for _, c := range a.chunks {
		if err := a.wrapped.stream.Append(ctx, a.wrapped.noteFirstChunk(c)); err != nil {
			return err
		}
	}
	return nil
}

// Finalize is called after the prompt completes. Returns abstained=true
// when the agent produced nothing but the sentinel (or nothing at all),
// meaning the caller should post nothing. If the buffered output was a
// strict prefix of the sentinel that never completed (e.g. the agent
// emitted "<<SIL" then stopped), it is NOT the sentinel, so we flush it
// rather than silently dropping real output.
func (a *abstainSink) Finalize(ctx context.Context) (abstained bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sent {
		return false, nil
	}
	full := strings.TrimSpace(strings.Join(a.chunks, ""))
	if full == a.sentinel || full == "" {
		kitlog.Debugf("abstain: output matched sentinel %q, suppressing post", a.sentinel)
		return true, nil
	}
	// Buffered a strict prefix of the sentinel that never completed —
	// treat as real (partial) output and flush it so nothing is lost.
	kitlog.Debugf("abstain: partial-sentinel output at finalize, flushing")
	a.sent = true
	for _, c := range a.chunks {
		if aerr := a.wrapped.stream.Append(ctx, a.wrapped.noteFirstChunk(c)); aerr != nil {
			return false, aerr
		}
	}
	return false, nil
}

// streamingSink converts ACP session updates to Slack streaming text.
//
// Surface choices (kept deliberately narrow to mirror poe-acp's mobile
// UX, since one fir agent serves both relays):
//
//   - AgentMessageChunk → appended verbatim (the answer body).
//   - AgentThoughtChunk → italicised one-liner, so reasoning still
//     surfaces but doesn't crowd the answer. Suppressed entirely when
//     hideThinking is set (config hide_thinking), mirroring poe-acp.
//   - Plan → suppressed (matches poe-acp). fir emits plan updates
//     frequently on multi-step tasks; rendering them inline stacks
//     "*Plan:*" blocks into the answer and reads as noise on mobile.
//   - dev.acp-kit.status-line/v1 _meta → mood/plan label captured and
//     appended ONCE, in italics after a blank line, as the very last
//     thing in the answer body. See maybeAppendFooter.
//   - ToolCall / ToolCallUpdate → suppressed. Slack chat.update is
//     rate-limited to ~1/s per channel; surfacing every tool tick burns
//     that budget and pushes the answer offscreen on mobile. Users who
//     want tool detail can read the agent's stdout / fir transcript
//     directly.
type streamingSink struct {
	stream *slackproto.PostStreamer

	// statusMu guards the status-line state. Updates arrive
	// concurrently with the chunk path via session/update._meta and
	// are read at the end of the turn to compose the footer, and on
	// every spinner tick via Status().
	statusMu      sync.Mutex
	status        statusline.Status
	footerEmitted bool // set once the status footer has been considered

	// hideThinking suppresses agent_thought_chunk rendering (mirrors
	// poe-acp hide_thinking). Set once at construction; read-only after.
	hideThinking bool
}

func newStreamingSink(s *slackproto.PostStreamer, hideThinking bool) *streamingSink {
	return &streamingSink{stream: s, hideThinking: hideThinking}
}

// SetModelInfo records the relay-resolved identity of the model
// servicing the active turn: the provider emoji and the short model
// name, which the renderer joins into one segment ("🏛️ opus-4.5").
// The handler calls this once, after the session has been established
// and the agent has reported its currentModelId. Either half may be
// empty — an unknown provider or an unnamed model degrades to the
// other half, and both empty drops the segment entirely.
func (s *streamingSink) SetModelInfo(emoji, model string) {
	s.statusMu.Lock()
	s.status.ProviderEmoji = emoji
	s.status.Model = model
	s.statusMu.Unlock()
}

// Status returns a snapshot of the current mood/plan labels as last
// parsed from session/update._meta. Read by the spinner goroutine each
// tick so the animated placeholder picks up agent-emitted state as
// soon as it lands.
func (s *streamingSink) Status() statusline.Status {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	return s.status
}

func (s *streamingSink) OnUpdate(ctx context.Context, n acp.SessionNotification) error {
	// Update the cached mood/plan whenever the agent ships one. The
	// footer is rendered from this snapshot at the END of the turn, so
	// a late update still lands in it — which is the whole reason the
	// line moved to the bottom.
	s.cacheMeta(n)
	chunk := renderChunk(n, s.hideThinking)
	if chunk == "" {
		return nil
	}
	return s.stream.Append(ctx, s.noteFirstChunk(chunk))
}

// cacheMeta parses any dev.acp-kit.status-line _meta on the
// notification and stores the latest mood/plan under statusMu.
func (s *streamingSink) cacheMeta(n acp.SessionNotification) {
	if mood, plan, ok := statusline.ParseMeta(n.Meta); ok {
		s.statusMu.Lock()
		s.status.Mood = mood
		s.status.Plan = plan
		s.statusMu.Unlock()
	}
}

// renderChunk converts a session update into the Slack-bound text for
// that update, or "" if the update produces no user-visible output.
// Shared by streamingSink and abstainSink so their rendering can never
// drift apart. Thought chunks are dropped when hideThinking is set;
// Plan and ToolCall updates never produce body text.
func renderChunk(n acp.SessionNotification, hideThinking bool) string {
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		return contentBlockText(u.AgentMessageChunk.Content)
	case u.AgentThoughtChunk != nil:
		if hideThinking {
			return ""
		}
		if t := contentBlockText(u.AgentThoughtChunk.Content); t != "" {
			return "_" + oneLine(t) + "_\n"
		}
	}
	return ""
}

// noteFirstChunk signals the streamer that real content is about to
// flow, so its placeholder spinner stops and the throttle resets and
// this write flushes immediately. Idempotent on the streamer's side.
// The chunk is returned unchanged — it exists purely so call sites can
// wrap an Append argument and cannot forget the signal.
//
// This is the surviving half of the old maybePrependHeader: the
// prepend itself is gone (the status line is now a footer, see
// maybeAppendFooter), but the FirstChunk hand-off it also performed is
// load-bearing and must still happen on the FIRST user-visible write,
// whichever sink makes it.
func (s *streamingSink) noteFirstChunk(t string) string {
	s.stream.FirstChunk()
	return t
}

// maybeAppendFooter returns the status line to append as the LAST
// thing in the turn's body — a blank line, then the line in italics
// ("\n\n_🏛️ opus-4.5 • steady • 2/5_") — or "" when no footer should
// be written. It is idempotent: only the first call can return a
// non-empty string.
//
// Why the bottom rather than the top (the original design): mood and
// plan are agent-supplied and typically arrive mid-turn, well after
// the first chunk. A header rendered on the first chunk therefore
// showed a status the agent had not published yet — usually the
// provider emoji alone. At the end of the turn the snapshot is final.
//
// It is suppressed when:
//   - the turn produced no user-visible content — nothing to sign, and
//     the streamer's "_thinking…_" fallback body is not an answer;
//   - the rendered line is empty (unknown provider, no model, no agent
//     _meta) — nothing is appended, not even the blank line.
//
// Error turns are excluded by the CALLER: handler.run only reaches
// this on the success path, and its error paths Close the stream with
// an "_error: …_" suffix and nothing else. Keeping that decision in
// the handler rather than latching an `errored` flag here is honest
// about where the knowledge lives — this sink never sees the error.
//
// Placement is safe against this surface's edit/replay mechanism, but
// only because the result goes into PostStreamer.Close's suffix:
//
//   - Slack has no incremental stream. The relay owns ONE message and
//     re-posts the whole accumulated buffer on every chat.update. Text
//     that is not in that buffer is erased by the next edit, so the
//     footer must be buffered, not sent side-band.
//   - Position in the buffer is permanent. Appending the footer
//     mid-turn would strand it in the middle of the answer as later
//     chunks arrived. Close's suffix is the only append that is
//     guaranteed to be last, and Close is idempotent, so it cannot be
//     duplicated.
//   - The spinner writes placeholder frames OUTSIDE the buffer via
//     chat.update. Close sets closed=true before its final flush and
//     UpdatePlaceholder re-checks closed under sendMu, so no late
//     spinner frame can overwrite the footer with "Thinking..".
func (s *streamingSink) maybeAppendFooter() string {
	s.statusMu.Lock()
	if s.footerEmitted {
		s.statusMu.Unlock()
		return ""
	}
	s.footerEmitted = true
	footer := statusline.Footer(s.status)
	s.statusMu.Unlock()
	// The streamer is consulted OUTSIDE statusMu. Nothing takes the
	// streamer's lock and then statusMu — the spinner reads Status()
	// and calls UpdatePlaceholder sequentially, never nested — so
	// holding both here would not deadlock today. It would still be
	// the only place in the relay where the two are nested at all,
	// which is a hierarchy worth not creating.
	if footer == "" || !s.stream.HasContent() {
		return ""
	}
	return footer
}

func contentBlockText(c acp.ContentBlock) string {
	if c.Text != nil {
		return c.Text.Text
	}
	return ""
}

// oneLine collapses newlines into spaces and caps the result to ~200
// runes. Used for thought chunks so a long, multi-line thought stays a
// single italicised line in Slack. Rune-safe: never splits a
// multibyte codepoint.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	const maxRunes = 200
	r := []rune(s)
	if len(r) > maxRunes {
		s = string(r[:maxRunes]) + "…"
	}
	return s
}
