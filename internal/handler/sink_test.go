package handler

import (
	"context"
	"strings"
	"testing"
	"time"

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
	if got := sink.Status(); got.Mood != "" || got.Plan != "" || got.ProviderEmoji != "" || got.Model != "" {
		t.Fatalf("expected zero status before any input; got %+v", got)
	}
	sink.SetModelInfo("🏛️", "opus-4.5")
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		Meta: map[string]any{
			statusline.ExtensionID: map[string]any{"mood": "curious", "plan": "1/2"},
		},
	})
	got := sink.Status()
	if got.ProviderEmoji != "🏛️" || got.Model != "opus-4.5" || got.Mood != "curious" || got.Plan != "1/2" {
		t.Fatalf("Status() did not reflect parsed meta + model info; got %+v", got)
	}
}

// TestSinkStatusFooterAppendedOnce: the status line is APPENDED at the
// end of the answer, in italics after a blank line — never in front of
// the body — and Close is the only thing that emits it.
func TestSinkStatusFooterAppendedOnce(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	sink.stream.SetMinInterval(0)
	sink.SetModelInfo("🏛️", "opus-4.5")
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Meta: map[string]any{
			statusline.ExtensionID: map[string]any{"mood": "steady", "plan": "3/8"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"hello", " world"} {
		if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
			Update: acp.SessionUpdate{
				AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
					Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: c}},
				},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Mid-turn: no footer anywhere yet.
	if mid := strings.Join(fs.bodies, ""); strings.Contains(mid, "opus-4.5") {
		t.Fatalf("footer must not appear before the turn ends; bodies=%q", mid)
	}
	if err := sink.stream.Close(context.Background(), sink.maybeAppendFooter()); err != nil {
		t.Fatal(err)
	}

	const want = "\n\n_🏛️ opus-4.5 • steady • 3/8_"
	final := fs.bodies[len(fs.bodies)-1]
	if !strings.HasSuffix(final, want) {
		t.Fatalf("final body must END with the footer; got %q", final)
	}
	if n := strings.Count(final, "opus-4.5"); n != 1 {
		t.Fatalf("footer must appear exactly once in the final body; got %d in %q", n, final)
	}
	if !strings.HasPrefix(final, "hello world") {
		t.Fatalf("answer body must come first, unadorned; got %q", final)
	}
	// A second consideration must yield nothing — idempotent.
	if again := sink.maybeAppendFooter(); again != "" {
		t.Fatalf("maybeAppendFooter must be idempotent; second call = %q", again)
	}
}

// TestSinkStatusFooterUsesLatestSnapshot is the whole point of moving
// the line to the bottom: mood and plan that arrive AFTER the first
// chunk still make it into the rendered line. Under the old prepend
// they could not.
func TestSinkStatusFooterUsesLatestSnapshot(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	sink.stream.SetMinInterval(0)
	sink.SetModelInfo("🏛️", "opus-4.5")
	// Answer first, status only once the agent is under way.
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "answer"}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		Meta: map[string]any{
			statusline.ExtensionID: map[string]any{"mood": "engaged", "plan": "4/7"},
		},
	})
	_ = sink.stream.Close(context.Background(), sink.maybeAppendFooter())
	final := fs.bodies[len(fs.bodies)-1]
	if !strings.HasSuffix(final, "\n\n_🏛️ opus-4.5 • engaged • 4/7_") {
		t.Fatalf("footer must carry the LATEST status; got %q", final)
	}
}

// TestSinkStatusFooterEmptyNoOp: no model info and no agent _meta →
// nothing is appended, not even a stray blank line or empty italics.
func TestSinkStatusFooterEmptyNoOp(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	sink.stream.SetMinInterval(0)
	if err := sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "hi"}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := sink.maybeAppendFooter(); got != "" {
		t.Fatalf("no status → no footer; got %q", got)
	}
	_ = sink.stream.Close(context.Background(), "")
	final := fs.bodies[len(fs.bodies)-1]
	if final != "hi" {
		t.Fatalf("body must be untouched; got %q", final)
	}
}

// TestSinkStatusFooterNotOnContentlessTurn: a turn that produced no
// user-visible output has nothing to sign. The streamer's "_thinking…_"
// fallback body is not an answer.
func TestSinkStatusFooterNotOnContentlessTurn(t *testing.T) {
	sink, _ := newSinkAndCapture(t)
	sink.SetModelInfo("🏛️", "opus-4.5")
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		Meta: map[string]any{
			statusline.ExtensionID: map[string]any{"mood": "steady", "plan": "2/5"},
		},
	})
	if got := sink.maybeAppendFooter(); got != "" {
		t.Fatalf("contentless turn must not be signed; got %q", got)
	}
}

