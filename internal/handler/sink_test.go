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
	return newStreamingSink(stream, false), fs
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

func TestSinkStatusHeaderOnMessageFirst(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		Meta: map[string]any{
			statusline.ExtensionID: map[string]any{"plan": "1/2"},
		},
	})
	// A Plan update produces no visible chunk (suppressed), so the
	// status header must land on the first real message chunk instead.
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			Plan: &acp.SessionUpdatePlan{Entries: []acp.PlanEntry{{Content: "do thing"}}},
		},
	})
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "answer"}},
			},
		},
	})
	body := strings.Join(fs.bodies, "")
	if !strings.Contains(body, "> _1/2_") {
		t.Fatalf("expected header on first message write; body=%q", body)
	}
	if strings.Contains(body, "do thing") {
		t.Fatalf("plan entry should be suppressed; body=%q", body)
	}
}

func TestSinkPlanSuppressed(t *testing.T) {
	// Plan updates are suppressed entirely (matches poe-acp), so a
	// non-empty plan must post nothing to the Slack thread.
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
	if fs.posts != 0 {
		t.Fatalf("plan should be suppressed; bodies=%q", fs.bodies)
	}
}

func TestSinkHideThinking(t *testing.T) {
	fs := newFakeSlack()
	t.Cleanup(fs.close)
	stream := slackproto.NewPostStreamer(fs.client(), "C1", "1.0")
	sink := newStreamingSink(stream, true) // hide_thinking on
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "pondering"}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if fs.posts != 0 {
		t.Fatalf("thought should be suppressed when hideThinking; bodies=%q", fs.bodies)
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

// ---- abstainSink ----

func msgChunk(text string) acp.SessionNotification {
	return acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: text}},
			},
		},
	}
}

func newAbstain(t *testing.T, sentinel string) (*abstainSink, *fakeSlack) {
	t.Helper()
	base, fs := newSinkAndCapture(t)
	// Flush every Append so tests can observe forwarded content without
	// waiting on the throttle (real flow flushes via watchdog/Close).
	base.stream.SetMinInterval(0)
	return newAbstainSink(base, sentinel), fs
}

// Exact sentinel across the whole turn -> nothing posted, Finalize
// reports abstained.
func TestAbstainExactSentinelSuppresses(t *testing.T) {
	a, fs := newAbstain(t, "<<SILENT>>")
	if err := a.OnUpdate(context.Background(), msgChunk("<<SILENT>>")); err != nil {
		t.Fatal(err)
	}
	if fs.posts != 0 {
		t.Fatalf("sentinel must not post; posts=%d", fs.posts)
	}
	abstained, err := a.Finalize(context.Background())
	if err != nil || !abstained {
		t.Fatalf("expected abstained,no-err; got %v,%v", abstained, err)
	}
}

// Empty output -> abstained.
func TestAbstainEmptyOutputSuppresses(t *testing.T) {
	a, fs := newAbstain(t, "<<SILENT>>")
	abstained, err := a.Finalize(context.Background())
	if err != nil || !abstained {
		t.Fatalf("empty output should abstain; got %v,%v", abstained, err)
	}
	if fs.posts != 0 {
		t.Fatal("no post expected on empty abstain")
	}
}

// Sentinel streamed in prefix chunks then completed -> still abstain.
func TestAbstainStreamedSentinelSuppresses(t *testing.T) {
	a, fs := newAbstain(t, "<<SILENT>>")
	for _, c := range []string{"<<", "SIL", "ENT", ">>"} {
		if err := a.OnUpdate(context.Background(), msgChunk(c)); err != nil {
			t.Fatal(err)
		}
	}
	if fs.posts != 0 {
		t.Fatalf("streamed sentinel must not post; posts=%d", fs.posts)
	}
	abstained, _ := a.Finalize(context.Background())
	if !abstained {
		t.Fatal("streamed exact sentinel should abstain")
	}
}

// Divergence from the sentinel mid-stream -> flush buffered + forward.
func TestAbstainDivergenceFlushes(t *testing.T) {
	a, fs := newAbstain(t, "<<SILENT>>")
	// First chunk is a strict prefix, second diverges.
	if err := a.OnUpdate(context.Background(), msgChunk("<<")); err != nil {
		t.Fatal(err)
	}
	if err := a.OnUpdate(context.Background(), msgChunk("hello there")); err != nil {
		t.Fatal(err)
	}
	// Subsequent chunk should forward immediately via the sent fast-path.
	if err := a.OnUpdate(context.Background(), msgChunk(" more")); err != nil {
		t.Fatal(err)
	}
	abstained, err := a.Finalize(context.Background())
	if err != nil || abstained {
		t.Fatalf("diverged output must not abstain; got %v,%v", abstained, err)
	}
	body := strings.Join(fs.bodies, "")
	if !strings.Contains(body, "hello there") || !strings.Contains(body, "more") {
		t.Fatalf("expected flushed body to contain both chunks; got %q", body)
	}
}

