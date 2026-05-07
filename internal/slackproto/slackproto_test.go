package slackproto

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// ---- helpers ----

func TestStripMention(t *testing.T) {
	cases := []struct{ in, bot, want string }{
		{"<@U1> hello", "U1", "hello"},
		{"  <@U1>   hi there  ", "U1", "hi there"},
		{"hey <@U1> what about <@U1> this", "U1", "hey  what about  this"},
		{"plain text", "U1", "plain text"},
		{"<@U2> hi", "U1", "<@U2> hi"},
		{"  spaces  ", "", "spaces"}, // empty botID branch
	}
	for _, c := range cases {
		if got := stripMention(c.in, c.bot); got != c.want {
			t.Errorf("stripMention(%q,%q)=%q want %q", c.in, c.bot, got, c.want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if firstNonEmpty("", "b") != "b" {
		t.Fail()
	}
	if firstNonEmpty("a", "b") != "a" {
		t.Fail()
	}
}

// ---- New / token validation ----

type stubHandler struct {
	mu    sync.Mutex
	calls []Event
}

func (h *stubHandler) Handle(_ context.Context, ev Event) {
	h.mu.Lock()
	h.calls = append(h.calls, ev)
	h.mu.Unlock()
}

func (h *stubHandler) seen() []Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Event, len(h.calls))
	copy(out, h.calls)
	return out
}

func TestNewTokenValidation(t *testing.T) {
	cases := []struct {
		bot, app string
		wantErr  bool
	}{
		{"", "", true},
		{"x", "", true},
		{"xoxb-x", "x", true},       // app token must start with xapp-
		{"x", "xapp-x", true},       // bot token must start with xoxb-
		{"xoxb-x", "xapp-x", false}, // ok
	}
	for _, c := range cases {
		_, err := New(c.bot, c.app, &stubHandler{})
		if (err != nil) != c.wantErr {
			t.Errorf("New(%q,%q): err=%v want=%v", c.bot, c.app, err, c.wantErr)
		}
	}
}

func TestAccessors(t *testing.T) {
	c, err := New("xoxb-x", "xapp-x", &stubHandler{})
	if err != nil {
		t.Fatal(err)
	}
	if c.API() == nil {
		t.Fatal("API nil")
	}
	c.botUserID = "Ubot"
	if c.BotUserID() != "Ubot" {
		t.Fatal("bot id")
	}
}

// ---- handleEventsAPI / deliver ----

func newClientForDispatch(t *testing.T, h Handler) *Client {
	t.Helper()
	c, err := New("xoxb-x", "xapp-x", h)
	if err != nil {
		t.Fatal(err)
	}
	c.botUserID = "Ubot"
	return c
}

func TestHandleEventsAPIAppMention(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.AppMentionEvent{
				User:            "U1",
				Channel:         "C1",
				ThreadTimeStamp: "",
				TimeStamp:       "100.0",
				Text:            "<@Ubot> hi",
			},
		},
	})
	got := h.seen()
	if len(got) != 1 || got[0].UserID != "U1" || got[0].Text != "hi" || got[0].ThreadTS != "100.0" {
		t.Fatalf("got %+v", got)
	}
	if got[0].BotUserID != "Ubot" {
		t.Fatal("bot user id not stamped")
	}
}

func TestHandleEventsAPINotCallback(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	// URLVerification etc. is short-circuited.
	c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{Type: slackevents.URLVerification})
	if len(h.seen()) != 0 {
		t.Fatal("non-callback should be dropped")
	}
}

func TestHandleEventsAPIDM(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{
				User:            "U1",
				Channel:         "D1",
				ChannelType:     "im",
				TimeStamp:       "100.0",
				ThreadTimeStamp: "",
				Text:            "hi",
			},
		},
	})
	got := h.seen()
	if len(got) != 1 || !got[0].IsDM {
		t.Fatalf("got %+v", got)
	}
}

