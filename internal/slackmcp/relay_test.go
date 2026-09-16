package slackmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/kfet/slack-acp/internal/slackproto"
)

// renderText extracts the message body from the MsgOptions the relay
// built, so tests can assert on what would actually reach Slack.
func renderText(options []slack.MsgOption) string {
	_, values, err := slack.UnsafeApplyMsgOptions("tok", "C9", "https://slack.test/", options...)
	if err != nil {
		return ""
	}
	return values.Get("text")
}

// fakeAPI stands in for *slack.Client. Every field is a canned answer,
// so no test depends on the network or on timing.
type fakeAPI struct {
	replies     []slack.Message
	repliesByTS map[string][]slack.Message
	replyCalls  []string
	repliesErr  error

	history          []slack.Message
	historyByChannel map[string][]slack.Message
	historyCalls     []string
	historyOldest    string
	historyErr       error

	channels      []slack.Channel
	channelsErr   error
	channelCursor string
	users         map[string]*slack.User
	userErr       error
	userCalls     int

	postErr     error
	postTS      string
	postChannel string
	postText    string
	postOpts    int
	postCalls   int
}

func (f *fakeAPI) GetConversationRepliesContext(_ context.Context, p *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	if p != nil {
		f.replyCalls = append(f.replyCalls, p.ChannelID+"@"+p.Timestamp)
		if f.repliesByTS != nil {
			return f.repliesByTS[p.Timestamp], false, "", f.repliesErr
		}
	}
	return f.replies, false, "", f.repliesErr
}

func (f *fakeAPI) GetConversationHistoryContext(_ context.Context, p *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	if f.historyErr != nil {
		return nil, f.historyErr
	}
	if p != nil {
		f.historyCalls = append(f.historyCalls, p.ChannelID)
		f.historyOldest = p.Oldest
		if f.historyByChannel != nil {
			return &slack.GetConversationHistoryResponse{Messages: f.historyByChannel[p.ChannelID]}, nil
		}
	}
	return &slack.GetConversationHistoryResponse{Messages: f.history}, nil
}

func (f *fakeAPI) GetConversationsForUserContext(context.Context, *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error) {
	return f.channels, f.channelCursor, f.channelsErr
}

func (f *fakeAPI) PostMessageContext(_ context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	f.postCalls++
	f.postChannel, f.postOpts = channelID, len(options)
	f.postText = renderText(options)
	if f.postErr != nil {
		return "", "", f.postErr
	}
	return channelID, f.postTS, nil
}

func (f *fakeAPI) GetUserInfoContext(_ context.Context, user string) (*slack.User, error) {
	f.userCalls++
	if f.userErr != nil {
		return nil, f.userErr
	}
	return f.users[user], nil
}

// capture records the tool-call log lines so tests can assert on the
// audit trail, which is a requirement in its own right.
type capture struct{ lines []string }

func (c *capture) logf(format string, v ...any) {
	c.lines = append(c.lines, strings.TrimSpace(fmt.Sprintf(format, v...)))
}

func (c *capture) joined() string { return strings.Join(c.lines, "\n") }

func newRelay(t *testing.T, api API, cfg RelayConfig) (*Relay, *capture) {
	t.Helper()
	cap := &capture{}
	cfg.API = api
	cfg.Logf = cap.logf
	return NewRelay(cfg), cap
}

func msgs(t *testing.T, s string) []message {
	t.Helper()
	var out []message
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("decode result %q: %v", s, err)
	}
	return out
}

func chans(t *testing.T, s string) []channelInfo {
	t.Helper()
	return channelListOut(t, s).Channels
}

func channelListOut(t *testing.T, s string) channelList {
	t.Helper()
	var out channelList
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("decode result %q: %v", s, err)
	}
	return out
}

func mkMsg(ts, user, text string) slack.Message {
	var m slack.Message
	m.Timestamp, m.User, m.Text = ts, user, text
	return m
}

func TestReadThreadRendersAndResolvesNames(t *testing.T) {
	api := &fakeAPI{
		replies: []slack.Message{
			mkMsg("1.0", "U1", "hello"),
			mkMsg("1.1", "U1", "  "), // blank: dropped
			mkMsg("1.2", "U2", "world"),
		},
		users: map[string]*slack.User{
			"U1": {Profile: slack.UserProfile{DisplayName: "ada"}},
			"U2": {RealName: "Grace Hopper"},
		},
	}
	r, cap := newRelay(t, api, RelayConfig{})
	out, err := r.ReadThread(context.Background(), "C1/9.9", "C9", "1.0", 5)
	if err != nil {
		t.Fatal(err)
	}
	got := msgs(t, out)
	if len(got) != 2 || got[0].User != "ada" || got[1].User != "Grace Hopper" {
		t.Fatalf("rendered = %+v", got)
	}
	if !strings.Contains(cap.joined(), "tool="+ToolReadThread) ||
		!strings.Contains(cap.joined(), "session=C1/9.9") ||
		!strings.Contains(cap.joined(), "channel=C9") ||
		!strings.Contains(cap.joined(), "outcome=ok") {
		t.Fatalf("log line missing required fields: %q", cap.joined())
	}
}

// User lookups are cached: a busy thread must not fan out one users.info
// call per message.
func TestUserNamesCached(t *testing.T) {
	api := &fakeAPI{
		replies: []slack.Message{mkMsg("1.0", "U1", "a"), mkMsg("1.1", "U1", "b")},
		users:   map[string]*slack.User{"U1": {Name: "ada"}},
	}
	r, _ := newRelay(t, api, RelayConfig{})
	if _, err := r.ReadThread(context.Background(), "k", "C9", "1.0", 0); err != nil {
		t.Fatal(err)
	}
	if api.userCalls != 1 {
		t.Fatalf("users.info called %d times, want 1 (cached)", api.userCalls)
	}
}

