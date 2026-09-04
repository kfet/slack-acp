package statusline

import (
	"strings"
	"testing"
)

// The wire contract (ExtensionID, MaxFieldRunes, Status, ParseMeta,
// ProviderEmoji / ProviderEmojiForModel / ShortModelName, Segments,
// CapRunes) is owned and tested by github.com/kfet/acp-kit/statusline.
// Here we only cover the Slack-mrkdwn renderer surface: Line, Footer,
// Thinking, Spinner.

func TestReExportsPointToKit(t *testing.T) {
	// Cheap guard against the kit drifting under us — pin the wire
	// constant and make sure the re-export function vars aren't nil.
	if ExtensionID != "dev.acp-kit.status-line/v1" {
		t.Fatalf("ExtensionID = %q, want dev.acp-kit.status-line/v1", ExtensionID)
	}
	if MaxFieldRunes != 12 {
		t.Fatalf("MaxFieldRunes = %d, want 12", MaxFieldRunes)
	}
	if ParseMeta == nil || ProviderEmoji == nil || ProviderEmojiForModel == nil || ShortModelName == nil {
		t.Fatal("re-exported function vars must not be nil")
	}
	// One smoke call per re-export — confirms the kit is actually
	// reachable and the alias plumbing works end-to-end.
	if _, _, ok := ParseMeta(nil); ok {
		t.Fatal("ParseMeta(nil) should not be ok")
	}
	if got := ProviderEmoji("anthropic"); got != "🏛️" {
		t.Fatalf("ProviderEmoji(anthropic) = %q", got)
	}
	if got := ProviderEmojiForModel("openai/gpt-5"); got != "🌐" {
		t.Fatalf("ProviderEmojiForModel(openai/gpt-5) = %q", got)
	}
	if got := ShortModelName("anthropic/claude-opus-4-5-20251001"); got != "opus-4.5" {
		t.Fatalf("ShortModelName = %q, want opus-4.5", got)
	}
	if got := ShortModelName(""); got != "" {
		t.Fatalf("ShortModelName(\"\") = %q, want empty", got)
	}
}

// TestLineRendering pins the bare status line, including the rule that
// matters most for this change: the provider emoji and the model short
// name are ONE space-joined segment, not two bullet-separated fields.
func TestLineRendering(t *testing.T) {
	cases := []struct {
		name string
		in   Status
		want string
	}{
		{"all-empty", Status{}, ""},
		{"whitespace-only", Status{ProviderEmoji: " ", Model: " ", Mood: "  ", Plan: " "}, ""},
		{"mood-only", Status{Mood: "steady"}, "steady"},
		{"plan-only", Status{Plan: "3/8"}, "3/8"},
		{"mood-plan", Status{Mood: "steady", Plan: "3/8"}, "steady • 3/8"},
		// Emoji + model share ONE segment, space-joined, no bullet.
		{"emoji-model", Status{ProviderEmoji: "🏛️", Model: "opus-4.5"}, "🏛️ opus-4.5"},
		{"full",
			Status{ProviderEmoji: "🏛️", Model: "opus-4.5", Mood: "steady", Plan: "3/8"},
			"🏛️ opus-4.5 • steady • 3/8"},
		// Either half alone degrades to just that half.
		{"emoji-only", Status{ProviderEmoji: "🏛️", Mood: "steady", Plan: "3/8"}, "🏛️ • steady • 3/8"},
		{"model-only", Status{Model: "gpt-5-codex"}, "gpt-5-codex"},
		{"model-mood-no-emoji", Status{Model: "gpt-5-codex", Mood: "steady"}, "gpt-5-codex • steady"},
	}
	for _, tc := range cases {
		if got := Line(tc.in); got != tc.want {
			t.Errorf("%s: Line(%#v) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestFooterRendering pins the final-answer surface: a blank line then
// the status line in SLACK-MRKDWN italics (single underscores — not
// asterisks, which are bold here), and nothing at all when there is
// nothing to say.
func TestFooterRendering(t *testing.T) {
	cases := []struct {
		name string
		in   Status
		want string
	}{
		// Nothing to show → nothing appended, not even the blank line.
		{"all-empty", Status{}, ""},
		{"whitespace-only", Status{ProviderEmoji: " ", Model: "  ", Mood: " ", Plan: "  "}, ""},
		{"full",
			Status{ProviderEmoji: "🏛️", Model: "opus-4.5", Mood: "steady", Plan: "2/5"},
			"\n\n_🏛️ opus-4.5 • steady • 2/5_"},
		{"model-identity-only",
			Status{ProviderEmoji: "🌐", Model: "gpt-5-codex"},
			"\n\n_🌐 gpt-5-codex_"},
		{"agent-meta-only", Status{Mood: "steady", Plan: "2/5"}, "\n\n_steady • 2/5_"},
	}
	for _, tc := range cases {
		if got := Footer(tc.in); got != tc.want {
			t.Errorf("%s: Footer(%#v) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestFooterIsNotBlockquoted: the footer is part of the ANSWER, so it
// must not carry the spinner's "> " blockquote chrome. Regression
// guard against someone reviving the old Header renderer's markup.
func TestFooterIsNotBlockquoted(t *testing.T) {
	got := Footer(Status{ProviderEmoji: "🏛️", Model: "opus-4.5"})
	if strings.Contains(got, ">") {
		t.Fatalf("footer must not be a blockquote; got %q", got)
	}
	if !strings.HasPrefix(got, "\n\n_") || !strings.HasSuffix(got, "_") {
		t.Fatalf("footer must be a blank line + italics; got %q", got)
	}
}

func TestThinkingEmpty(t *testing.T) {
	got := Thinking(Status{})
	if got != "> _Thinking…_" {
		t.Fatalf("got %q", got)
	}
}

func TestThinkingWithStatus(t *testing.T) {
	got := Thinking(Status{Mood: "steady", Plan: "3/8"})
	if got != "> _steady • 3/8 • Thinking…_" {
		t.Fatalf("got %q", got)
	}
}

// TestThinkingCarriesModel: the live indicator keeps its blockquote
// form and now also names the model, exactly as the footer does.
func TestThinkingCarriesModel(t *testing.T) {
	got := Thinking(Status{ProviderEmoji: "🏛️", Model: "opus-4.5", Mood: "steady"})
	if got != "> _🏛️ opus-4.5 • steady • Thinking…_" {
		t.Fatalf("got %q", got)
	}
}

func TestSpinnerWithProviderEmoji(t *testing.T) {
	got := Spinner(Status{ProviderEmoji: "🌐"}, ".")
	if got != "> _🌐 • Thinking._" {
		t.Fatalf("got %q", got)
	}
}

// TestSpinnerCarriesModel pins the live frame's model identity segment.
func TestSpinnerCarriesModel(t *testing.T) {
	got := Spinner(Status{ProviderEmoji: "🌐", Model: "gpt-5-codex", Mood: "steady", Plan: "2/5"}, "..")
	if got != "> _🌐 gpt-5-codex • steady • 2/5 • Thinking.._" {
		t.Fatalf("got %q", got)
	}
}

func TestSpinnerCustomDots(t *testing.T) {
	got := Spinner(Status{Mood: "steady"}, "..")
	if got != "> _steady • Thinking.._" {
		t.Fatalf("got %q", got)
	}
}

func TestSpinnerEmptyDotsDefault(t *testing.T) {
	got := Spinner(Status{}, "")
	if got != "> _Thinking…_" {
		t.Fatalf("got %q", got)
	}
}
