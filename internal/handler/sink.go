package handler

import (
	"context"
	"strings"
	"sync"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/slack-acp/internal/slackproto"
	"github.com/kfet/slack-acp/internal/statusline"
)

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
	if mood, plan, ok := statusline.ParseMeta(n.Meta); ok {
		s.statusMu.Lock()
		s.status.Mood = mood
		s.status.Plan = plan
		s.statusMu.Unlock()
	}

	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		if t := contentBlockText(u.AgentMessageChunk.Content); t != "" {
			return s.stream.Append(ctx, s.maybePrependHeader(t))
		}
	case u.AgentThoughtChunk != nil:
		// Render thoughts in italics, kept compact.
		if t := contentBlockText(u.AgentThoughtChunk.Content); t != "" {
			return s.stream.Append(ctx, s.maybePrependHeader("_"+oneLine(t)+"_\n"))
		}
	case u.Plan != nil:
		// Render a short plan block. Skip empty/cleared plan updates so we
		// don't leave a bare "Plan:" trailer in the Slack message.
		if len(u.Plan.Entries) == 0 {
			return nil
		}
		var b strings.Builder
		b.WriteString("\n*Plan:*\n")
		for _, e := range u.Plan.Entries {
			b.WriteString("• " + e.Content + "\n")
		}
		return s.stream.Append(ctx, s.maybePrependHeader(b.String()))
	}
	return nil
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
