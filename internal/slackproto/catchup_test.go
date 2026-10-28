package slackproto

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/kfet/slack-acp/internal/catchup"
	"github.com/kfet/slack-acp/internal/journal"
)

// histAPI serves one scripted catch-up: the conversations to list and
// each conversation's history. listGate, when set, is received from
// before every list call returns, so a test can act while a run is in
// flight; listed is signalled as each list call starts.
type histAPI struct {
	mu       sync.Mutex
	convs    []slack.Channel
	history  map[string][]slack.Message
	listErr  error
	listGate chan struct{}
	listed   chan struct{}
	lists    int
}

func (a *histAPI) GetConversationsForUserContext(context.Context, *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error) {
	a.mu.Lock()
	a.lists++
	a.mu.Unlock()
	if a.listed != nil {
		a.listed <- struct{}{}
	}
	if a.listGate != nil {
		<-a.listGate
	}
	return a.convs, "", a.listErr
}

func (a *histAPI) GetConversationHistoryContext(_ context.Context, p *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []slack.Message
	for _, m := range a.history[p.ChannelID] {
		if m.Timestamp > p.Oldest {
			out = append(out, m)
		}
	}
	return &slack.GetConversationHistoryResponse{Messages: out}, nil
}

func (a *histAPI) GetConversationRepliesContext(context.Context, *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	return nil, false, "", nil
}

func (a *histAPI) post(ch string, m slack.Message) {
	a.mu.Lock()
	a.history[ch] = append(a.history[ch], m)
	a.mu.Unlock()
}

func conv(id string, im bool) slack.Channel {
	c := slack.Channel{}
	c.ID, c.IsIM = id, im
	return c
}

func hmsg(ts, user, text string) slack.Message {
	m := slack.Message{}
	m.Timestamp, m.User, m.Text = ts, user, text
	return m
}

// batchHandler records live events and catch-up batches separately.
type batchHandler struct {
	stubHandler
	batches [][]Event
}

func (h *batchHandler) CatchUp(_ context.Context, evs []Event) {
	h.mu.Lock()
	h.batches = append(h.batches, evs)
	h.mu.Unlock()
}

func (h *batchHandler) seenBatches() [][]Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([][]Event(nil), h.batches...)
}

const floorTS = "1700000000.000000"

// seededTS is the checkpoint of a channel known from an earlier run.
// A channel with none is new, and the catch-up does not read it.
const seededTS = "1700000000.500000"

