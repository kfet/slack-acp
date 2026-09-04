// Package statusline is the Slack-mrkdwn renderer for the
// dev.acp-kit.status-line/v1 ACP extension. The wire contract
// (ExtensionID, MaxFieldRunes, Status, ParseMeta, ProviderEmoji,
// ProviderEmojiForModel, ShortModelName, Segments, CapRunes) lives in
// github.com/kfet/acp-kit/statusline so poe-acp, slack-acp and the
// fir agent stay byte-identical on the wire. This package owns only
// the Slack-specific markup — Slack mrkdwn italics (_…_), the
// blockquote spinner frame, and the "Thinking…" placeholder.
//
// Footer, not header. The status line used to be PREPENDED to the
// first user-visible chunk. Mood and plan are agent-supplied and
// normally arrive mid-turn, so a line rendered at the first chunk
// showed whatever was known before the agent had started — usually
// the provider emoji alone. Rendering it as a FOOTER under the
// finished answer means it carries the LATEST snapshot. That is the
// whole point of the move.
//
// Slack's chat.update rate-limit (~1/s/channel) means the spinner
// loop ticks well above 1s; renderers here have no opinion on rate,
// they just produce a frame string.
package statusline

import (
	"strings"

	kit "github.com/kfet/acp-kit/statusline"
)

// Re-exports of the wire contract so call sites only need to import
// this package. Adding a new field or helper to the kit also requires
// re-exporting it here if the relay uses it.

// ExtensionID is the _meta key both sides use to advertise support
// and to carry per-update mood/plan payloads.
const ExtensionID = kit.ExtensionID

// MaxFieldRunes caps the rendered length of mood and plan.
const MaxFieldRunes = kit.MaxFieldRunes

// Status is the renderable state of one status line.
type Status = kit.Status

// ParseMeta extracts the v1 mood/plan fields from a session/update
// _meta map. See kit docs for full semantics.
var ParseMeta = kit.ParseMeta

// ProviderEmoji maps a provider slug to its emoji.
var ProviderEmoji = kit.ProviderEmoji

// ProviderEmojiForModel resolves the provider emoji from a fully
// qualified "<provider>/<model>" id.
var ProviderEmojiForModel = kit.ProviderEmojiForModel

// ShortModelName derives the compact display name shown next to the
// provider emoji from a fully qualified model id
// ("anthropic/claude-opus-4-5-20251001" → "opus-4.5"). It is lossy by
// design — a display label, never an identifier to put back on the
// wire.
var ShortModelName = kit.ShortModelName

// Line renders the bare status line: the non-empty segments joined
// with " • ". The provider emoji and the model short name share the
// FIRST segment, space-joined ("🏛️ opus-4.5"), because they name one
// thing; a bullet between them would read as two independent fields.
//
// Returns "" when there is nothing to show. This is the raw text —
// callers wrap it in the surface markup they need (Footer for the
// finished answer, Spinner for the live frame).
func Line(s Status) string {
	return strings.Join(kit.Segments(s), " • ")
}

// Footer renders the status line as it is APPENDED to the end of a
// finished Slack answer: a blank line, then the line in Slack-mrkdwn
// italics.
//
//	\n\n_🏛️ opus-4.5 • steady • 2/5_
//
// Slack mrkdwn italics are single underscores (`_text_`) — the same
// markup this relay already uses for thought one-liners and for the
// `_(stopped: …)_` suffix. It is NOT Poe's or Zulip's markdown, and
// asterisks would render as *bold* here.
//
// Returns "" when there is nothing to show (unknown provider, no
// model, no agent _meta), so the caller appends nothing at all rather
// than a stray blank line or an empty pair of underscores.
//
// The leading "\n\n" is part of the footer, not the caller's job: it
// is what stops the italic line being swallowed into the answer's
// last paragraph.
func Footer(s Status) string {
	line := Line(s)
	if line == "" {
		return ""
	}
	return "\n\n_" + line + "_"
}

// Thinking renders the initial placeholder line (no animation — the
// caller hasn't started the spinner loop yet). Always emits a visible
// frame ("Thinking…" suffix) even with no mood/plan known yet, so
// users see immediate liveness in Slack. Once mood/plan land, callers
// can re-render with the updated Status.
func Thinking(s Status) string {
	return Spinner(s, "…")
}

// Spinner renders a single live thinking frame. dots is the current
// animation phase (e.g. ".", "..", "…") and is appended to "Thinking".
// Empty dots default to "…" so the function always returns a visible
// frame even with no mood/plan in the Status.
//
// The segments include the model identity ("🏛️ opus-4.5"), so the
// live indicator names the model servicing the turn just as the final
// footer does. The frame keeps its blockquote+italic form — it is a
// transient placeholder that must read as chrome, not as the answer.
func Spinner(s Status, dots string) string {
	if dots == "" {
		dots = "…"
	}
	parts := kit.Segments(s)
	parts = append(parts, "Thinking"+dots)
	return "> _" + strings.Join(parts, " • ") + "_"
}
