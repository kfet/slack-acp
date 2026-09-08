package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/slack-acp/internal/slackproto"
)

func chunk(text string) acp.SessionNotification {
	return acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock(text)},
		},
	}
}

func toolUpdate() acp.SessionNotification {
	return acp.SessionNotification{
		Update: acp.SessionUpdate{
			ToolCallUpdate: &acp.SessionToolCallUpdate{ToolCallId: "t1"},
		},
	}
}

func event() slackproto.Event {
	return slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "x"}
}

// TestWedgedTurnIsCutAndSaysSo is the regression for the bug this
// mechanism exists for: a turn that stops making progress is cut, the
// partial answer survives, and the note says what happened instead of a
// bare "_error: context deadline exceeded_".
func TestWedgedTurnIsCutAndSaysSo(t *testing.T) {
	fa := newFakeAgent()
	fa.promptHook = func(ctx context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		fa.emit(sid, chunk("half an ans"))
		<-ctx.Done() // wedged: nothing more ever happens
		return "", ctx.Err()
	}
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	h := New(Config{Router: r, API: fs.client(), NoProgressTimeout: 150 * time.Millisecond})
	h.Handle(context.Background(), event())
	waitForIdle(t, h)

	final := lastBody(t, fs)
	if !strings.Contains(final, "half an ans") {
		t.Fatalf("the partial answer was lost: %q", final)
	}
	if !strings.Contains(final, "no tool activity") {
		t.Fatalf("the cut must name its reason, got %q", final)
	}
	if strings.Contains(final, "deadline exceeded") {
		t.Fatalf("the bare deadline error is back: %q", final)
	}
	// The other half of the incident — telling the agent to stop, so
	// its tool does not run on — lives in acp-kit's AgentProc.Prompt
	// and is tested there; this fake bypasses that layer entirely.
}

// TestToolActivityKeepsATurnAlive is the other half of the contract: a
// turn far longer than the no-progress window is NOT cut, because tool
// calls keep landing.
func TestToolActivityKeepsATurnAlive(t *testing.T) {
	window := 100 * time.Millisecond
	fa := newFakeAgent()
	fa.promptHook = func(ctx context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		for i := 0; i < 8; i++ {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(window / 4):
			}
			fa.emit(sid, toolUpdate())
		}
		fa.emit(sid, chunk("done"))
		return acp.StopReasonEndTurn, nil
	}
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	h := New(Config{Router: r, API: fs.client(), NoProgressTimeout: window})
	h.Handle(context.Background(), event())
	waitForIdle(t, h)

	if final := lastBody(t, fs); !strings.Contains(final, "done") || strings.Contains(final, "wedged") {
		t.Fatalf("a working turn was cut: %q", final)
	}
}

// TestTurnCeilingCutsSaysSo: when an operator opts into the ceiling it
// fires despite progress, and names itself so the difference from a
// wedge is visible in the thread.
func TestTurnCeilingCutsSaysSo(t *testing.T) {
	fa := newFakeAgent()
	fa.promptHook = func(ctx context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		fa.emit(sid, chunk("partial"))
		for {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(20 * time.Millisecond):
				fa.emit(sid, toolUpdate())
			}
		}
	}
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	h := New(Config{Router: r, API: fs.client(),
		NoProgressTimeout: time.Hour, TurnCeiling: 150 * time.Millisecond})
	h.Handle(context.Background(), event())
	waitForIdle(t, h)

	final := lastBody(t, fs)
	if !strings.Contains(final, "ceiling") || !strings.Contains(final, "partial") {
		t.Fatalf("body = %q", final)
	}
}

// TestSupersededTurnReadsAsSuperseded: a turn the relay cancelled on
// purpose must not surface as an error.
func TestSupersededTurnReadsAsSuperseded(t *testing.T) {
	fa := newFakeAgent()
	entered := make(chan struct{}, 4)
	fa.promptHook = func(ctx context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		fa.emit(sid, chunk("first"))
		entered <- struct{}{}
		<-ctx.Done()
		return "", ctx.Err()
	}
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	h := New(Config{Router: r, API: fs.client(), NoProgressTimeout: time.Hour})
	h.Handle(context.Background(), event())
	<-entered

	fa.mu.Lock()
	fa.promptHook = nil
	fa.promptStop = acp.StopReasonEndTurn
	fa.mu.Unlock()
	ev := event()
	ev.TS = "2.0"
	h.Handle(context.Background(), ev)
	waitForIdle(t, h)

	if !anyContains(fs.bodies, "superseded by your next message") {
		t.Fatalf("bodies = %q", fs.bodies)
	}
	if anyContains(fs.bodies, "_error: context canceled_") {
		t.Fatalf("raw cancellation leaked: %q", fs.bodies)
	}
}