func newCatchUpClient(t *testing.T, h Handler, api catchup.API, opts ...Option) (*Client, *catchup.Store) {
	t.Helper()
	store, err := catchup.Open(filepath.Join(t.TempDir(), "catchup.json"), time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range []string{"C1", "C2"} {
		if err := store.Advance(ch, seededTS); err != nil {
			t.Fatal(err)
		}
	}
	opts = append(opts, WithCatchUp(catchup.Config{API: api, Store: store}))
	c, err := New("xoxb-x", "xapp-x", h, opts...)
	if err != nil {
		t.Fatal(err)
	}
	c.botUserID = "Ubot"
	return c, store
}

func connect(c *Client) {
	c.dispatch(context.Background(), socketmode.Event{Type: socketmode.EventTypeConnected})
}

func TestCatchUpRoutesHistoryThroughLiveGates(t *testing.T) {
	records := captureJournal(t)
	edited := hmsg("1700000004.000000", "U1", "<@Ubot> edited later")
	edited.Edited = &slack.Edited{User: "U1", Timestamp: "1700000009.000000"}
	parent := hmsg("1700000002.000000", "U1", "top-level chatter")
	parent.ThreadTimestamp = parent.Timestamp // a thread parent, as history shows it
	reply := hmsg("1700000003.000000", "U2", "ambient reply")
	reply.ThreadTimestamp = "1700000002.000000"
	ourBot := hmsg("1700000005.000000", "Ubot", "our own answer")
	join := hmsg("1700000006.000000", "U3", "joined")
	join.SubType = "channel_join"
	api := &histAPI{
		convs: []slack.Channel{conv("C1", false), conv("D1", true), conv("C2", false)},
		history: map[string][]slack.Message{
			"C1": {
				hmsg("1700000001.000000", "U1", "<@Ubot> are you there?"),
				parent, reply, edited, ourBot, join,
				hmsg("1699999999.000000", "U1", "<@Ubot> before the floor"),
			},
			"D1": {hmsg("1700000007.000000", "U1", "dm while away")},
		},
	}
	h := &batchHandler{}
	c, store := newCatchUpClient(t, h, api)
	connect(c)
	c.WaitCatchUp()

	b := h.seenBatches()
	if len(b) != 2 {
		t.Fatalf("batches = %+v", b)
	}
	c1, d1 := b[0], b[1]
	if len(c1) != 3 {
		t.Fatalf("C1 batch = %+v", c1)
	}
	if e := c1[0]; !e.IsMention || !e.CatchUp || e.Text != "are you there?" || e.ThreadTS != e.TS || e.BotUserID != "Ubot" {
		t.Fatalf("mention = %+v", e)
	}
	if e := c1[1]; e.IsMention || e.ThreadTS != "1700000002.000000" || e.Text != "ambient reply" {
		t.Fatalf("ambient reply = %+v", e)
	}
	if e := c1[2]; !e.IsMention || e.TS != "1700000004.000000" {
		t.Fatalf("edited mention = %+v", e)
	}
	if e := d1[0]; !e.IsDM || !e.CatchUp || e.Text != "dm while away" {
		t.Fatalf("dm = %+v", e)
	}
	if len(h.seen()) != 0 {
		t.Fatal("catch-up events must not arrive through Handle")
	}
	if store.Mark("C1") != "1700000006.000000" || store.Mark("D1") != "1700000007.000000" || store.Mark("C2") != seededTS {
		t.Fatalf("marks C1=%s D1=%s C2=%s", store.Mark("C1"), store.Mark("D1"), store.Mark("C2"))
	}

	reasons := map[string]string{}
	for _, r := range records() {
		if !r.CatchUp {
			t.Fatalf("catch-up record without the flag: %+v", r)
		}
		reasons[r.TS] = r.Reason
	}
	for ts, want := range map[string]string{
		"1700000002.000000": journal.ReasonNotThreadReply,
		"1700000005.000000": journal.ReasonSelfDriveNotAccept,
		"1700000006.000000": journal.ReasonSubType,
		"1700000003.000000": journal.ReasonAmbientThreadReply,
	} {
		if reasons[ts] != want {
			t.Fatalf("ts %s: reason %q, want %q (all %v)", ts, reasons[ts], want, reasons)
		}
	}
	if _, ok := reasons["1699999999.000000"]; ok {
		t.Fatal("a message from before the floor must not be read")
	}

	// A second connect finds nothing new: exactly once.
	connect(c)
	c.WaitCatchUp()
	if len(h.seenBatches()) != 2 {
		t.Fatalf("replayed: %+v", h.seenBatches())
	}
}

// TestCatchUpTakesAppIDFromBotProfile: history carries the posting app
// on bot_profile, and the human-author reclassification needs it.
func TestCatchUpTakesAppIDFromBotProfile(t *testing.T) {
	m := hmsg("1700000001.000000", "Uhuman", "<@Ubot> via our app")
	m.BotID = "B0USERPOST1"
	m.BotProfile = &slack.BotProfile{AppID: ourAppID}
	api := &histAPI{convs: []slack.Channel{conv("C1", false)}, history: map[string][]slack.Message{"C1": {m}}}
	h := &batchHandler{}
	c, _ := newCatchUpClient(t, h, api, WithHumanAuthors(map[string]struct{}{"Uhuman": {}}))
	armHumanAuthors(c)
	connect(c)
	c.WaitCatchUp()
	if b := h.seenBatches(); len(b) != 1 || b[0][0].UserID != "Uhuman" {
		t.Fatalf("batches = %+v", b)
	}
}

func mentionEvent(ts, text string) slackevents.EventsAPIEvent {
	return slackevents.EventsAPIEvent{
		Type: slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Data: &slackevents.AppMentionEvent{
			User: "U1", Channel: "C1", TimeStamp: ts, Text: text,
		}},
	}
}