// Non-sentinel first chunk commits immediately (divergence on first).
func TestAbstainNonSentinelPostsImmediately(t *testing.T) {
	a, fs := newAbstain(t, "<<SILENT>>")
	if err := a.OnUpdate(context.Background(), msgChunk("Sure!")); err != nil {
		t.Fatal(err)
	}
	if fs.posts == 0 || !strings.Contains(strings.Join(fs.bodies, ""), "Sure!") {
		t.Fatalf("non-sentinel must post; bodies=%q", fs.bodies)
	}
}

// Partial sentinel that never completes -> Finalize flushes it (not lost).
func TestAbstainPartialSentinelFlushedAtFinalize(t *testing.T) {
	a, fs := newAbstain(t, "<<SILENT>>")
	if err := a.OnUpdate(context.Background(), msgChunk("<<SIL")); err != nil {
		t.Fatal(err)
	}
	// Nothing posted yet (still a strict prefix).
	if fs.posts != 0 {
		t.Fatal("strict-prefix must buffer, not post")
	}
	abstained, err := a.Finalize(context.Background())
	if err != nil || abstained {
		t.Fatalf("partial sentinel is real output; got %v,%v", abstained, err)
	}
	if !strings.Contains(strings.Join(fs.bodies, ""), "<<SIL") {
		t.Fatalf("partial output must be flushed; bodies=%q", fs.bodies)
	}
}

// Finalize after already-sent is a no-op returning false.
func TestAbstainFinalizeAfterSent(t *testing.T) {
	a, _ := newAbstain(t, "<<SILENT>>")
	if err := a.OnUpdate(context.Background(), msgChunk("plain reply")); err != nil {
		t.Fatal(err)
	}
	abstained, err := a.Finalize(context.Background())
	if err != nil || abstained {
		t.Fatalf("already-sent finalize should be false,nil; got %v,%v", abstained, err)
	}
}

// Empty-render updates (no chunk) are ignored by abstain.
func TestAbstainIgnoresEmptyRender(t *testing.T) {
	a, fs := newAbstain(t, "<<SILENT>>")
	if err := a.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{}}},
	}); err != nil {
		t.Fatal(err)
	}
	if fs.posts != 0 {
		t.Fatal("empty render must produce nothing")
	}
}

// Delegation methods pass through to the wrapped sink.
func TestAbstainDelegation(t *testing.T) {
	a, _ := newAbstain(t, "<<SILENT>>")
	a.SetProviderEmoji("🏛️")
	if got := a.Status().ProviderEmoji; got != "🏛️" {
		t.Fatalf("SetProviderEmoji/Status delegation failed; got %q", got)
	}
}

func TestDiscardSinkNoop(t *testing.T) {
	if err := (discardSink{}).OnUpdate(context.Background(), msgChunk("x")); err != nil {
		t.Fatalf("discardSink must never error; got %v", err)
	}
}

// Append error during divergence flush is propagated.
func TestAbstainDivergenceFlushError(t *testing.T) {
	a, fs := newAbstain(t, "<<SILENT>>")
	fs.postErr = true // first Append (postMessage) will fail
	if err := a.OnUpdate(context.Background(), msgChunk("diverged now")); err == nil {
		t.Fatal("expected Append error to propagate from divergence flush")
	}
}

// Append error during partial-sentinel Finalize flush is propagated.
func TestAbstainFinalizeFlushError(t *testing.T) {
	a, fs := newAbstain(t, "<<SILENT>>")
	if err := a.OnUpdate(context.Background(), msgChunk("<<SIL")); err != nil {
		t.Fatal(err)
	}
	fs.postErr = true // Finalize's flush (postMessage) will fail
	if _, err := a.Finalize(context.Background()); err == nil {
		t.Fatal("expected Finalize flush error to propagate")
	}
}

// Regression: an agent that emits thought chunks and then the sentinel
// must still abstain. Thoughts are suppressed on the abstain path, so
// they never diverge the buffered output from the sentinel.
func TestAbstainThoughtsDoNotBreakSentinel(t *testing.T) {
	a, fs := newAbstain(t, "<<SILENT>>")
	if err := a.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "should I reply?"}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "<<SILENT>>"}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	abstained, err := a.Finalize(context.Background())
	if err != nil || !abstained {
		t.Fatalf("expected abstained,no-err; got %v,%v", abstained, err)
	}
	if fs.posts != 0 {
		t.Fatalf("nothing should be posted; bodies=%q", fs.bodies)
	}
}
