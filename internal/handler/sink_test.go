package handler

import (
	"context"
	"strings"
	"testing"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/slack-acp/internal/slackproto"
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

func TestSinkToolCall(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{Title: "Run tests"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(fs.bodies, ""), "Run tests") {
		t.Fatalf("expected tool-call line; bodies=%q", fs.bodies)
	}
}

func TestSinkToolCallEmptyTitle(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Still posts (the rendering just has empty backticks).
	if fs.posts == 0 {
		t.Fatal("expected a post even on empty title")
	}
}

func TestSinkToolCallUpdate(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	statusCompleted := acp.ToolCallStatus("completed")
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			ToolCallUpdate: &acp.SessionToolCallUpdate{Status: &statusCompleted},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(fs.bodies, ""), "completed") {
		t.Fatalf("expected completed marker; bodies=%q", fs.bodies)
	}

	// Pending status (not completed/failed) → no post.
	sink2, fs2 := newSinkAndCapture(t)
	pending := acp.ToolCallStatus("pending")
	_ = sink2.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{Status: &pending}},
	})
	if fs2.posts != 0 {
		t.Fatal("non-terminal status should not post")
	}

	// Nil status → no post.
	sink3, fs3 := newSinkAndCapture(t)
	_ = sink3.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{}},
	})
	if fs3.posts != 0 {
		t.Fatal("nil status should not post")
	}

	// Failed status posts.
	sink4, fs4 := newSinkAndCapture(t)
	failed := acp.ToolCallStatus("failed")
	_ = sink4.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{Status: &failed}},
	})
	if fs4.posts == 0 || !strings.Contains(strings.Join(fs4.bodies, ""), "failed") {
		t.Fatalf("expected failed marker; bodies=%q", fs4.bodies)
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

func TestContentBlockTextNil(t *testing.T) {
	if got := contentBlockText(acp.ContentBlock{}); got != "" {
		t.Fatalf("got %q", got)
	}
}
