package handler

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/slack-acp/internal/router"
	"github.com/kfet/slack-acp/internal/slackproto"
)

// TestCommandsWorkWithBrokenModel is the bug that motivated wiring the
// shared command core: `!model` used to be sent to the agent as a
// prompt, so a broken model could not be switched away from. Every
// standard command is now answered by the relay, and the agent is never
// prompted for one.
func TestCommandsWorkWithBrokenModel(t *testing.T) {
	fa := newFakeAgent()
	fa.models = []client.ModelInfo{{ID: "a/good"}, {ID: "a/broken"}}
	fa.currentModel = "a/broken"
	var prompts int32
	block := make(chan struct{})
	started := make(chan struct{}, 4)
	fa.promptHook = func(ctx context.Context, _ acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		atomic.AddInt32(&prompts, 1)
		started <- struct{}{}
		select {
		case <-block:
		case <-ctx.Done():
		}
		return "", errors.New("model a/broken is broken")
	}
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()
	h := New(Config{Router: r, API: fs.client(), NoProgressTimeout: 5 * time.Second})
	ev := func(text string) slackproto.Event {
		return slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "2.0", Text: text, IsDM: true}
	}

	h.Handle(context.Background(), ev("hello"))
	<-started
	replies := func() string {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return strings.Join(fs.bodies, "\n")
	}
	for _, c := range []struct{ text, want string }{
		{"!status", "turn running: yes"},
		{"!stop", "Interrupted"},
		{"!model", "a/good"},
		{"!model a/good", "a/good"},
		{"!new", "Fresh session"},
	} {
		h.Handle(context.Background(), ev(c.text))
		if !strings.Contains(replies(), c.want) {
			t.Fatalf("%s: no %q in replies:\n%s", c.text, c.want, replies())
		}
	}
	close(block)
	waitForIdle(t, h)
	if n := atomic.LoadInt32(&prompts); n != 1 {
		t.Fatalf("agent prompted %d times; commands must never reach it", n)
	}
	if strings.Contains(replies(), "**") {
		t.Fatalf("CommonMark bold leaked into Slack:\n%s", replies())
	}

	// The next real turn runs on the chosen model, on a fresh session.
	fa.promptHook = nil
	h.Handle(context.Background(), ev("again"))
	waitForIdle(t, h)
	fa.mu.Lock()
	set := append([]string(nil), fa.setModels...)
	fa.mu.Unlock()
	if len(set) != 1 || !strings.HasSuffix(set[0], "=a/good") {
		t.Fatalf("SetModel calls = %v", set)
	}
	if atomic.LoadInt32(&fa.dropCount) == 0 {
		t.Fatal("!new did not drop the session")
	}
}

func TestReplyPostFailureIsTolerated(t *testing.T) {
	fs := newFakeSlack()
	defer fs.close()
	fs.postErr = true
	r := newTestRouter(t, newFakeAgent())
	h := New(Config{Router: r, API: fs.client()})
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "2.0", Text: "!status", IsDM: true})
	if h.inflightCount() != 0 {
		t.Fatal("a command started a turn")
	}
}

func TestRouterSessionsAdapter(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	s := routerSessions{r}
	key := router.ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	if _, _, ok := s.Live(key.String()); ok || s.Len() != 0 {
		t.Fatal("live before a turn")
	}
	if _, err := r.GetOrCreate(context.Background(), key, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Live(key.String()); !ok {
		t.Fatal("not live")
	}
	s.Cancel(context.Background(), key.String())
	if atomic.LoadInt32(&fa.cancelCount) != 1 {
		t.Fatal("cancel")
	}
	if parseKey("C1/1.0") != key {
		t.Fatal("parseKey")
	}
	if m, cur := (noAgent{}).Models(); m != nil || cur != "" || (noAgent{}).AvailableCommands() != nil {
		t.Fatal("noAgent")
	}
}