// TestSinkStatusFooterAfterThoughtOnly: a thought one-liner is
// user-visible content, so a thought-only turn still gets signed — and
// the footer lands after it.
func TestSinkStatusFooterAfterThoughtOnly(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	sink.stream.SetMinInterval(0)
	sink.SetModelInfo("🏛️", "opus-4.5")
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
	_ = sink.stream.Close(context.Background(), sink.maybeAppendFooter())
	final := fs.bodies[len(fs.bodies)-1]
	if !strings.HasSuffix(final, "\n\n_🏛️ opus-4.5 • curious_") {
		t.Fatalf("expected footer after the thought; got %q", final)
	}
	if strings.Index(final, "hmm") > strings.Index(final, "opus-4.5") {
		t.Fatalf("footer must follow the body; got %q", final)
	}
}

// TestSinkStatusFooterModelOnly is the backwards-compat case: the
// agent emits no _meta at all, so only the model identity segment
// survives. The footer is still appended.
func TestSinkStatusFooterModelOnly(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	sink.stream.SetMinInterval(0)
	sink.SetModelInfo("🌐", "gpt-5-codex")
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "payload"}},
			},
		},
	})
	_ = sink.stream.Close(context.Background(), sink.maybeAppendFooter())
	final := fs.bodies[len(fs.bodies)-1]
	if !strings.HasSuffix(final, "\n\n_🌐 gpt-5-codex_") {
		t.Fatalf("model-only footer missing; got %q", final)
	}
	// No mood/plan dividers should be present.
	if strings.Contains(final, "gpt-5-codex • ") {
		t.Fatalf("unexpected mood/plan segment; got %q", final)
	}
}

// TestSinkStatusFooterSurvivesLaterEdits pins THE surface-specific
// hazard. Slack has no incremental stream: the relay owns one message
// and every chat.update re-posts the WHOLE accumulated buffer. So a
// footer written side-band (its own chat.update, or a placeholder
// frame) would be silently erased by the next edit, and a footer
// appended mid-turn would be stranded above later chunks.
//
// Riding in Close's suffix defeats both: it goes INTO the buffer, so
// every subsequent re-post replays it, and it is by construction the
// last thing in that buffer. Close also seals the stream, so the
// watchdog flush and the spinner's placeholder update can no longer
// touch the message at all.
func TestSinkStatusFooterSurvivesLaterEdits(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	sink.stream.SetMinInterval(0)
	sink.SetModelInfo("🏛️", "opus-4.5")
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		Meta: map[string]any{
			statusline.ExtensionID: map[string]any{"mood": "steady", "plan": "2/5"},
		},
	})
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "body text"}},
			},
		},
	})
	_ = sink.stream.Close(context.Background(), sink.maybeAppendFooter())
	sealed := fs.bodies[len(fs.bodies)-1]
	const want = "\n\n_🏛️ opus-4.5 • steady • 2/5_"
	if !strings.HasSuffix(sealed, want) {
		t.Fatalf("footer missing from the sealed body; got %q", sealed)
	}

	// Everything that could still write after the turn ends must now be
	// inert: a late spinner frame, a watchdog flush, a duplicate Close.
	alive, err := sink.stream.UpdatePlaceholder(context.Background(),
		statusline.Spinner(sink.Status(), "..."))
	if err != nil {
		t.Fatal(err)
	}
	if alive {
		t.Fatal("placeholder window must be shut after Close")
	}
	if err := sink.stream.FlushIfPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sink.stream.Close(context.Background(), "\n\n_second footer_"); err != nil {
		t.Fatal(err)
	}
	if got := fs.bodies[len(fs.bodies)-1]; got != sealed {
		t.Fatalf("post-Close writes must not touch the message; last body %q != sealed %q", got, sealed)
	}
	if n := strings.Count(strings.Join(fs.bodies, ""), want); n != 1 {
		t.Fatalf("footer must have been written exactly once across all bodies; got %d", n)
	}
}

// TestSinkStatusFooterCountsBufferedContent: on this surface a whole
// answer can still be sitting unsent in the streamer's buffer when the
// turn ends — the throttle skipped every flush, or the abstain sink
// released it all at once at Finalize. An answer that is about to be
// written is an answer, so it must still be signed.
func TestSinkStatusFooterCountsBufferedContent(t *testing.T) {
	sink, fs := newSinkAndCapture(t)
	// Throttle wide open: nothing reaches Slack during the turn.
	sink.stream.SetMinInterval(time.Hour)
	sink.SetModelInfo("🏛️", "opus-4.5")
	_ = sink.OnUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "buffered answer"}},
			},
		},
	})
	_ = sink.stream.Close(context.Background(), sink.maybeAppendFooter())
	final := fs.bodies[len(fs.bodies)-1]
	if !strings.Contains(final, "buffered answer") {
		t.Fatalf("buffered answer must be flushed by Close; got %q", final)
	}
	if !strings.HasSuffix(final, "\n\n_🏛️ opus-4.5_") {
		t.Fatalf("a buffered-but-unsent answer must still be signed; got %q", final)
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
	a.SetModelInfo("🏛️", "opus-4.5")
	st := a.Status()
	if st.ProviderEmoji != "🏛️" || st.Model != "opus-4.5" {
		t.Fatalf("SetModelInfo/Status delegation failed; got %+v", st)
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