func TestHandleEventsAPIDropsBotsAndEditsAndNonIM(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	cases := []*slackevents.MessageEvent{
		{User: "Ubot", Channel: "D1", ChannelType: "im", TimeStamp: "1"},                           // self
		{User: "U1", BotID: "B1", Channel: "D1", ChannelType: "im", TimeStamp: "1"},                // bot
		{User: "U1", SubType: "message_changed", Channel: "D1", ChannelType: "im", TimeStamp: "1"}, // edit
		{User: "", Channel: "D1", ChannelType: "im", TimeStamp: "1"},                               // empty user
		{User: "U1", Channel: "C2", ChannelType: "channel", TimeStamp: "1"},                        // not-im
	}
	for _, m := range cases {
		c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
			Type:       slackevents.CallbackEvent,
			InnerEvent: slackevents.EventsAPIInnerEvent{Data: m},
		})
	}
	if len(h.seen()) != 0 {
		t.Fatalf("expected all dropped, got %+v", h.seen())
	}
}

func TestHandleEventsAPIIgnoresUnknownInner(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type:       slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Data: &slackevents.ChannelCreatedEvent{}},
	})
	if len(h.seen()) != 0 {
		t.Fatal("unknown inner should be ignored")
	}
}

func TestDeliverNilHandler(t *testing.T) {
	c, err := New("xoxb-x", "xapp-x", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Must not panic.
	c.deliver(context.Background(), Event{})
}

// ---- dispatch ----

func TestDispatchMethods(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	// All non-EventsAPI variants are debug-logged and ignored.
	for _, et := range []socketmode.EventType{
		socketmode.EventTypeConnecting,
		socketmode.EventTypeConnected,
		socketmode.EventTypeHello,
		socketmode.EventTypeDisconnect,
		socketmode.EventTypeIncomingError, // → default branch
	} {
		c.dispatch(context.Background(), socketmode.Event{Type: et})
	}
	if len(h.seen()) != 0 {
		t.Fatal("non-EventsAPI dispatch should not deliver")
	}

	// EventsAPI with non-matching data type → return early after Ack.
	req := socketmode.Request{}
	c.dispatch(context.Background(), socketmode.Event{
		Type:    socketmode.EventTypeEventsAPI,
		Data:    "wrong",
		Request: &req,
	})
}

// ---- Run: error path ----

func TestRunAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
	}))
	defer srv.Close()
	api := slack.New("xoxb-x", slack.OptionAPIURL(srv.URL+"/"), slack.OptionAppLevelToken("xapp-x"))
	c := &Client{api: api, sm: socketmode.New(api), handler: &stubHandler{}}
	if err := c.Run(context.Background()); err == nil {
		t.Fatal("expected auth error")
	}
}

// TestRunSocketModeFails covers the happy auth.test → RunContext path.
// The fake server returns ok for auth.test but rejects apps.connections.open
// so socketmode's RunContext returns an error.
func TestRunSocketModeFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"user":"botname","user_id":"Ubot","team":"T","team_id":"T1"}`))
		case "/apps.connections.open":
			_, _ = w.Write([]byte(`{"ok":false,"error":"invalid_auth"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	api := slack.New("xoxb-x", slack.OptionAPIURL(srv.URL+"/"), slack.OptionAppLevelToken("xapp-x"))
	c := &Client{api: api, sm: socketmode.New(api), handler: &stubHandler{}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Run(ctx); err == nil {
		t.Fatal("expected socketmode error")
	}
	if c.botUserID != "Ubot" {
		t.Fatalf("auth response not stamped: %q", c.botUserID)
	}
}

// ---- consume: ctx cancel exits cleanly ----

func TestConsumeCancels(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.consume(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("consume did not exit on ctx cancel")
	}
}

// TestConsumeChannelClose covers the !ok branch in consume's receive.
func TestConsumeChannelClose(t *testing.T) {
	t.Skip("consume's !ok branch is on socketmode.Client.Events which we cannot close from outside")
}

// TestDispatchEventsAPIHappy drives the EventsAPI branch end-to-end so
// dispatch reaches sm.Ack + handleEventsAPI. We pass a real Request
// pointer (the json field is parsed, not used) and an EventsAPIEvent.
func TestDispatchEventsAPIHappy(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	req := socketmode.Request{Type: "events_api", EnvelopeID: "E1"}
	c.dispatch(context.Background(), socketmode.Event{
		Type:    socketmode.EventTypeEventsAPI,
		Request: &req,
		Data: slackevents.EventsAPIEvent{
			Type: slackevents.CallbackEvent,
			InnerEvent: slackevents.EventsAPIInnerEvent{
				Data: &slackevents.AppMentionEvent{User: "U1", Channel: "C1", TimeStamp: "1.0", Text: "<@Ubot> hi"},
			},
		},
	})
	if len(h.seen()) != 1 {
		t.Fatalf("expected delivery, got %d", len(h.seen()))
	}
}

// ---- PostStreamer ----

type fakeSlackSrv struct {
	srv *httptest.Server

	mu        sync.Mutex
	posts     int32
	updates   int32
	postErr   bool
	updateErr bool
	bodies    []string
}

func newFakeSlackSrv() *fakeSlackSrv {
	fs := &fakeSlackSrv{}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fs.posts, 1)
		_ = r.ParseForm()
		fs.mu.Lock()
		fs.bodies = append(fs.bodies, r.FormValue("text"))
		err := fs.postErr
		fs.mu.Unlock()
		if err {
			_, _ = w.Write([]byte(`{"ok":false,"error":"bad"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1.0","message":{"text":"x"}}`))
	})
	mux.HandleFunc("/chat.update", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fs.updates, 1)
		_ = r.ParseForm()
		fs.mu.Lock()
		fs.bodies = append(fs.bodies, r.FormValue("text"))
		err := fs.updateErr
		fs.mu.Unlock()
		if err {
			_, _ = w.Write([]byte(`{"ok":false,"error":"nope"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1.0","text":"x"}`))
	})
	fs.srv = httptest.NewServer(mux)
	return fs
}

