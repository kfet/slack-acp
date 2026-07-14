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

func (a *abstainSink) SetProviderEmoji(emoji string) {
	a.wrapped.SetProviderEmoji(emoji)
}

func (a *abstainSink) Status() statusline.Status {
	return a.wrapped.Status()
}

func (a *abstainSink) OnUpdate(ctx context.Context, n acp.SessionNotification) error {
	// Keep the wrapped sink's mood/plan warm via its own method (no
	// direct field access, so the two sinks can't drift on locking).
	a.wrapped.cacheMeta(n)

	chunk := renderChunk(n)
	if chunk == "" {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Buffer the chunk.
	a.chunks = append(a.chunks, chunk)

	// Already committed to sending — forward immediately.
	if a.sent {
		return a.wrapped.stream.Append(ctx, a.wrapped.maybePrependHeader(chunk))
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
		if err := a.wrapped.stream.Append(ctx, a.wrapped.maybePrependHeader(c)); err != nil {
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
		if aerr := a.wrapped.stream.Append(ctx, a.wrapped.maybePrependHeader(c)); aerr != nil {
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
//     surfaces but doesn't crowd the answer.
//   - Plan → rendered as a short "*Plan:*" block (empty plans skipped
//     so we don't leave a bare trailer).
//   - dev.acp-kit.status-line/v1 _meta → mood/plan label captured and
//     prepended once, on the first user-visible chunk, as a status
//     header line.
//   - ToolCall / ToolCallUpdate → suppressed. Slack chat.update is
//     rate-limited to ~1/s per channel; surfacing every tool tick burns
//     that budget and pushes the answer offscreen on mobile. Users who
//     want tool detail can read the agent's stdout / fir transcript
//     directly.
type streamingSink struct {
	stream *slackproto.PostStreamer

	// statusMu guards the status-line state. Updates can arrive
	// concurrently with the chunk path via session/update._meta; the
	// first user-visible text chunk reads it once to compose the
	// header prepend.
	statusMu      sync.Mutex
	status        statusline.Status
	headerEmitted bool // set after the first prepend has been considered
}

func newStreamingSink(s *slackproto.PostStreamer) *streamingSink {
	return &streamingSink{stream: s}
}

// SetProviderEmoji records the relay-resolved provider emoji for the
// active turn. The handler calls this once after the session has been
// established and the agent has reported its currentModelId. Empty
// string means the provider is unknown and the segment will be
// dropped by the renderer.
func (s *streamingSink) SetProviderEmoji(emoji string) {
	s.statusMu.Lock()
	s.status.ProviderEmoji = emoji
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
	// Update the cached mood/plan whenever the agent ships one.
	// Header rendering happens lazily on the first user-visible chunk;
	// this just keeps the latest values warm.
	s.cacheMeta(n)
	chunk := renderChunk(n)
	if chunk == "" {
		return nil
	}
	return s.stream.Append(ctx, s.maybePrependHeader(chunk))
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
// drift apart.
func renderChunk(n acp.SessionNotification) string {
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		return contentBlockText(u.AgentMessageChunk.Content)
	case u.AgentThoughtChunk != nil:
		if t := contentBlockText(u.AgentThoughtChunk.Content); t != "" {
			return "_" + oneLine(t) + "_\n"
		}
	case u.Plan != nil:
		if len(u.Plan.Entries) == 0 {
			return ""
		}
		var b strings.Builder
		b.WriteString("\n*Plan:*\n")
		for _, e := range u.Plan.Entries {
			b.WriteString("• " + e.Content + "\n")
		}
		return b.String()
	}
	return ""
}

// maybePrependHeader injects the final-message status header in front
// of the first user-visible write, exactly once, AND signals
// FirstChunk on the streamer so its placeholder spinner stops and the
// throttle resets to flush this write immediately. Subsequent writes
// pass through unchanged. Returns t verbatim when the header would be
// empty (no mood/plan ever landed).
func (s *streamingSink) maybePrependHeader(t string) string {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if s.headerEmitted {
		return t
	}
	s.headerEmitted = true
	s.stream.FirstChunk()
	h := statusline.Header(s.status)
	if h == "" {
		return t
	}
	return h + "\n" + t
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
