package slackproto

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
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

func TestNewHonoursSlackAPIBaseEnv(t *testing.T) {
	// Start a tiny HTTP server and assert that auth.test goes to it
	// when SLACK_API_BASE points at it. This proves the override is
	// actually wired through to the slack.Client, not just that the
	// branch is taken.
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"user_id":"UBOT","user":"bot","team_id":"T0"}`))
	}))
	defer srv.Close()

	t.Setenv("SLACK_API_BASE", srv.URL+"/api/")
	c, err := New("xoxb-x", "xapp-x", &stubHandler{})
	if err != nil {
		t.Fatalf("New with SLACK_API_BASE: %v", err)
	}
	if _, err := c.API().AuthTestContext(context.Background()); err != nil {
		t.Fatalf("AuthTest against override: %v", err)
	}
	if got := hits.Load(); got == 0 {
		t.Fatalf("override URL not hit (hits=%d)", got)
	}

	// Unset path: env var empty → no override option, slack.Client
	// uses its default URL. We just exercise the branch (and confirm
	// construction still succeeds).
	t.Setenv("SLACK_API_BASE", "")
	if _, err := New("xoxb-x", "xapp-x", &stubHandler{}); err != nil {
		t.Fatalf("New without SLACK_API_BASE: %v", err)
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
	ch := make(chan socketmode.Event)
	go func() { c.consume(ctx, ch); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("consume did not exit on ctx cancel")
	}
}

// TestConsumeChannelClose covers the !ok branch when the events channel
// is closed by the upstream socketmode client.
func TestConsumeChannelClose(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	ch := make(chan socketmode.Event)
	close(ch)
	done := make(chan struct{})
	go func() { c.consume(context.Background(), ch); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("consume did not exit on channel close")
	}
}

// TestConsumeDispatchesEvents drives the dispatch path of consume by
// pushing an event onto the channel and asserting it surfaces.
func TestConsumeDispatchesEvents(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	ch := make(chan socketmode.Event, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { c.consume(ctx, ch); close(done) }()
	ch <- socketmode.Event{Type: socketmode.EventTypeHello}
	close(ch) // consume returns after draining
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("consume did not return after channel close")
	}
	// No assertion beyond "doesn't panic / dispatch fires" — the Hello
	// branch is debug-logged and produces no handler call.
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
	// updateGate, if non-nil, blocks each chat.update handler until
	// the channel is closed. Used to make the spinner/Append race
	// deterministic in TestPostStreamerSendMuSerializes.
	updateGate chan struct{}
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
		fs.mu.Lock()
		gate := fs.updateGate
		fs.mu.Unlock()
		if gate != nil {
			<-gate
		}
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
	// Inject a fake clock so the test doesn't sleep.
	var clock atomic.Int64 // ns since epoch
	clock.Store(time.Now().UnixNano())
	s.now = func() time.Time { return time.Unix(0, clock.Load()) }

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
	// Advance the clock past minInterval; flush should now fire.
	clock.Add(int64(120 * time.Millisecond))
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

func TestPostStreamerStartPostsImmediately(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	if err := s.Start(context.Background(), "> _Thinking…_"); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	posts := fs.posts
	first := ""
	if len(fs.bodies) > 0 {
		first = fs.bodies[0]
	}
	fs.mu.Unlock()
	if posts != 1 || !strings.Contains(first, "Thinking") {
		t.Fatalf("expected 1 post with thinking placeholder; posts=%d body=%q", posts, first)
	}
}

func TestPostStreamerStartDefaultBody(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	if err := s.Start(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	first := fs.bodies[0]
	fs.mu.Unlock()
	if !strings.Contains(first, "thinking") {
		t.Fatalf("expected default placeholder; body=%q", first)
	}
}

func TestPostStreamerStartIdempotent(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	if err := s.Start(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background(), "hi again"); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	posts := fs.posts
	fs.mu.Unlock()
	if posts != 1 {
		t.Fatalf("Start must be idempotent; posts=%d", posts)
	}
}

func TestPostStreamerStartThenAppendOverwrites(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	if err := s.Start(context.Background(), "> _Thinking…_"); err != nil {
		t.Fatal(err)
	}
	// First Append after Start must flush as an update (lastSent is
	// zero), so the user sees real content within milliseconds rather
	// than waiting for the throttle.
	if err := s.Append(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.posts != 1 || fs.updates != 1 {
		t.Fatalf("want 1 post + 1 update; got posts=%d updates=%d", fs.posts, fs.updates)
	}
	// The second body (the update) carries the real content, not the
	// placeholder.
	if !strings.Contains(fs.bodies[1], "hello") || strings.Contains(fs.bodies[1], "Thinking") {
		t.Fatalf("update body should replace placeholder; got %q", fs.bodies[1])
	}
}

func TestPostStreamerStartClosedNoOp(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	if err := s.Close(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	// Close already posted once (the Closing flush). Start on a
	// closed streamer must not post a second time.
	if err := s.Start(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.posts != 1 {
		t.Fatalf("Start on closed must be no-op; posts=%d", fs.posts)
	}
}

func TestPostStreamerSetMinInterval(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	s.SetMinInterval(0)
	if s.minInterval != 0 {
		t.Fatalf("SetMinInterval did not stick; got %v", s.minInterval)
	}
}

func TestPostStreamerStartPostError(t *testing.T) {
	fs := newFakeSlackSrv()
	fs.postErr = true
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	if err := s.Start(context.Background(), "x"); err == nil {
		t.Fatal("expected post error from Start")
	}
}

func TestPostStreamerUpdatePlaceholderNotStarted(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	// No Start → s.ts empty → not alive, no IO.
	alive, err := s.UpdatePlaceholder(context.Background(), "x")
	if alive || err != nil {
		t.Fatalf("want !alive nil err; got alive=%v err=%v", alive, err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.updates != 0 {
		t.Fatalf("must not call chat.update before Start; updates=%d", fs.updates)
	}
}

func TestPostStreamerUpdatePlaceholderHappy(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	s.minInterval = 0 // bypass throttle
	if err := s.Start(context.Background(), ">_T_"); err != nil {
		t.Fatal(err)
	}
	alive, err := s.UpdatePlaceholder(context.Background(), ">_T._")
	if !alive || err != nil {
		t.Fatalf("want alive nil err; got alive=%v err=%v", alive, err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.updates != 1 {
		t.Fatalf("want 1 update; got %d", fs.updates)
	}
}

func TestPostStreamerUpdatePlaceholderThrottled(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	// Stub clock at a fixed instant so throttle math is deterministic.
	now := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return now }
	if err := s.Start(context.Background(), ">_T_"); err != nil {
		t.Fatal(err)
	}
	// First placeholder tick goes through (lastSent is zero).
	if alive, err := s.UpdatePlaceholder(context.Background(), ">_T._"); !alive || err != nil {
		t.Fatalf("first tick: alive=%v err=%v", alive, err)
	}
	// Second tick is within minInterval → must skip without IO but
	// remain alive.
	if alive, err := s.UpdatePlaceholder(context.Background(), ">_T.._"); !alive || err != nil {
		t.Fatalf("throttled tick should stay alive nil err; got alive=%v err=%v", alive, err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.updates != 1 {
		t.Fatalf("throttled tick should not hit network; updates=%d", fs.updates)
	}
}

func TestPostStreamerUpdatePlaceholderClosedNotAlive(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	if err := s.Start(context.Background(), ">_T_"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if alive, err := s.UpdatePlaceholder(context.Background(), "x"); alive || err != nil {
		t.Fatalf("closed streamer: alive=%v err=%v", alive, err)
	}
}

func TestPostStreamerUpdatePlaceholderAfterFirstChunkNotAlive(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	if err := s.Start(context.Background(), ">_T_"); err != nil {
		t.Fatal(err)
	}
	s.FirstChunk()
	if alive, _ := s.UpdatePlaceholder(context.Background(), "x"); alive {
		t.Fatal("must not be alive after FirstChunk")
	}
}

func TestPostStreamerUpdatePlaceholderError(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	s.minInterval = 0
	if err := s.Start(context.Background(), ">_T_"); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	fs.updateErr = true
	fs.mu.Unlock()
	alive, err := s.UpdatePlaceholder(context.Background(), "x")
	if !alive || err == nil {
		// alive stays true so the caller doesn't disarm on a
		// transient Slack hiccup — but the error is surfaced.
		t.Fatalf("want alive + err; got alive=%v err=%v", alive, err)
	}
}

func TestPostStreamerFirstChunkResetsThrottle(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	if err := s.Start(context.Background(), ">_T_"); err != nil {
		t.Fatal(err)
	}
	// Simulate a placeholder tick having just happened.
	s.mu.Lock()
	s.lastSent = s.now()
	s.mu.Unlock()
	s.FirstChunk()
	s.mu.Lock()
	zeroed := s.lastSent.IsZero()
	done := s.placeholderDone
	s.mu.Unlock()
	if !zeroed || !done {
		t.Fatalf("FirstChunk must reset lastSent + set placeholderDone; zeroed=%v done=%v", zeroed, done)
	}
	// Idempotent: a second call is a no-op (covers the early-return branch).
	s.FirstChunk()
}

// TestPostStreamerSendMuSerializes pins the fix for the spinner-vs-Append
// race: a slow in-flight UpdatePlaceholder must NOT clobber real content
// that lands while it's blocked on Slack. The Slack chat.update sender
// is serialized through sendMu and re-checks placeholderDone after
// acquiring it; the second (placeholder) request must therefore find
// placeholderDone=true and bail without touching the wire.
//
// Sequencing (all deterministic — no wall-clock waits):
//
//  1. Start posts the placeholder (1 chat.postMessage).
//  2. Goroutine A calls UpdatePlaceholder("thinking") — server's
//     update handler blocks on the gate; sendMu is held.
//  3. Goroutine B calls FirstChunk (closes the window) + Append("real")
//     — Append's flush waits on sendMu.
//  4. Release the gate. The in-flight placeholder update completes:
//     1 chat.update with "thinking".
//  5. sendMu is released; Append's flush acquires it and posts the
//     real content: 1 more chat.update with "real".
//  6. We verify the LAST body on the wire is the real content, not
//     the placeholder. If a second (post-FirstChunk) UpdatePlaceholder
//     races in, it must bail under sendMu — no third update.
func TestPostStreamerSendMuSerializes(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	s.minInterval = 0 // bypass throttle for placeholder/append
	if err := s.Start(context.Background(), "> _Thinking…_"); err != nil {
		t.Fatal(err)
	}

	gate := make(chan struct{})
	fs.mu.Lock()
	fs.updateGate = gate
	fs.mu.Unlock()

	upDone := make(chan struct{})
	go func() {
		defer close(upDone)
		_, _ = s.UpdatePlaceholder(context.Background(), "> _Thinking._")
	}()

	// Wait until the in-flight UpdatePlaceholder has actually entered
	// sendMu (i.e. is blocked on the gated server). We can't observe
	// the mutex directly, so we observe its effect: a TryLock on
	// sendMu fails iff someone holds it.
	for {
		if !s.sendMu.TryLock() {
			break
		}
		s.sendMu.Unlock()
		// Yield without sleeping; the goroutine above will get on cpu.
		// (No time.Sleep — tests must not wall-clock-poll.)
		runtimeGosched()
	}

	// Real content arrives behind the in-flight placeholder.
	appDone := make(chan struct{})
	go func() {
		defer close(appDone)
		s.FirstChunk()
		_ = s.Append(context.Background(), "real-answer")
	}()

	// Stop blocking the placeholder request. It completes (1 update),
	// sendMu released, Append's flush now acquires it and lands the
	// real content (2nd update).
	close(gate)

	<-upDone
	<-appDone

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if got := atomic.LoadInt32(&fs.updates); got != 2 {
		t.Fatalf("want exactly 2 chat.update calls (placeholder + real); got %d", got)
	}
	if len(fs.bodies) < 3 {
		t.Fatalf("want 3 bodies (post + 2 updates); got %d: %q", len(fs.bodies), fs.bodies)
	}
	last := fs.bodies[len(fs.bodies)-1]
	if !strings.Contains(last, "real-answer") {
		t.Fatalf("last body must carry real content (placeholder must not clobber it); got %q", last)
	}
}

// TestPostStreamerUpdatePlaceholderBailsAfterFirstChunk pins the
// "re-check placeholderDone under sendMu" branch: an UpdatePlaceholder
// that loses the sendMu race against a FirstChunk-er must NOT issue
// the chat.update.
func TestPostStreamerUpdatePlaceholderBailsAfterFirstChunk(t *testing.T) {
	fs := newFakeSlackSrv()
	defer fs.close()
	s := NewPostStreamer(fs.client(), "C1", "100.0")
	s.minInterval = 0
	if err := s.Start(context.Background(), "> _Thinking…_"); err != nil {
		t.Fatal(err)
	}
	// Hold sendMu so any UpdatePlaceholder must wait on it.
	s.sendMu.Lock()
	upDone := make(chan struct{})
	go func() {
		defer close(upDone)
		alive, err := s.UpdatePlaceholder(context.Background(), "> _Thinking._")
		if alive || err != nil {
			t.Errorf("expected !alive nil err after FirstChunk; got alive=%v err=%v", alive, err)
		}
	}()
	// Flip placeholderDone while UpdatePlaceholder is parked on sendMu.
	s.FirstChunk()
	// Release sendMu → UpdatePlaceholder re-checks placeholderDone and bails.
	s.sendMu.Unlock()
	<-upDone

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if got := atomic.LoadInt32(&fs.updates); got != 0 {
		t.Fatalf("UpdatePlaceholder must bail without IO once FirstChunk fired; updates=%d", got)
	}
}

// runtimeGosched yields the current goroutine without a wall-clock
// sleep. Used in TestPostStreamerSendMuSerializes to spin until
// another goroutine has acquired the streamer's sendMu.
func runtimeGosched() { runtime.Gosched() }
