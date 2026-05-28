package handler

import (
	"context"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/slack-acp/internal/slackproto"
	"github.com/kfet/slack-acp/internal/statusline"
)

func newSinkAndCapture(t *testing.T) (*streamingSink, *fakeSlack) {
	t.Helper()
	fs := newFakeSlack()
	t.Cleanup(fs.close)
	stream := slackproto.NewPostStreamer(fs.client(), "C1", "1.0")
	return newStreamingSink(stream), fs
}

func TestSinkAgentMessageChunk(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "hello"}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if fs.posts == 0 || !strings.Contains(strings.Join(fs.bodies, ""), "hello") {
		t.Fatalf("expected post w/ 'hello'; bodies=%q posts=%d", fs.bodies, fs.posts)
	}
}

func TestSinkEmptyAgentMessageChunkSkipped(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if fs.posts != 0 {
		t.Fatal("empty content should not produce a post")
	}
}

func TestSinkAgentThoughtChunk(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "thinking…"}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(fs.bodies, ""), "_thinking") {
		t.Fatalf("expected italicised thought; bodies=%q", fs.bodies)
	}
}

func TestSinkEmptyAgentThoughtChunkSkipped(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.ContentBlock{}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if fs.posts != 0 {
		t.Fatal("empty thought should not produce a post")
	}
}

func TestSinkToolCallSuppressed(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{Title: "Run tests"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if fs.posts != 0 {
		t.Fatalf("tool calls must be hidden; bodies=%q", fs.bodies)
	}
}

func TestSinkToolCallUpdateSuppressed(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	completed := acp.ToolCallStatus("completed")
	failed := acp.ToolCallStatus("failed")
	for _, st := range []*acp.ToolCallStatus{nil, &completed, &failed} {
		if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
			Update: acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{Status: st}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if fs.posts != 0 {
		t.Fatalf("tool-call updates must be hidden; bodies=%q", fs.bodies)
	}
}

func TestSinkStatusGetter(t *testing.T) {
	sink, _ := newSinkAndCapture(t)
	if got := sink.Status(); got.Mood != "" || got.Plan != "" || got.ProviderEmoji != "" {
		t.Fatalf("expected zero status before any input; got %+v", got)
	}
	sink.SetProviderEmoji("🏛️")
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		Meta: map[string]any{
			statusline.ExtensionID: map[string]any{"mood": "curious", "plan": "1/2"},
		},
	})
	got := sink.Status()
	if got.ProviderEmoji != "🏛️" || got.Mood != "curious" || got.Plan != "1/2" {
		t.Fatalf("Status() did not reflect parsed meta + emoji; got %+v", got)
	}
}

func TestSinkStatusHeaderPrepended(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	// Mood/plan arrive before the first text chunk.
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Meta: map[string]any{
			statusline.ExtensionID: map[string]any{"mood": "steady", "plan": "3/8"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "hello"}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(fs.bodies, "")
	if !strings.Contains(body, "> _steady • 3/8_") {
		t.Fatalf("expected status header; body=%q", body)
	}
	if !strings.Contains(body, "hello") {
		t.Fatalf("expected message body; body=%q", body)
	}
	// Second chunk must NOT prepend the header again.
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: " world"}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.Join(fs.bodies, ""), "> _steady"); n != 1 {
		t.Fatalf("header must appear exactly once across all bodies; got %d", n)
	}
}
func TestSinkStatusHeaderEmptyNoOp(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "hi"}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(fs.bodies, "")
	if strings.Contains(body, "> _") {
		t.Fatalf("no status → no header; body=%q", body)
	}
}

func TestSinkStatusHeaderOnThoughtChunk(t *testing.T) {
	// First user-visible write is a thought, not a message — header
	// should still prepend exactly once.
	sink, fs := newSinkAndCapture(t)
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		Meta: map[string]any{
			statusline.ExtensionID: map[string]any{"mood": "curious"},
		},
	})
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "hmm"}},
			},
		},
	})
	body := strings.Join(fs.bodies, "")
	if !strings.Contains(body, "> _curious_") {
		t.Fatalf("expected header on first thought; body=%q", body)
	}
}

func TestSinkStatusHeaderOnPlanFirst(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		Meta: map[string]any{
			statusline.ExtensionID: map[string]any{"plan": "1/2"},
		},
	})
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			Plan: &acp.SessionUpdatePlan{Entries: []acp.PlanEntry{{Content: "do thing"}}},
		},
	})
	body := strings.Join(fs.bodies, "")
	if !strings.Contains(body, "> _1/2_") {
		t.Fatalf("expected header on first plan write; body=%q", body)
	}
}

func TestSinkPlan(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			Plan: &acp.SessionUpdatePlan{
				Entries: []acp.PlanEntry{{Content: "step 1"}, {Content: "step 2"}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(fs.bodies, "")
	if !strings.Contains(body, "step 1") || !strings.Contains(body, "step 2") {
		t.Fatalf("missing plan entries; body=%q", body)
	}
}

func TestSinkPlanEmptySkipped(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{Plan: &acp.SessionUpdatePlan{}},
	}); err != nil {
		t.Fatal(err)
	}
	if fs.posts != 0 {
		t.Fatalf("empty plan should not post; bodies=%q", fs.bodies)
	}
}

func TestSinkUnknownUpdate(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	// All variants nil → switch falls through to default → no-op.
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{}); err != nil {
		t.Fatal(err)
	}
	if fs.posts != 0 {
		t.Fatal("default branch should not post")
	}
}

func TestOneLineTruncates(t *testing.T) {
	in := strings.Repeat("a", 250) + "\n" + "b"
	got := oneLine(in)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis truncation, got %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatal("newlines should be replaced")
	}
}

// TestOneLineRuneSafe pins the rune-safe truncation: feeding 250
// multibyte runes (each 4 bytes in UTF-8) must yield exactly 200
// runes + "…", with no truncated codepoint at the tail.
func TestOneLineRuneSafe(t *testing.T) {
	in := strings.Repeat("🌲", 250)
	got := oneLine(in)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis; got %q", got)
	}
	// Must end with a complete rune before the ellipsis, never a
	// partial UTF-8 byte sequence.
	trimmed := strings.TrimSuffix(got, "…")
	if r := []rune(trimmed); len(r) != 200 || string(r) != strings.Repeat("🌲", 200) {
		t.Fatalf("not rune-safe: %d runes, %q", len(r), trimmed)
	}
}

func TestContentBlockTextNil(t *testing.T) {
	if got := contentBlockText(acp.ContentBlock{}); got != "" {
		t.Fatalf("got %q", got)
	}
}
