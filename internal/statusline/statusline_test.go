package statusline

import "testing"

// The wire contract (ExtensionID, MaxFieldRunes, Status, ParseMeta,
// ProviderEmoji / ProviderEmojiForModel, Segments, CapRunes) is owned
// and tested by github.com/kfet/acp-kit/statusline. Here we only
// cover the Slack-mrkdwn renderer surface: Header, Thinking, Spinner.

func TestReExportsPointToKit(t *testing.T) {
	// Cheap guard against the kit drifting under us — pin the wire
	// constant and make sure the re-export function vars aren't nil.
	if ExtensionID != "dev.acp-kit.status-line/v1" {
		t.Fatalf("ExtensionID = %q, want dev.acp-kit.status-line/v1", ExtensionID)
	}
	if MaxFieldRunes != 12 {
		t.Fatalf("MaxFieldRunes = %d, want 12", MaxFieldRunes)
	}
	if ParseMeta == nil || ProviderEmoji == nil || ProviderEmojiForModel == nil {
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
}

func TestHeaderWithProviderEmoji(t *testing.T) {
	got := Header(Status{ProviderEmoji: "🏛️", Mood: "steady", Plan: "3/8"})
	if got != "> _🏛️ • steady • 3/8_" {
		t.Fatalf("got %q", got)
	}
}

func TestHeaderEmptyDropsPrepend(t *testing.T) {
	if got := Header(Status{}); got != "" {
		t.Fatalf("empty status should render empty; got %q", got)
	}
	if got := Header(Status{Mood: "   "}); got != "" {
		t.Fatalf("whitespace-only must be dropped; got %q", got)
	}
}

func TestHeaderMoodOnly(t *testing.T) {
	got := Header(Status{Mood: "steady"})
	if got != "> _steady_" {
		t.Fatalf("got %q", got)
	}
}

func TestHeaderPlanOnly(t *testing.T) {
	got := Header(Status{Plan: "3/8"})
	if got != "> _3/8_" {
		t.Fatalf("got %q", got)
	}
}

func TestHeaderBoth(t *testing.T) {
	got := Header(Status{Mood: "steady", Plan: "3/8"})
	if got != "> _steady • 3/8_" {
		t.Fatalf("got %q", got)
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

func TestSpinnerWithProviderEmoji(t *testing.T) {
	got := Spinner(Status{ProviderEmoji: "🌐"}, ".")
	if got != "> _🌐 • Thinking._" {
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
