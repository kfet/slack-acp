package catchup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

var t0 = time.Unix(1_700_000_000, 500_000)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state", "catchup.json"), t0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTS(t *testing.T) {
	if got := TS(t0); got != "1700000000.000500" {
		t.Fatalf("TS = %q", got)
	}
}

func TestStoreFirstRunSetsFloorToNow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catchup.json")
	s, err := Open(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Mark("C1"); got != TS(t0) {
		t.Fatalf("mark of an unseen channel = %q, want the floor", got)
	}
	// The floor is persisted at once: a second open at a later time
	// keeps it rather than moving "now" forward past a gap.
	s2, err := Open(path, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Mark("C1"); got != TS(t0) {
		t.Fatalf("reopened floor = %q", got)
	}
}

func TestStoreAdvanceIsMonotonicAndPersisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catchup.json")
	s, _ := Open(path, t0)
	if err := s.Advance("C1", "1700000100.000000"); err != nil {
		t.Fatal(err)
	}
	if err := s.Advance("C1", "1700000050.000000"); err != nil {
		t.Fatal(err)
	}
	// Below the floor is a no-op for a channel with no mark of its own.
	if err := s.Advance("C2", "1600000000.000000"); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(path, t0)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Mark("C1"); got != "1700000100.000000" {
		t.Fatalf("C1 = %q", got)
	}
	if got := s2.Mark("C2"); got != TS(t0) {
		t.Fatalf("C2 = %q", got)
	}
}