// Unresolvable or absent authors must still render something.
func TestUserNameFallbacks(t *testing.T) {
	bot := slack.Message{}
	bot.Timestamp, bot.Username, bot.Text = "1.3", "webhook", "beep"
	anon := slack.Message{}
	anon.Timestamp, anon.Text = "1.4", "ghost"

	api := &fakeAPI{
		replies: []slack.Message{
			mkMsg("1.0", "U1", "a"), // lookup errors → raw id
			mkMsg("1.1", "U2", "b"), // nil user → raw id
			bot,                     // bot: username
			anon,                    // neither: "unknown"
		},
		users:   map[string]*slack.User{"U2": nil},
		userErr: nil,
	}
	// U1 resolves to a user with an entirely empty profile.
	api.users["U1"] = &slack.User{}
	r, _ := newRelay(t, api, RelayConfig{})
	out, err := r.ReadThread(context.Background(), "k", "C9", "1.0", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := msgs(t, out)
	want := []string{"U1", "U2", "webhook", "unknown"}
	for i, w := range want {
		if got[i].User != w {
			t.Errorf("message %d user = %q, want %q", i, got[i].User, w)
		}
	}
}

func TestUserLookupErrorFallsBackToID(t *testing.T) {
	api := &fakeAPI{
		replies: []slack.Message{mkMsg("1.0", "U1", "a")},
		userErr: errors.New("rate limited"),
	}
	r, _ := newRelay(t, api, RelayConfig{})
	out, err := r.ReadThread(context.Background(), "k", "C9", "1.0", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := msgs(t, out); got[0].User != "U1" {
		t.Fatalf("user = %q, want the raw id", got[0].User)
	}
}

func TestReadChannel(t *testing.T) {
	api := &fakeAPI{history: []slack.Message{mkMsg("2.0", "U1", "x")}, users: map[string]*slack.User{"U1": {Name: "ada"}}}
	r, cap := newRelay(t, api, RelayConfig{})
	out, err := r.ReadChannel(context.Background(), "k", "C9", 500, "1.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := msgs(t, out); len(got) != 1 || got[0].Text != "x" {
		t.Fatalf("rendered = %+v", got)
	}
	if !strings.Contains(cap.joined(), "tool="+ToolReadChannel) {
		t.Fatalf("log = %q", cap.joined())
	}
}

func TestListChannelsFiltersByAllowlist(t *testing.T) {
	api := &fakeAPI{channels: []slack.Channel{
		mkChan("C1", "general", false),
		mkChan("C2", "secret", true),
	}}
	r, _ := newRelay(t, api, RelayConfig{AllowedChannelIDs: map[string]struct{}{"C1": {}}})
	out, err := r.ListChannels(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	got := chans(t, out)
	if len(got) != 1 || got[0].ID != "C1" || got[0].Name != "general" {
		t.Fatalf("listed = %+v — a disallowed channel's id must not leak", got)
	}
}

func TestListChannelsUnfiltered(t *testing.T) {
	api := &fakeAPI{channels: []slack.Channel{mkChan("C1", "general", false), mkChan("C2", "secret", true)}}
	r, _ := newRelay(t, api, RelayConfig{})
	out, err := r.ListChannels(context.Background(), "k")
	if err != nil {
		t.Fatal(err)
	}
	if got := chans(t, out); len(got) != 2 {
		t.Fatalf("listed = %+v", got)
	}
	// is_private is deliberately not reported: it is a targeting oracle
	// and the agent never branches on it.
	if strings.Contains(out, "is_private") {
		t.Fatalf("channel listing leaks is_private: %s", out)
	}
}

func mkChan(id, name string, private bool) slack.Channel {
	var c slack.Channel
	c.ID, c.Name, c.IsPrivate = id, name, private
	return c
}

// The allowlist is enforced on EVERY tool, and the denial names the
// channel so the agent can report something actionable.
func TestAllowlistDeniesEveryTool(t *testing.T) {
	api := &fakeAPI{}
	r, cap := newRelay(t, api, RelayConfig{AllowedChannelIDs: map[string]struct{}{"Cok": {}}})
	ctx := context.Background()

	calls := map[string]func() (string, error){
		ToolReadThread:  func() (string, error) { return r.ReadThread(ctx, "k", "Cbad", "1.0", 0) },
		ToolReadChannel: func() (string, error) { return r.ReadChannel(ctx, "k", "Cbad", 0, "") },
		ToolPost:        func() (string, error) { return r.Post(ctx, "k", "Cbad", "", "hi") },
	}
	for name, call := range calls {
		_, err := call()
		if err == nil {
			t.Fatalf("%s: allowed a denied channel", name)
		}
		if !strings.Contains(err.Error(), "Cbad") {
			t.Errorf("%s: error %q does not name the channel", name, err)
		}
	}
	if api.postCalls != 0 {
		t.Fatal("a denied post still reached Slack")
	}
	if n := strings.Count(cap.joined(), "outcome=denied"); n != 3 {
		t.Fatalf("denials logged %d times, want 3: %q", n, cap.joined())
	}
}

func TestAPIErrorsSurfaceAndLog(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")
	for _, tc := range []struct {
		name string
		api  *fakeAPI
		call func(*Relay) (string, error)
		want string
	}{
		{"replies", &fakeAPI{repliesErr: boom}, func(r *Relay) (string, error) { return r.ReadThread(ctx, "k", "C9", "1.0", 0) }, "conversations.replies"},
		{"history", &fakeAPI{historyErr: boom}, func(r *Relay) (string, error) { return r.ReadChannel(ctx, "k", "C9", 0, "") }, "conversations.history"},
		{"channels", &fakeAPI{channelsErr: boom}, func(r *Relay) (string, error) { return r.ListChannels(ctx, "k") }, "users.conversations"},
		{"post", &fakeAPI{postErr: boom}, func(r *Relay) (string, error) { return r.Post(ctx, "k", "C9", "", "hi") }, "chat.postMessage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, cap := newRelay(t, tc.api, RelayConfig{})
			_, err := tc.call(r)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
			if !strings.Contains(cap.joined(), "outcome=denied") {
				t.Fatalf("failure not logged: %q", cap.joined())
			}
		})
	}
}

func TestPostSuccess(t *testing.T) {
	api := &fakeAPI{postTS: "9.1"}
	r, cap := newRelay(t, api, RelayConfig{})
	out, err := r.Post(context.Background(), "C1/9.9", "C9", "1.2", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "9.1") {
		t.Fatalf("confirmation = %q", out)
	}
	// text + thread_ts.
	if api.postOpts != 2 {
		t.Fatalf("post options = %d, want 2 (text + thread_ts)", api.postOpts)
	}
	if !strings.Contains(cap.joined(), "session=C1/9.9") || !strings.Contains(cap.joined(), "channel=C9") {
		t.Fatalf("post log lacks provenance: %q", cap.joined())
	}
}

// An empty thread_ts must not be sent at all: slack-go writes the
// parameter unconditionally and Slack rejects a blank one.
func TestPostTopLevelOmitsThreadTS(t *testing.T) {
	api := &fakeAPI{postTS: "9.1"}
	r, _ := newRelay(t, api, RelayConfig{})
	if _, err := r.Post(context.Background(), "k", "C9", "", "hello"); err != nil {
		t.Fatal(err)
	}
	if api.postOpts != 1 {
		t.Fatalf("post options = %d, want 1 (text only)", api.postOpts)
	}
}

// read_write plus a live self-drive hatch would otherwise let injected
// thread text make the agent post a message that drives another session.
func TestPostRefusesSelfDriveSentinel(t *testing.T) {
	api := &fakeAPI{postTS: "9.1"}
	r, cap := newRelay(t, api, RelayConfig{SelfDrive: slackproto.NewSelfDrive("drive-me-9f3a")})
	_, err := r.Post(context.Background(), "k", "C9", "", "drive-me-9f3a do the thing")
	if err == nil || !strings.Contains(err.Error(), "self-drive sentinel") {
		t.Fatalf("err = %v", err)
	}
	if api.postCalls != 0 {
		t.Fatal("sentinel post reached Slack")
	}
	if !strings.Contains(cap.joined(), "outcome=denied") {
		t.Fatalf("refusal not logged: %q", cap.joined())
	}
	// A mention that is not a prefix gets through — the hatch only fires
	// on a prefix — but the sentinel is scrubbed on the way out, so the
	// relay can never emit a live drive token by any path.
	if _, err := r.Post(context.Background(), "k", "C9", "", "the token is drive-me-9f3a"); err != nil {
		t.Fatalf("non-prefix mention refused: %v", err)
	}
	if strings.Contains(api.postText, "drive-me-9f3a") {
		t.Fatalf("posted text still carries the sentinel: %q", api.postText)
	}
	// Every relay-posted ts must land in the self-posted memory, whatever
	// path posted it, or the streamer's loop guard has a blind spot.
	if !r.cfg.SelfDrive.SeenTS("9.1") {
		t.Fatal("agent-posted ts not recorded in the self-drive memory")
	}
}

// The literal-string blocklist this replaced missed the labelled form
// Slack itself emits, and posts go out with escape=false so Slack parses
// them. Every form must go.
func TestStripBroadcastPings(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"<!channel> hey <!here> all <!everyone>", "hey  all"},
		{"<!here|@here> ping", "ping"},
		{"<!channel|@channel>x", "x"},
		{"<!subteam^S012|@oncall> up", "up"},
		{"<!subteam^S012> up", "up"},
		{"a <@U1> b", "a <@U1> b"}, // ordinary user mentions survive
	} {
		if got := stripBroadcastPings(tc.in); got != tc.want {
			t.Errorf("stripBroadcastPings(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Bot messages carry an attacker-chosen username, so read output must
// say so rather than presenting "operator" as a human.
func TestBotMessagesFlagged(t *testing.T) {
	bot := slack.Message{}
	bot.Timestamp, bot.Username, bot.Text, bot.BotID = "1.0", "operator", "trust me", "B1"
	api := &fakeAPI{replies: []slack.Message{bot, mkMsg("1.1", "U1", "hi")}, users: map[string]*slack.User{"U1": {Name: "ada"}}}
	r, _ := newRelay(t, api, RelayConfig{})
	out, err := r.ReadThread(context.Background(), "k", "C9", "1.0", 0)
	if err != nil {
		t.Fatal(err)
	}
	got := msgs(t, out)
	if !got[0].IsBot {
		t.Error("bot message not flagged is_bot")
	}
	if got[1].IsBot {
		t.Error("human message flagged is_bot")
	}
}

// The per-call clamps bound one response; the read cap bounds a walk.
func TestReadRateCap(t *testing.T) {
	now := time.Unix(0, 0)
	api := &fakeAPI{}
	r, cap := newRelay(t, api, RelayConfig{ReadsPerMinute: 2, Now: func() time.Time { return now }})
	ctx := context.Background()
	if _, err := r.ReadThread(ctx, "k", "C9", "1.0", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadChannel(ctx, "k", "C9", 0, ""); err != nil {
		t.Fatal(err)
	}
	// The cap is shared across all three read tools, so the walk cannot
	// be continued by switching tool.
	if _, err := r.ListChannels(ctx, "k"); err == nil || !strings.Contains(err.Error(), "rate cap") {
		t.Fatalf("err = %v, want a read rate-cap refusal", err)
	}
	if !strings.Contains(cap.joined(), "outcome=denied") {
		t.Fatalf("read cap refusal not logged: %q", cap.joined())
	}
	now = now.Add(time.Minute)
	if _, err := r.ListChannels(ctx, "k"); err != nil {
		t.Fatalf("read refused after refill: %v", err)
	}

	// Each read tool must consult the cap itself; a shared bucket is no
	// use if one entry point forgets to ask.
	for name, call := range map[string]func(*Relay) (string, error){
		ToolReadThread:   func(r *Relay) (string, error) { return r.ReadThread(ctx, "k", "C9", "1.0", 0) },
		ToolReadChannel:  func(r *Relay) (string, error) { return r.ReadChannel(ctx, "k", "C9", 0, "") },
		ToolListChannels: func(r *Relay) (string, error) { return r.ListChannels(ctx, "k") },
		ToolSearch:       func(r *Relay) (string, error) { return r.Search(ctx, "k", SearchParams{Query: "x"}) },
	} {
		frozen := time.Unix(0, 0)
		rr, _ := newRelay(t, &fakeAPI{}, RelayConfig{ReadsPerMinute: 1, Now: func() time.Time { return frozen }})
		if _, err := call(rr); err != nil {
			t.Fatalf("%s: first call refused: %v", name, err)
		}
		if _, err := call(rr); err == nil || !strings.Contains(err.Error(), "rate cap") {
			t.Fatalf("%s: second call err = %v, want a rate-cap refusal", name, err)
		}
	}
}

func TestPostRateCap(t *testing.T) {
	now := time.Unix(0, 0)
	api := &fakeAPI{postTS: "9.1"}
	r, cap := newRelay(t, api, RelayConfig{PostsPerMinute: 2, Now: func() time.Time { return now }})
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := r.Post(ctx, "k", "C9", "", "hi"); err != nil {
			t.Fatalf("post %d refused inside the cap: %v", i+1, err)
		}
	}
	_, err := r.Post(ctx, "k", "C9", "", "hi")
	if err == nil || !strings.Contains(err.Error(), "rate cap") {
		t.Fatalf("err = %v, want a rate-cap refusal", err)
	}
	if api.postCalls != 2 {
		t.Fatalf("posts reaching Slack = %d, want 2", api.postCalls)
	}
	if !strings.Contains(cap.joined(), "rate cap") {
		t.Fatalf("cap refusal not logged: %q", cap.joined())
	}
	// A full window restores the budget.
	now = now.Add(time.Minute)
	if _, err := r.Post(ctx, "k", "C9", "", "hi"); err != nil {
		t.Fatalf("post refused after refill: %v", err)
	}
}

func TestClampLimit(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, defaultReadLimit},
		{-5, defaultReadLimit},
		{7, 7},
		{maxReadLimit + 1, maxReadLimit},
	} {
		if got := clampLimit(tc.in); got != tc.want {
			t.Errorf("clampLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 80); got != "short" {
		t.Errorf("truncate short = %q", got)
	}
	// Rune-wise, not byte-wise: a multi-byte body must not be cut mid-rune.
	if got := truncate("ααααα", 3); got != "ααα…" {
		t.Errorf("truncate long = %q", got)
	}
}

func TestMessageTextTruncated(t *testing.T) {
	long := strings.Repeat("x", maxTextRunes+50)
	api := &fakeAPI{replies: []slack.Message{mkMsg("1.0", "U1", long)}, users: map[string]*slack.User{"U1": {Name: "a"}}}
	r, _ := newRelay(t, api, RelayConfig{})
	out, err := r.ReadThread(context.Background(), "k", "C9", "1.0", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := []rune(msgs(t, out)[0].Text); len(got) != maxTextRunes+1 {
		t.Fatalf("text length = %d runes, want %d + ellipsis", len(got), maxTextRunes)
	}
}

// The default Logf must be a no-op rather than nil, or every tool call
// panics the moment an operator forgets to wire logging.
func TestNilLogfIsSafe(t *testing.T) {
	r := NewRelay(RelayConfig{API: &fakeAPI{}})
	if _, err := r.ListChannels(context.Background(), "k"); err != nil {
		t.Fatal(err)
	}
}

// encode's panic is the documented contract for a shape that cannot
// marshal. Pin it so the branch is exercised and the behaviour stays
// deliberate.
func TestEncodePanicsOnUnmarshalableShape(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("encode did not panic on an unmarshalable value")
		}
	}()
	_, _ = encode(make(chan int))
}

// --- slack_search: a bounded local fanout, not Slack search ----------

func searchOut(t *testing.T, s string) searchResult {
	t.Helper()
	var out searchResult
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("decode search result %q: %v", s, err)
	}
	return out
}

// The happy path: fan out over the bot's channels, match
// case-insensitively, and say where each hit came from.
func TestSearchFansOutOverPermittedChannels(t *testing.T) {
	api := &fakeAPI{
		channels: []slack.Channel{mkChan("C1", "ops", false), mkChan("C2", "random", false)},
		historyByChannel: map[string][]slack.Message{
			"C1": {mkMsg("1.0", "U1", "the DEPLOY is stuck"), mkMsg("1.1", "U1", "unrelated")},
			"C2": {mkMsg("2.0", "U1", "deploy notes")},
		},
		users: map[string]*slack.User{"U1": {Name: "ada"}},
	}
	r, cap := newRelay(t, api, RelayConfig{})
	out, err := r.Search(context.Background(), "C1/9.9", SearchParams{Query: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	res := searchOut(t, out)
	if res.Truncated || res.ChannelsScanned != 2 || res.ChannelsInScope != 2 {
		t.Fatalf("res = %+v", res)
	}
	if len(res.Matches) != 2 {
		t.Fatalf("matches = %+v", res.Matches)
	}
	if res.Matches[0].Channel != "C1" || res.Matches[0].ChannelName != "ops" || res.Matches[0].User != "ada" {
		t.Fatalf("match[0] = %+v", res.Matches[0])
	}
	if res.Matches[1].Channel != "C2" || res.Matches[1].TS != "2.0" {
		t.Fatalf("match[1] = %+v", res.Matches[1])
	}
	if !strings.Contains(cap.joined(), "tool="+ToolSearch) || !strings.Contains(cap.joined(), "outcome=ok") {
		t.Fatalf("search not logged: %q", cap.joined())
	}
}

// The allowlist is the search scope, exactly as for the read tools —
// never wider. A channel outside it is neither scanned nor named.
func TestSearchRespectsAllowlist(t *testing.T) {
	api := &fakeAPI{
		channels: []slack.Channel{mkChan("C1", "ops", false), mkChan("C2", "secret", false)},
		historyByChannel: map[string][]slack.Message{
			"C1": {mkMsg("1.0", "U1", "hit")},
			"C2": {mkMsg("2.0", "U1", "hit")},
		},
	}
	r, _ := newRelay(t, api, RelayConfig{AllowedChannelIDs: map[string]struct{}{"C1": {}}})
	out, err := r.Search(context.Background(), "k", SearchParams{Query: "hit"})
	if err != nil {
		t.Fatal(err)
	}
	res := searchOut(t, out)
	if len(res.Matches) != 1 || res.Matches[0].Channel != "C1" {
		t.Fatalf("matches = %+v", res.Matches)
	}
	if len(api.historyCalls) != 1 || api.historyCalls[0] != "C1" {
		t.Fatalf("history calls = %v, want only C1", api.historyCalls)
	}

	// An explicit disallowed channel is refused outright.
	if _, err := r.Search(context.Background(), "k", SearchParams{Query: "hit", Channel: "C2"}); err == nil ||
		!strings.Contains(err.Error(), "allowed_channel_ids") {
		t.Fatalf("err = %v, want an allowlist refusal", err)
	}
}

// An explicit channel skips the listing call entirely.
func TestSearchSingleChannel(t *testing.T) {
	api := &fakeAPI{historyByChannel: map[string][]slack.Message{"C9": {mkMsg("1.0", "U1", "needle")}}}
	r, _ := newRelay(t, api, RelayConfig{})
	out, err := r.Search(context.Background(), "k", SearchParams{Query: "needle", Channel: "C9"})
	if err != nil {
		t.Fatal(err)
	}
	res := searchOut(t, out)
	if res.ChannelsInScope != 1 || len(res.Matches) != 1 || res.Matches[0].ChannelName != "" {
		t.Fatalf("res = %+v", res)
	}
}

// Result hygiene must match the read tools: bot-authored messages are
// flagged, and bodies are truncated.
func TestSearchAppliesReadHygiene(t *testing.T) {
	bot := slack.Message{}
	bot.Timestamp, bot.Username, bot.BotID, bot.Text = "1.0", "operator", "B1", "needle from a webhook"
	long := mkMsg("1.1", "U1", "needle "+strings.Repeat("x", maxTextRunes+50))
	api := &fakeAPI{historyByChannel: map[string][]slack.Message{"C9": {bot, long}}}
	r, _ := newRelay(t, api, RelayConfig{})
	out, err := r.Search(context.Background(), "k", SearchParams{Query: "needle", Channel: "C9"})
	if err != nil {
		t.Fatal(err)
	}
	res := searchOut(t, out)
	if len(res.Matches) != 2 {
		t.Fatalf("matches = %+v", res.Matches)
	}
	if !res.Matches[0].IsBot || res.Matches[0].User != "operator" {
		t.Fatalf("webhook message not flagged is_bot: %+v", res.Matches[0])
	}
	if res.Matches[1].IsBot {
		t.Error("human message flagged is_bot")
	}
	if n := len([]rune(res.Matches[1].Text)); n != maxTextRunes+1 {
		t.Fatalf("body not truncated: %d runes", n)
	}
}

// A fanout costs one read token per channel, not one per call. When the
// shared budget runs out mid-scan the caller gets what was found plus an
// explicit truncation note — never a silently short answer.
func TestSearchBudgetExhaustedMidFanout(t *testing.T) {
	now := time.Unix(0, 0)
	api := &fakeAPI{
		channels: []slack.Channel{mkChan("C1", "a", false), mkChan("C2", "b", false), mkChan("C3", "c", false)},
		historyByChannel: map[string][]slack.Message{
			"C1": {mkMsg("1.0", "U1", "needle")},
			"C2": {mkMsg("2.0", "U1", "needle")},
			"C3": {mkMsg("3.0", "U1", "needle")},
		},
	}
	// 3 tokens: listing + C1 + C2, leaving C3 unscanned.
	r, cap := newRelay(t, api, RelayConfig{ReadsPerMinute: 3, Now: func() time.Time { return now }})
	out, err := r.Search(context.Background(), "k", SearchParams{Query: "needle"})
	if err != nil {
		t.Fatalf("budget exhaustion must degrade, not fail: %v", err)
	}
	res := searchOut(t, out)
	if !res.Truncated || res.Note != noteBudget {
		t.Fatalf("res = %+v, want truncated with the budget note", res)
	}
	if res.ChannelsScanned != 2 || res.ChannelsInScope != 3 || len(res.Matches) != 2 {
		t.Fatalf("res = %+v", res)
	}
	if !strings.Contains(cap.joined(), "outcome=denied") {
		t.Fatalf("mid-fanout refusal not logged: %q", cap.joined())
	}
}

func TestSearchLimitTruncation(t *testing.T) {
	ctx := context.Background()

	// Limit reached inside a channel's own hits.
	api := &fakeAPI{historyByChannel: map[string][]slack.Message{
		"C9": {mkMsg("1.0", "U1", "needle a"), mkMsg("1.1", "U1", "needle b")},
	}}
	r, _ := newRelay(t, api, RelayConfig{})
	out, err := r.Search(ctx, "k", SearchParams{Query: "needle", Channel: "C9", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res := searchOut(t, out); !res.Truncated || res.Note != noteLimit || len(res.Matches) != 1 {
		t.Fatalf("res = %+v", res)
	}

	// Limit reached before the next channel is even scanned.
	api2 := &fakeAPI{
		channels: []slack.Channel{mkChan("C1", "a", false), mkChan("C2", "b", false)},
		historyByChannel: map[string][]slack.Message{
			"C1": {mkMsg("1.0", "U1", "needle")},
			"C2": {mkMsg("2.0", "U1", "needle")},
		},
	}
	r2, _ := newRelay(t, api2, RelayConfig{})
	out2, err := r2.Search(ctx, "k", SearchParams{Query: "needle", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	res2 := searchOut(t, out2)
	if !res2.Truncated || res2.Note != noteLimit || res2.ChannelsScanned != 1 {
		t.Fatalf("res = %+v", res2)
	}
	if len(api2.historyCalls) != 1 {
		t.Fatalf("scanned %v after the limit was reached", api2.historyCalls)
	}
}

// More permitted channels than one call may scan: bounded independently
// of how much rate budget happened to be left, and reported.
func TestSearchFanoutCapped(t *testing.T) {
	api := &fakeAPI{historyByChannel: map[string][]slack.Message{}}
	for i := range maxSearchChannels + 5 {
		id := fmt.Sprintf("C%03d", i)
		api.channels = append(api.channels, mkChan(id, id, false))
	}
	r, _ := newRelay(t, api, RelayConfig{ReadsPerMinute: 1000})
	out, err := r.Search(context.Background(), "k", SearchParams{Query: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	res := searchOut(t, out)
	if !res.Truncated || res.Note != noteFanout || res.ChannelsScanned != maxSearchChannels {
		t.Fatalf("res = %+v", res)
	}
}

// include_threads reaches replies, deduplicating the thread parent that
// conversations.history already returned.
func TestSearchIncludeThreads(t *testing.T) {
	parent := mkMsg("1.0", "U1", "needle in parent")
	parent.ReplyCount = 2
	api := &fakeAPI{
		historyByChannel: map[string][]slack.Message{"C9": {parent, mkMsg("1.9", "U1", "no thread here")}},
		repliesByTS: map[string][]slack.Message{
			"1.0": {parent, mkMsg("1.1", "U2", "needle in reply")},
		},
	}
	r, _ := newRelay(t, api, RelayConfig{})
	ctx := context.Background()

	out, err := r.Search(ctx, "k", SearchParams{Query: "needle", Channel: "C9", IncludeThreads: true})
	if err != nil {
		t.Fatal(err)
	}
	res := searchOut(t, out)
	if len(res.Matches) != 2 || res.Matches[0].TS != "1.0" || res.Matches[1].TS != "1.1" {
		t.Fatalf("matches = %+v", res.Matches)
	}
	if len(api.replyCalls) != 1 || api.replyCalls[0] != "C9@1.0" {
		t.Fatalf("reply calls = %v", api.replyCalls)
	}

	// Off by default: no reply fetch, so the reply is not found.
	api.replyCalls = nil
	out, err = r.Search(ctx, "k", SearchParams{Query: "needle", Channel: "C9"})
	if err != nil {
		t.Fatal(err)
	}
	if res := searchOut(t, out); len(res.Matches) != 1 || len(api.replyCalls) != 0 {
		t.Fatalf("res = %+v calls = %v", res, api.replyCalls)
	}
}

func TestSearchThreadCaps(t *testing.T) {
	hist := make([]slack.Message, 0, maxSearchThreadFetches+2)
	replies := map[string][]slack.Message{}
	for i := range maxSearchThreadFetches + 2 {
		m := mkMsg(fmt.Sprintf("1.%02d", i), "U1", "parent")
		m.ReplyCount = 1
		hist = append(hist, m)
		replies[m.Timestamp] = []slack.Message{mkMsg(m.Timestamp+"1", "U2", "needle")}
	}
	ctx := context.Background()

	// Per-call thread cap.
	api := &fakeAPI{historyByChannel: map[string][]slack.Message{"C9": hist}, repliesByTS: replies}
	r, _ := newRelay(t, api, RelayConfig{ReadsPerMinute: 1000})
	out, err := r.Search(ctx, "k", SearchParams{Query: "needle", Channel: "C9", Limit: maxSearchLimit, IncludeThreads: true})
	if err != nil {
		t.Fatal(err)
	}
	if res := searchOut(t, out); !res.Truncated || res.Note != noteThreads || len(res.Matches) != maxSearchThreadFetches {
		t.Fatalf("res = %+v", res)
	}

	// Read budget exhausted inside the thread walk: same partial-plus-note
	// contract as the channel fanout.
	frozen := time.Unix(0, 0)
	api2 := &fakeAPI{historyByChannel: map[string][]slack.Message{"C9": hist}, repliesByTS: replies}
	r2, _ := newRelay(t, api2, RelayConfig{ReadsPerMinute: 3, Now: func() time.Time { return frozen }})
	out2, err := r2.Search(ctx, "k", SearchParams{Query: "needle", Channel: "C9", IncludeThreads: true})
	if err != nil {
		t.Fatal(err)
	}
	if res := searchOut(t, out2); !res.Truncated || res.Note != noteBudget || len(res.Matches) != 2 {
		t.Fatalf("res = %+v", res)
	}
}

func TestSearchAPIErrors(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("boom")

	if _, err := mustRelay(t, &fakeAPI{channelsErr: boom}).Search(ctx, "k", SearchParams{Query: "x"}); err == nil ||
		!strings.Contains(err.Error(), "users.conversations") {
		t.Fatalf("err = %v", err)
	}
	if _, err := mustRelay(t, &fakeAPI{historyErr: boom}).Search(ctx, "k", SearchParams{Query: "x", Channel: "C9"}); err == nil ||
		!strings.Contains(err.Error(), "conversations.history") {
		t.Fatalf("err = %v", err)
	}
	// Empty query is rejected relay-side too, not only at the tool layer.
	if _, err := mustRelay(t, &fakeAPI{}).Search(ctx, "k", SearchParams{Query: "  "}); err == nil ||
		!strings.Contains(err.Error(), "query is required") {
		t.Fatalf("err = %v", err)
	}
}

func mustRelay(t *testing.T, api API) *Relay {
	t.Helper()
	r, _ := newRelay(t, api, RelayConfig{})
	return r
}

// The time bound is the relay's, not the agent's: `days` is clamped, and
// an unbounded search cannot walk all history.
func TestSearchClamps(t *testing.T) {
	if got := clampSearchLimit(0); got != defaultSearchLimit {
		t.Errorf("limit 0 → %d", got)
	}
	if got := clampSearchLimit(maxSearchLimit + 1); got != maxSearchLimit {
		t.Errorf("limit over cap → %d", got)
	}
	if got := clampSearchLimit(7); got != 7 {
		t.Errorf("limit 7 → %d", got)
	}
	if got := clampSearchDays(0); got != defaultSearchDays {
		t.Errorf("days 0 → %d", got)
	}
	if got := clampSearchDays(maxSearchDays + 1); got != maxSearchDays {
		t.Errorf("days over cap → %d", got)
	}

	now := time.Unix(1_000_000, 0)
	api := &fakeAPI{historyByChannel: map[string][]slack.Message{"C9": nil}}
	r, _ := newRelay(t, api, RelayConfig{Now: func() time.Time { return now }})
	for _, tc := range []struct{ days, want int }{
		{0, defaultSearchDays}, {3, 3}, {maxSearchDays + 100, maxSearchDays},
	} {
		if _, err := r.Search(context.Background(), "k", SearchParams{Query: "x", Channel: "C9", Days: tc.days}); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("%d.000000", now.Add(-time.Duration(tc.want)*24*time.Hour).Unix())
		if api.historyOldest != want {
			t.Fatalf("days %d → oldest %s, want %s", tc.days, api.historyOldest, want)
		}
	}
}

// The default clock is the wall clock; nothing else in the package
// exercises the nil-Now branch of the search window.
func TestSearchDefaultClock(t *testing.T) {
	api := &fakeAPI{historyByChannel: map[string][]slack.Message{"C9": nil}}
	r, _ := newRelay(t, api, RelayConfig{})
	if _, err := r.Search(context.Background(), "k", SearchParams{Query: "x", Channel: "C9"}); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-defaultSearchDays * 24 * time.Hour).Unix()
	var got int64
	if _, err := fmt.Sscanf(api.historyOldest, "%d.000000", &got); err != nil {
		t.Fatalf("oldest %q: %v", api.historyOldest, err)
	}
	if d := cutoff - got; d < 0 || d > 5 {
		t.Fatalf("oldest %d is %ds from the expected cutoff %d", got, d, cutoff)
	}
}

// ---- regression tests for the five holes the post-rebase review found ----

// With no allowlist configured, "wherever the bot already is" must NOT
// include DMs. The bot holds a 1:1 DM with everyone who ever messaged
// it, and these tools are driven by an agent steered by ambient thread
// text written by someone else — so reaching a D… conversation lets one
// person's prompt surface another person's private conversation.
func TestReadToolsRefuseDMsWithoutAnExplicitAllowlist(t *testing.T) {
	ctx := context.Background()
	api := &fakeAPI{
		historyByChannel: map[string][]slack.Message{"D9": {mkMsg("1.0", "U1", "private")}},
		repliesByTS:      map[string][]slack.Message{"1.0": {mkMsg("1.0", "U1", "private")}},
	}
	r, cap := newRelay(t, api, RelayConfig{})

	for _, tc := range []struct {
		name string
		call func() (string, error)
	}{
		{"read_channel", func() (string, error) { return r.ReadChannel(ctx, "k", "D9", 0, "") }},
		{"read_thread", func() (string, error) { return r.ReadThread(ctx, "k", "D9", "1.0", 0) }},
		{"search", func() (string, error) { return r.Search(ctx, "k", SearchParams{Query: "private", Channel: "D9"}) }},
		{"post", func() (string, error) { return r.Post(ctx, "k", "D9", "", "hi") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.call()
			if err == nil || !strings.Contains(err.Error(), "direct message") {
				t.Fatalf("err = %v — a DM must be refused when no allowlist names it", err)
			}
		})
	}
	if len(api.historyCalls) != 0 || api.postCalls != 0 {
		t.Fatalf("a refused DM still reached Slack: history=%v post=%d", api.historyCalls, api.postCalls)
	}
	if !strings.Contains(cap.joined(), "outcome=denied") {
		t.Fatalf("refusals must be logged: %q", cap.joined())
	}
}

// An operator who explicitly names a DM in allowed_channel_ids meant it;
// the allowlist is the exhaustive answer and overrides the DM rule.
func TestExplicitAllowlistMayNameADM(t *testing.T) {
	api := &fakeAPI{historyByChannel: map[string][]slack.Message{"D9": {mkMsg("1.0", "U1", "hi")}}}
	r, _ := newRelay(t, api, RelayConfig{AllowedChannelIDs: map[string]struct{}{"D9": {}}})
	out, err := r.ReadChannel(context.Background(), "k", "D9", 0, "")
	if err != nil {
		t.Fatalf("explicitly allowlisted DM refused: %v", err)
	}
	if got := msgs(t, out); len(got) != 1 {
		t.Fatalf("rendered = %+v", got)
	}
}

// users.conversations is one page, not a walk. A bot in more channels
// than that page holds gets a SHORT answer — and a short answer must say
// so, on both tools that share the listing.
func TestChannelListingTruncationIsReported(t *testing.T) {
	ctx := context.Background()
	api := &fakeAPI{
		channels:      []slack.Channel{mkChan("C1", "general", false)},
		channelCursor: "more-pages",
		historyByChannel: map[string][]slack.Message{
			"C1": {mkMsg("1.0", "U1", "needle")},
		},
	}
	r, _ := newRelay(t, api, RelayConfig{})

	list, err := r.ListChannels(ctx, "k")
	if err != nil {
		t.Fatal(err)
	}
	if res := channelListOut(t, list); !res.Truncated || res.Note != noteChannelList {
		t.Fatalf("slack_list_channels hid a short listing: %+v", res)
	}

	out, err := r.Search(ctx, "k", SearchParams{Query: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	res := searchOut(t, out)
	if !res.Truncated || len(res.Matches) != 1 {
		t.Fatalf("search over a short listing must stay truncated: %+v", res)
	}
}

// A single unreadable thread (archived, deleted, permissions) must not
// discard every match already found. It is a shortfall, reported like
// any other bound, not a fatal error.
func TestSearchSurvivesAnUnreadableThread(t *testing.T) {
	threaded := mkMsg("2.0", "U1", "needle in a parent")
	threaded.ReplyCount = 1
	api := &fakeAPI{
		historyByChannel: map[string][]slack.Message{"C9": {threaded}},
		repliesErr:       errors.New("thread_not_found"),
	}
	r, cap := newRelay(t, api, RelayConfig{})
	out, err := r.Search(context.Background(), "k", SearchParams{Query: "needle", Channel: "C9", IncludeThreads: true})
	if err != nil {
		t.Fatalf("one bad thread aborted the whole search: %v", err)
	}
	res := searchOut(t, out)
	if len(res.Matches) != 1 {
		t.Fatalf("matches found before the bad thread were discarded: %+v", res)
	}
	if !res.Truncated || res.Note != noteThreadFetchFailed {
		t.Fatalf("the skipped thread was not reported: %+v", res)
	}
	if !strings.Contains(cap.joined(), "thread_not_found") {
		t.Fatalf("the underlying failure must still be logged: %q", cap.joined())
	}
}

// A body that is nothing but broadcast pings strips to empty. Slack
// rejects an empty text, so posting it would spend one of the ten posts
// per minute on a call that could never land, and hand the agent a
// mysterious no_text instead of the refusal it actually is.
func TestPostRefusesABodyThatStripsToNothingWithoutSpendingTheCap(t *testing.T) {
	api := &fakeAPI{postTS: "9.9"}
	r, cap := newRelay(t, api, RelayConfig{PostsPerMinute: 1})

	if _, err := r.Post(context.Background(), "k", "C9", "", "<!here|@here> <!channel>"); err == nil ||
		!strings.Contains(err.Error(), "empty message") {
		t.Fatalf("err = %v", err)
	}
	if api.postCalls != 0 {
		t.Fatal("an unpostable message still reached chat.postMessage")
	}
	// The cap was not charged: a real post still goes through.
	if _, err := r.Post(context.Background(), "k", "C9", "", "a real message"); err != nil {
		t.Fatalf("the refused post spent the rate cap: %v", err)
	}
	if !strings.Contains(cap.joined(), "outcome=denied") {
		t.Fatalf("log = %q", cap.joined())
	}
}