func (fs *fakeSlackSrv) close() { fs.srv.Close() }
func (fs *fakeSlackSrv) client() *slack.Client {
	return slack.New("xoxb-x", slack.OptionAPIURL(fs.srv.URL+"/"))
}

func TestPostStreamerInitialPost(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	if err := s.Append(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&fs.posts) != 1 {
		t.Fatalf("expected 1 post, got %d", fs.posts)
	}
	// Empty append is a no-op.
	if err := s.Append(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
}

func TestPostStreamerThrottlesAndFlushes(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	s.minInterval = 100 * time.Millisecond

	// First Append posts.
	if err := s.Append(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	// Immediate second Append: throttled, pending.
	if err := s.Append(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&fs.updates) != 0 {
		t.Fatal("update should be throttled")
	}
	// FlushIfPending without enough time has elapsed: no-op.
	if err := s.FlushIfPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&fs.updates) != 0 {
		t.Fatal("flush before interval should be no-op")
	}
	time.Sleep(120 * time.Millisecond)
	if err := s.FlushIfPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&fs.updates) != 1 {
		t.Fatalf("expected one update, got %d", fs.updates)
	}
	// Close flushes a final suffix.
	if err := s.Close(context.Background(), "_done_"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&fs.updates) != 2 {
		t.Fatalf("expected 2 updates, got %d", fs.updates)
	}
	// Second close: no-op.
	if err := s.Close(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	// Append after close: no-op.
	if err := s.Append(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
}

func TestPostStreamerTruncatesBody(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	s.maxChars = 32
	long := strings.Repeat("a", 100)
	if err := s.Append(context.Background(), long); err != nil {
		t.Fatal(err)
	}
	// Body in the post payload should contain the truncation marker.
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.bodies) == 0 || !strings.Contains(fs.bodies[0], "truncated") {
		t.Fatalf("expected truncation marker; bodies=%q", fs.bodies)
	}
}

func TestPostStreamerEmptyBodyShowsThinking(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	// Close with no content and no suffix → flush emits "_thinking…_".
	if err := s.Close(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.bodies) == 0 || !strings.Contains(fs.bodies[0], "thinking") {
		t.Fatalf("expected thinking placeholder; bodies=%q", fs.bodies)
	}
}

func TestPostStreamerPostError(t *testing.T) {
	fs := newFakeSlackSrv()
	fs.postErr = true
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	if err := s.Append(context.Background(), "x"); err == nil {
		t.Fatal("expected post error")
	}
}

func TestPostStreamerUpdateError(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	// First append posts ok.
	if err := s.Append(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	// Now flip the server to reject updates and force a flush.
	fs.mu.Lock()
	fs.updateErr = true
	fs.mu.Unlock()
	s.minInterval = 0 // make the next Append flush immediately
	if err := s.Append(context.Background(), "b"); err == nil {
		t.Fatal("expected update error")
	}
}

func TestFlushIfPendingClosed(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	if err := s.Close(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushIfPending(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// silence unused import if a path is removed
var _ = errors.New
