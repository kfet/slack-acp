// Package statusline is the Slack-mrkdwn renderer for the
// dev.acp-kit.status-line/v1 ACP extension. The wire contract
// (ExtensionID, MaxFieldRunes, Status, ParseMeta, ProviderEmoji,
// ProviderEmojiForModel, Segments, CapRunes) lives in
// github.com/kfet/acp-kit/statusline so poe-acp, slack-acp and the
// fir agent stay byte-identical on the wire. This package owns only
// the Slack-specific markup — a blockquote + italic line — and the
// "Thinking…" placeholder frame.
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

// Status is the renderable state of one status header.
type Status = kit.Status

// ParseMeta extracts the v1 mood/plan fields from a session/update
// _meta map. See kit docs for full semantics.
var ParseMeta = kit.ParseMeta

// ProviderEmoji maps a provider slug to its emoji.
var ProviderEmoji = kit.ProviderEmoji

// ProviderEmojiForModel resolves the provider emoji from a fully
// qualified "<provider>/<model>" id.
var ProviderEmojiForModel = kit.ProviderEmojiForModel

// Header renders the final-message header for Slack. Returns "" when
// nothing would be shown (all segments empty) — caller drops the
// prepend entirely. Segments are joined with " • " and wrapped in a
// Slack-mrkdwn blockquote + italic.
func Header(s Status) string {
	parts := kit.Segments(s)
	if len(parts) == 0 {
		return ""
	}
	return "> _" + strings.Join(parts, " • ") + "_"
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
func Spinner(s Status, dots string) string {
	if dots == "" {
		dots = "…"
	}
	parts := kit.Segments(s)
	parts = append(parts, "Thinking"+dots)
	return "> _" + strings.Join(parts, " • ") + "_"
}