// TestCatchUpHoldsLiveEventsAndDeliversExactlyOnce pins the reconnect
// race: events arriving while the run reads history are held, the ones
// the run already read are dropped, the rest delivered after it.
func TestCatchUpHoldsLiveEventsAndDeliversExactlyOnce(t *testing.T) {
	records := captureJournal(t)
	api := &histAPI{
		convs:    []slack.Channel{conv("C1", false)},
		history:  map[string][]slack.Message{"C1": {hmsg("1700000001.000000", "U1", "<@Ubot> missed")}},
		listGate: make(chan struct{}),
		listed:   make(chan struct{}),
	}
	h := &batchHandler{}
	c, store := newCatchUpClient(t, h, api)
	ctx := context.Background()

	connect(c)
	<-api.listed // the run is in flight
	// Posted after the socket came back: Slack delivers it live AND the
	// run will read it from history.
	api.post("C1", hmsg("1700000002.000000", "U1", "<@Ubot> around reconnect"))
	c.handleEventsAPI(ctx, mentionEvent("1700000002.000000", "<@Ubot> around reconnect"), "")
	// Only live: posted after the run's read.
	c.handleEventsAPI(ctx, mentionEvent("1700000003.000000", "<@Ubot> after"), "")
	if len(h.seen()) != 0 {
		t.Fatal("live events must be held while a run is in flight")
	}
	// A reconnect during the run asks for another run after it.
	connect(c)
	api.listGate <- struct{}{} // first run reads
	<-api.listed               // second run starts only after the held events are released
	if got := h.seen(); len(got) != 1 || got[0].TS != "1700000003.000000" {
		t.Fatalf("released live events = %+v", got)
	}
	api.listGate <- struct{}{}
	c.WaitCatchUp()

	b := h.seenBatches()
	if len(b) != 1 || len(b[0]) != 2 || b[0][0].TS != "1700000001.000000" || b[0][1].TS != "1700000002.000000" {
		t.Fatalf("batches = %+v", b)
	}
	if store.Mark("C1") != "1700000003.000000" {
		t.Fatalf("mark = %s", store.Mark("C1"))
	}
	var seen int
	for _, r := range records() {
		if r.Reason == journal.ReasonCatchUpSeen {
			seen++
			if r.TS != "1700000002.000000" || r.Path != journal.PathAppMention || r.Stage != journal.StageProto {
				t.Fatalf("seen record = %+v", r)
			}
		}
	}
	if seen != 1 {
		t.Fatalf("catch_up_seen records = %d", seen)
	}

	// After the runs, live delivery is direct and advances the mark.
	c.handleEventsAPI(ctx, mentionEvent("1700000004.000000", "<@Ubot> live"), "")
	if len(h.seen()) != 2 || store.Mark("C1") != "1700000004.000000" {
		t.Fatalf("seen=%+v mark=%s", h.seen(), store.Mark("C1"))
	}
}

// TestCatchUpReleasesHeldEventsInOrder: two held events in one
// conversation, out of ts order, are both delivered — releasing the
// newer must not make the older look already read.
func TestCatchUpReleasesHeldEventsInOrder(t *testing.T) {
	api := &histAPI{
		convs:    []slack.Channel{conv("C1", false)},
		history:  map[string][]slack.Message{},
		listGate: make(chan struct{}),
		listed:   make(chan struct{}, 1),
	}
	h := &stubHandler{}
	c, _ := newCatchUpClient(t, h, api)
	connect(c)
	<-api.listed
	c.handleEventsAPI(context.Background(), mentionEvent("1700000009.000000", "<@Ubot> b"), "")
	c.handleEventsAPI(context.Background(), mentionEvent("1700000008.000000", "<@Ubot> a"), "")
	api.listGate <- struct{}{}
	c.WaitCatchUp()
	if got := h.seen(); len(got) != 2 {
		t.Fatalf("seen = %+v", got)
	}
}

func TestCatchUpWithoutBatchHandlerUsesHandle(t *testing.T) {
	api := &histAPI{
		convs:   []slack.Channel{conv("D1", true)},
		history: map[string][]slack.Message{"D1": {hmsg("1700000001.000000", "U1", "one"), hmsg("1700000002.000000", "U1", "two")}},
	}
	h := &stubHandler{}
	c, _ := newCatchUpClient(t, h, api)
	connect(c)
	c.WaitCatchUp()
	if got := h.seen(); len(got) != 2 || !got[0].CatchUp || got[1].Text != "two" {
		t.Fatalf("seen = %+v", got)
	}
}

func TestCatchUpNilHandler(t *testing.T) {
	api := &histAPI{
		convs:   []slack.Channel{conv("D1", true)},
		history: map[string][]slack.Message{"D1": {hmsg("1700000001.000000", "U1", "one")}},
	}
	c, store := newCatchUpClient(t, nil, api)
	connect(c)
	c.WaitCatchUp()
	if store.Mark("D1") != "1700000001.000000" {
		t.Fatal("the checkpoint still moves with no handler")
	}
}

func TestCatchUpListErrorEndsTheRun(t *testing.T) {
	api := &histAPI{listErr: errors.New("invalid_auth")}
	h := &stubHandler{}
	c, _ := newCatchUpClient(t, h, api)
	connect(c)
	c.WaitCatchUp()
	c.handleEventsAPI(context.Background(), mentionEvent("1700000001.000000", "<@Ubot> hi"), "")
	if len(h.seen()) != 1 {
		t.Fatal("live delivery must resume after a failed run")
	}
}