func TestStoreOpenErrors(t *testing.T) {
	dir := t.TempDir()
	// Unreadable: the path is a directory.
	if _, err := Open(dir, t0); err == nil || !strings.Contains(err.Error(), "read checkpoint") {
		t.Fatalf("dir path: %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	for _, body := range []string{"not json", `{"channels":{}}`} {
		if err := os.WriteFile(bad, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(bad, t0); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("%q: %v", body, err)
		}
	}
	// A file with a floor and no channel map is valid.
	if err := os.WriteFile(bad, []byte(`{"floor":"1.000000"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(bad, t0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Advance("C1", "2.000000"); err != nil {
		t.Fatal(err)
	}
	// First run under a directory that cannot be written: the floor
	// cannot be persisted, and Open says so.
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	if _, err := Open(filepath.Join(ro, "x", "catchup.json"), t0); err == nil || !strings.Contains(err.Error(), "mkdir") {
		t.Fatalf("mkdir: %v", err)
	}
}

func TestStoreSaveErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catchup.json")
	s, _ := Open(path, t0)

	// The temp file's name is taken by a directory.
	if err := os.Mkdir(path+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Advance("C1", "1800000000.000000"); err == nil || !strings.Contains(err.Error(), "write checkpoint") {
		t.Fatalf("write: %v", err)
	}
	if err := os.Remove(path + ".tmp"); err != nil {
		t.Fatal(err)
	}
	// The checkpoint's own name is taken by a non-empty directory.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(path, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Advance("C1", "1900000000.000000"); err == nil || !strings.Contains(err.Error(), "commit checkpoint") {
		t.Fatalf("rename: %v", err)
	}
}

// fakeAPI serves scripted pages. Keys: "list", "history:<ch>",
// "replies:<ch>/<thread>". Each call pops the next step for its key; a
// key with no steps left answers an empty, final page.
type fakeAPI struct {
	steps map[string][]step
	calls []string
}

type step struct {
	err     error
	chans   []slack.Channel
	msgs    []slack.Message
	next    string
	hasMore bool
}

func (f *fakeAPI) pop(key string) step {
	f.calls = append(f.calls, key)
	s := f.steps[key]
	if len(s) == 0 {
		return step{}
	}
	f.steps[key] = s[1:]
	return s[0]
}

func (f *fakeAPI) GetConversationsForUserContext(_ context.Context, p *slack.GetConversationsForUserParameters) ([]slack.Channel, string, error) {
	if strings.Join(p.Types, ",") != "public_channel,private_channel,im" || !p.ExcludeArchived {
		panic("unexpected conversations params")
	}
	s := f.pop("list")
	return s.chans, s.next, s.err
}

func (f *fakeAPI) GetConversationHistoryContext(_ context.Context, p *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	s := f.pop("history:" + p.ChannelID + "@" + p.Oldest + "#" + p.Cursor)
	if s.err != nil {
		return nil, s.err
	}
	r := &slack.GetConversationHistoryResponse{HasMore: s.hasMore, Messages: s.msgs}
	r.ResponseMetaData.NextCursor = s.next
	return r, nil
}

func (f *fakeAPI) GetConversationRepliesContext(_ context.Context, p *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	s := f.pop("replies:" + p.ChannelID + "/" + p.Timestamp + "@" + p.Oldest + "#" + p.Cursor)
	return s.msgs, s.hasMore, s.next, s.err
}

func chans(ids ...string) []slack.Channel {
	var out []slack.Channel
	for _, id := range ids {
		c := slack.Channel{}
		c.ID = id
		c.IsIM = strings.HasPrefix(id, "D")
		out = append(out, c)
	}
	return out
}

func msg(ts string) slack.Message {
	m := slack.Message{}
	m.Timestamp = ts
	m.User = "U1"
	m.Text = "m" + ts
	return m
}

func parent(ts, latest string) slack.Message {
	m := msg(ts)
	m.ThreadTimestamp = ts
	m.ReplyCount = 1
	m.LatestReply = latest
	return m
}

type delivery struct {
	conv Conversation
	ts   []string
}

func collect(out *[]delivery) Deliver {
	return func(_ context.Context, conv Conversation, msgs []slack.Message) {
		d := delivery{conv: conv}
		for _, m := range msgs {
			d.ts = append(d.ts, m.Timestamp)
		}
		*out = append(*out, d)
	}
}

const floor = "1700000000.000500"

// seeded is the checkpoint a channel known from an earlier run has.
const seeded = "1700000001.000000"

func seed(t *testing.T, s *Store, channels ...string) {
	t.Helper()
	for _, ch := range channels {
		if err := s.Advance(ch, seeded); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunReadsHistoryRepliesAndKnownThreads(t *testing.T) {
	store := openStore(t)
	if err := store.Advance("C1", "1700000010.000000"); err != nil {
		t.Fatal(err)
	}
	mark := "1700000010.000000"
	old := TS(time.Unix(1700000010, 0).Add(-DefaultThreadMaxAge - time.Hour))
	api := &fakeAPI{steps: map[string][]step{
		"list": {
			{chans: chans("C1"), next: "p2"},
			{chans: chans("D1")},
		},
		"history:C1@" + mark + "#": {{
			// Newest first, as Slack pages. One at the mark (the
			// exclusive anchor leaking), one thread parent with new
			// replies, one parent whose replies are all old.
			msgs: []slack.Message{
				msg("1700000030.000000"),
				parent("1700000020.000000", "1700000040.000000"),
				parent("1700000015.000000", "1700000016.000000"),
				msg(mark),
			},
			hasMore: true, next: "h2",
		}},
		"history:C1@" + mark + "#h2": {{msgs: []slack.Message{msg("1700000012.000000")}}},
		"replies:C1/1700000020.000000@" + mark + "#": {
			{msgs: []slack.Message{parent("1700000020.000000", "1700000040.000000")}, hasMore: true, next: "r2"},
		},
		"replies:C1/1700000020.000000@" + mark + "#r2": {{msgs: []slack.Message{msg("1700000040.000000")}}},
		// A known thread from before the checkpoint, re-read.
		"replies:C1/1700000001.000000@" + mark + "#": {{msgs: []slack.Message{msg("1700000001.000000"), msg("1700000035.000000")}}},
		// A known thread that was deleted.
		"replies:C1/1700000002.000000@" + mark + "#": {{err: errors.New("thread_not_found")}},
		"history:D1@" + floor + "#":                  {{msgs: []slack.Message{msg("1700000050.000000")}}},
	}}
	var out []delivery
	sc := New(Config{
		API:   api,
		Store: store,
		Threads: func(ch string) []string {
			if ch != "C1" {
				return nil
			}
			return []string{"1700000001.000000", "1700000002.000000", old}
		},
	})
	st, err := sc.Run(context.Background(), collect(&out))
	if err != nil {
		t.Fatal(err)
	}
	want := []delivery{
		{Conversation{ID: "C1"}, []string{"1700000012.000000", "1700000015.000000", "1700000020.000000", "1700000030.000000", "1700000035.000000", "1700000040.000000"}},
		{Conversation{ID: "D1", IsIM: true}, []string{"1700000050.000000"}},
	}
	if len(out) != len(want) {
		t.Fatalf("deliveries = %+v", out)
	}
	for i := range want {
		if out[i].conv != want[i].conv || strings.Join(out[i].ts, " ") != strings.Join(want[i].ts, " ") {
			t.Fatalf("delivery %d = %+v, want %+v", i, out[i], want[i])
		}
	}
	if got := store.Mark("C1"); got != "1700000040.000000" {
		t.Fatalf("C1 checkpoint = %q", got)
	}
	if got := store.Mark("D1"); got != "1700000050.000000" {
		t.Fatalf("D1 checkpoint = %q", got)
	}
	for _, c := range api.calls {
		if strings.Contains(c, old) {
			t.Fatalf("should not have read %s", c)
		}
	}
	if st.Conversations != 2 || st.Messages != 7 || st.Failed != 0 || st.Calls != len(api.calls) {
		t.Fatalf("stats = %+v (calls %v)", st, api.calls)
	}
}

func TestRunNothingNew(t *testing.T) {
	store := openStore(t)
	seed(t, store, "C1")
	api := &fakeAPI{steps: map[string][]step{"list": {{chans: chans("C1")}}}}
	var out []delivery
	if _, err := New(Config{API: api, Store: store}).Run(context.Background(), collect(&out)); err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 || store.Mark("C1") != seeded {
		t.Fatalf("out=%v mark=%s", out, store.Mark("C1"))
	}
}

func TestRunListErrorFailsTheRun(t *testing.T) {
	api := &fakeAPI{steps: map[string][]step{"list": {{err: errors.New("invalid_auth")}}}}
	_, err := New(Config{API: api, Store: openStore(t)}).Run(context.Background(), collect(new([]delivery)))
	if err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunFailedConversationKeepsItsCheckpoint(t *testing.T) {
	store := openStore(t)
	seed(t, store, "C1", "C2", "C3")
	api := &fakeAPI{steps: map[string][]step{
		"list":                       {{chans: chans("C1", "C2", "C3")}},
		"history:C1@" + seeded + "#": {{err: errors.New("not_in_channel")}},
		"history:C2@" + seeded + "#": {{msgs: []slack.Message{parent("1700000020.000000", "1700000030.000000")}}},
		"replies:C2/1700000020.000000@" + seeded + "#": {{err: errors.New("boom")}},
		"history:C3@" + seeded + "#":                   {{msgs: []slack.Message{msg("1700000020.000000")}}},
	}}
	var logs []string
	var out []delivery
	st, err := New(Config{API: api, Store: store, Logf: func(f string, a ...any) { logs = append(logs, f) }}).Run(context.Background(), collect(&out))
	if err != nil {
		t.Fatal(err)
	}
	if st.Failed != 2 || len(out) != 1 || out[0].conv.ID != "C3" {
		t.Fatalf("stats=%+v out=%+v", st, out)
	}
	if store.Mark("C1") != seeded || store.Mark("C2") != seeded {
		t.Fatal("a failed conversation must keep its checkpoint")
	}
	if len(logs) != 2 {
		t.Fatalf("logs = %v", logs)
	}
}

func TestRunStopsAtMaxMessages(t *testing.T) {
	store := openStore(t)
	seed(t, store, "C1")
	api := &fakeAPI{steps: map[string][]step{
		"list": {{chans: chans("C1")}},
		"history:C1@" + seeded + "#": {{
			msgs:    []slack.Message{msg("1700000030.000000"), msg("1700000020.000000")},
			hasMore: true, next: "h2",
		}},
	}}
	var logs []string
	var out []delivery
	_, err := New(Config{API: api, Store: store, MaxMessages: 2, Logf: func(f string, a ...any) { logs = append(logs, f) }}).Run(context.Background(), collect(&out))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || len(out[0].ts) != 2 || len(logs) != 1 || !strings.Contains(logs[0], "stopped at") {
		t.Fatalf("out=%v logs=%v", out, logs)
	}
	if store.Mark("C1") != "1700000030.000000" {
		t.Fatalf("mark = %s", store.Mark("C1"))
	}
}

func TestRunWaitsOutRateLimits(t *testing.T) {
	store := openStore(t)
	seed(t, store, "C1")
	api := &fakeAPI{steps: map[string][]step{
		"list": {
			{err: &slack.RateLimitedError{RetryAfter: 30 * time.Second}},
			{err: &slack.RateLimitedError{}}, // no Retry-After: wait the minimum
			{chans: chans("C1")},
		},
		"history:C1@" + seeded + "#": {{msgs: []slack.Message{msg("1700000020.000000")}}},
	}}
	var slept []time.Duration
	clock := t0
	sc := New(Config{
		API:   api,
		Store: store,
		Sleep: func(_ context.Context, d time.Duration) error {
			slept = append(slept, d)
			clock = clock.Add(d)
			return nil
		},
		Now: func() time.Time { return clock },
	})
	st, err := sc.Run(context.Background(), collect(new([]delivery)))
	if err != nil {
		t.Fatal(err)
	}
	if len(slept) != 2 || slept[0] != 30*time.Second || slept[1] != minRetryAfter {
		t.Fatalf("slept = %v", slept)
	}
	if st.RateLimited != 2 || st.Waited != 31*time.Second || st.Calls != 4 || st.Elapsed != 31*time.Second {
		t.Fatalf("stats = %+v", st)
	}
	for _, want := range []string{"1 conversation(s)", "4 API call(s)", "2 rate-limited", "waited 31s"} {
		if !strings.Contains(st.String(), want) {
			t.Fatalf("%q lacks %q", st, want)
		}
	}
}

func TestRunGivesUpAfterRetries(t *testing.T) {
	rl := &slack.RateLimitedError{RetryAfter: time.Second}
	api := &fakeAPI{steps: map[string][]step{"list": {{err: rl}, {err: rl}, {err: rl}}}}
	sc := New(Config{API: api, Store: openStore(t), Retries: 2, Sleep: func(context.Context, time.Duration) error { return nil }})
	st, err := sc.Run(context.Background(), collect(new([]delivery)))
	if !errors.As(err, &rl) || st.Calls != 3 || st.RateLimited != 3 {
		t.Fatalf("err=%v stats=%+v", err, st)
	}
}

func TestRunStopsWhenSleepIsCancelled(t *testing.T) {
	api := &fakeAPI{steps: map[string][]step{"list": {{err: &slack.RateLimitedError{RetryAfter: time.Hour}}}}}
	sc := New(Config{API: api, Store: openStore(t), Sleep: func(context.Context, time.Duration) error { return context.Canceled }})
	if _, err := sc.Run(context.Background(), collect(new([]delivery))); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunStopsOnCancelledContext(t *testing.T) {
	api := &fakeAPI{steps: map[string][]step{"list": {{chans: chans("C1", "C2")}}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	st, err := New(Config{API: api, Store: openStore(t)}).Run(ctx, collect(new([]delivery)))
	if err != nil || st.Conversations != 0 {
		t.Fatalf("err=%v stats=%+v", err, st)
	}
}

func TestRunLogsCheckpointWriteFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catchup.json")
	store, _ := Open(path, t0)
	if err := os.Mkdir(path+".tmp", 0o755); err != nil {
		t.Fatal(err)
	}
	api := &fakeAPI{steps: map[string][]step{
		"list":                      {{chans: chans("C1", "D1")}},
		"history:D1@" + floor + "#": {{msgs: []slack.Message{msg("1700000020.000000")}}},
	}}
	var logs []string
	_, err := New(Config{API: api, Store: store, Logf: func(f string, a ...any) { logs = append(logs, f) }}).Run(context.Background(), collect(new([]delivery)))
	if err != nil || len(logs) != 2 || !strings.Contains(logs[0], "checkpoint") || !strings.Contains(logs[1], "checkpoint") {
		t.Fatalf("err=%v logs=%v", err, logs)
	}
}

func TestThreadCutoffMalformedMarkKeepsEveryThread(t *testing.T) {
	sc := New(Config{})
	if got := sc.threadCutoff("garbage"); got != "" {
		t.Fatalf("cutoff = %q", got)
	}
	if sc.Store() != nil {
		t.Fatal("no store configured")
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

// TestRunNewChannelStartsNow: a channel with no checkpoint of its own —
// the bot was invited since the last run — is not read; its checkpoint
// starts at now. A DM with none is read from the floor.
func TestRunNewChannelStartsNow(t *testing.T) {
	store := openStore(t)
	api := &fakeAPI{steps: map[string][]step{
		"list":                      {{chans: chans("C9", "D1")}},
		"history:D1@" + floor + "#": {{msgs: []slack.Message{msg("1700000020.000000")}}},
	}}
	// The clock moves on while the run waits out 429s on earlier
	// conversations. The stamp must be the run's START: a live message
	// in the new channel posted after the run began is held, and is
	// released only if it is newer than the stamp.
	now := time.Unix(1700000500, 0)
	clock := now
	tick := func() time.Time { t := clock; clock = clock.Add(time.Minute); return t }
	var out []delivery
	st, err := New(Config{API: api, Store: store, Now: tick}).Run(context.Background(), collect(&out))
	if err != nil {
		t.Fatal(err)
	}
	if store.Mark("C9") != TS(now) || len(out) != 1 || out[0].conv.ID != "D1" {
		t.Fatalf("mark=%s out=%+v", store.Mark("C9"), out)
	}
	for _, c := range api.calls {
		if strings.Contains(c, "C9") {
			t.Fatalf("a new channel must not be read: %v", api.calls)
		}
	}
	if st.Conversations != 2 || !store.Has("C9") || store.Has("C8") {
		t.Fatalf("stats = %+v", st)
	}
}