func TestCatchUpCheckpointWriteFailureIsLogged(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catchup.json")
	store, err := catchup.Open(path, time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	h := &stubHandler{}
	c, err := New("xoxb-x", "xapp-x", h, WithCatchUp(catchup.Config{API: &histAPI{}, Store: store}))
	if err != nil {
		t.Fatal(err)
	}
	c.botUserID = "Ubot"
	c.handleEventsAPI(context.Background(), mentionEvent("1700000001.000000", "<@Ubot> hi"), "")
	if len(h.seen()) != 1 || store.Mark("C1") != "1700000001.000000" {
		t.Fatalf("delivery must not depend on persisting the checkpoint: %+v", h.seen())
	}
}

func TestWithCatchUpDefaultsToTheClientAPI(t *testing.T) {
	store, err := catchup.Open(filepath.Join(t.TempDir(), "c.json"), time.Unix(1700000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	c, err := New("xoxb-x", "xapp-x", nil, WithCatchUp(catchup.Config{Store: store}))
	if err != nil {
		t.Fatal(err)
	}
	if c.catchUp == nil || c.catchUp.scanner.Store() != store {
		t.Fatal("catch-up not configured")
	}
	// Without the option there is nothing to wait for.
	c2 := newClientForDispatch(t, nil)
	c2.WaitCatchUp()
	c2.startCatchUp(context.Background())
}

func TestEventJournalPath(t *testing.T) {
	for want, ev := range map[journal.Path]Event{
		journal.PathSelfDrive:  {SelfDrive: true, IsDM: true},
		journal.PathMessageIM:  {IsDM: true},
		journal.PathAppMention: {IsMention: true},
		journal.PathMessage:    {},
	} {
		if got := ev.JournalPath(); got != want {
			t.Fatalf("%+v: %s, want %s", ev, got, want)
		}
	}
}

// TestCatchUpKeepsFiles: a file_share read from history is an original
// post, admitted with its files — as it would have been live.
func TestCatchUpKeepsFiles(t *testing.T) {
	m := hmsg("1700000001.000000", "U1", "")
	m.SubType = subtypeFileShare
	m.Files = []slack.File{
		{ID: "F1", Name: "a.txt", URLPrivateDownload: "https://files.slack.com/a.txt"},
		{ID: "F2", Name: "evil", URLPrivate: "https://example.com/evil"},
	}
	api := &histAPI{convs: []slack.Channel{conv("D1", true)}, history: map[string][]slack.Message{"D1": {m}}}
	h := &batchHandler{}
	c, _ := newCatchUpClient(t, h, api)
	connect(c)
	c.WaitCatchUp()
	b := h.seenBatches()
	if len(b) != 1 || len(b[0][0].Files) != 1 || b[0][0].Files[0].ID != "F1" {
		t.Fatalf("batches = %+v", b)
	}
}

// TestCatchUpObservesEveryLiveMessage: a live message advances its
// channel's checkpoint even when it is dropped — it never needs reading
// again — but not while a run is in flight, whose read it would skip.
func TestCatchUpObservesEveryLiveMessage(t *testing.T) {
	api := &histAPI{
		convs:    []slack.Channel{conv("C1", false)},
		history:  map[string][]slack.Message{},
		listGate: make(chan struct{}),
		listed:   make(chan struct{}, 1),
	}
	c, store := newCatchUpClient(t, &stubHandler{}, api)
	chatter := func(ts string) slackevents.EventsAPIEvent {
		return slackevents.EventsAPIEvent{Type: slackevents.CallbackEvent, InnerEvent: slackevents.EventsAPIInnerEvent{
			Data: &slackevents.MessageEvent{User: "U1", Channel: "C1", TimeStamp: ts, Text: "top-level chatter"}}}
	}
	c.handleEventsAPI(context.Background(), chatter("1700000005.000000"), "")
	if store.Mark("C1") != "1700000005.000000" {
		t.Fatalf("dropped live message did not advance: %s", store.Mark("C1"))
	}
	connect(c)
	<-api.listed
	c.handleEventsAPI(context.Background(), chatter("1700000006.000000"), "")
	if store.Mark("C1") != "1700000005.000000" {
		t.Fatalf("advanced during a run: %s", store.Mark("C1"))
	}
	api.listGate <- struct{}{}
	c.WaitCatchUp()
}
