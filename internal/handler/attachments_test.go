package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/slack-acp/internal/slackproto"
)

// fileHost serves Slack-private files, refusing any request that does
// not carry the bot token.
func fileHost(t *testing.T, token string, files map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runTurn delivers ev and returns the blocks the agent was prompted
// with. hook, if set, runs inside the turn with the session cwd.
func runTurn(t *testing.T, cfg Config, fa *fakeAgent, ev slackproto.Event, hook func(cwd string)) []acp.ContentBlock {
	t.Helper()
	var (
		mu  sync.Mutex
		got []acp.ContentBlock
	)
	cwd := filepath.Join(cfg.Router.StateDir(), "threads", ev.ChannelID, ev.ThreadTS)
	fa.promptHook = func(_ context.Context, _ acp.SessionId, blocks []acp.ContentBlock) (acp.StopReason, error) {
		mu.Lock()
		got = blocks
		mu.Unlock()
		if hook != nil {
			hook(cwd)
		}
		return acp.StopReasonEndTurn, nil
	}
	h := New(cfg)
	h.Handle(context.Background(), ev)
	waitForIdle(t, h)
	mu.Lock()
	defer mu.Unlock()
	return got
}

func TestInboundAttachments(t *testing.T) {
	host := fileHost(t, "xoxb-test", map[string]string{"/ok.txt": "hello"})
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()

	ev := slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "2.5", Text: "look",
		Files: []slackproto.File{
			{ID: "F1", Name: "ok.txt", Mimetype: "text/plain", Size: 5, URL: host.URL + "/ok.txt"},
			{ID: "F2", Name: "big.bin", Size: 100, URL: host.URL + "/big.bin"},
			{ID: "F3", Name: "gone.txt", Size: 1, URL: host.URL + "/gone.txt"},
		}}
	blocks := runTurn(t, Config{Router: r, API: fs.client(), FileClient: NewFileClient("xoxb-test"), MaxAttachmentBytes: 10}, fa, ev, nil)

	if len(blocks) != 2 || blocks[1].ResourceLink == nil {
		t.Fatalf("want text + 1 resource_link, got %+v", blocks)
	}
	want := filepath.Join(r.StateDir(), "threads", "C1", "1.0", InboxDir, "2.5", "ok.txt")
	if b, err := os.ReadFile(want); err != nil || string(b) != "hello" {
		t.Fatalf("saved file: %q %v", b, err)
	}
	if blocks[1].ResourceLink.Name != "ok.txt" {
		t.Errorf("link name %q", blocks[1].ResourceLink.Name)
	}
	text := blocks[0].Text.Text
	for _, sub := range []string{"look", "saved to: " + want, "big.bin (100 bytes, over the 10-byte cap)", "gone.txt (http 404)"} {
		if !strings.Contains(text, sub) {
			t.Errorf("prompt text missing %q:\n%s", sub, text)
		}
	}
}

func TestInboundWithoutTokenFails(t *testing.T) {
	host := fileHost(t, "xoxb-test", map[string]string{"/ok.txt": "hello"})
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()
	ev := slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "x",
		Files: []slackproto.File{{Name: "ok.txt", Size: 5, URL: host.URL + "/ok.txt"}}}
	blocks := runTurn(t, Config{Router: r, API: fs.client(), FileClient: NewFileClient("wrong")}, fa, ev, nil)
	if len(blocks) != 1 || !strings.Contains(blocks[0].Text.Text, "ok.txt (http 403)") {
		t.Fatalf("got %+v", blocks)
	}
}

func TestFileOnlyMessageIsDelivered(t *testing.T) {
	host := fileHost(t, "t", map[string]string{"/a.png": "png"})
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()
	ev := slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0",
		Files: []slackproto.File{{Name: "a.png", Mimetype: "image/png", Size: 3, URL: host.URL + "/a.png"}}}
	blocks := runTurn(t, Config{Router: r, API: fs.client(), FileClient: NewFileClient("t")}, fa, ev, nil)
	if len(blocks) != 2 {
		t.Fatalf("file-only message not prompted: %+v", blocks)
	}
}

func TestInboundFileLimit(t *testing.T) {
	host := fileHost(t, "t", map[string]string{"/f": "x"})
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()
	ev := slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "x"}
	for i := 0; i <= maxAttachments; i++ {
		ev.Files = append(ev.Files, slackproto.File{Name: "f.txt", Size: 1, URL: host.URL + "/f"})
	}
	blocks := runTurn(t, Config{Router: r, API: fs.client(), FileClient: NewFileClient("t")}, fa, ev, nil)
	if len(blocks) != 1+maxAttachments {
		t.Fatalf("want %d blocks, got %d", 1+maxAttachments, len(blocks))
	}
	if !strings.Contains(blocks[0].Text.Text, "over the 10-file limit") {
		t.Errorf("limit not named: %s", blocks[0].Text.Text)
	}
}

func TestMaxAttachmentBytesDefault(t *testing.T) {
	if got := New(Config{}).maxAttachmentBytes(); got != DefaultMaxAttachmentBytes {
		t.Fatalf("default %d", got)
	}
}

func TestOutboxUploadedAtTurnEnd(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()
	ev := slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "make files"}
	var out string
	runTurn(t, Config{Router: r, API: fs.client()}, fa, ev, func(cwd string) {
		out = filepath.Join(cwd, OutboxDir)
		must(t, os.MkdirAll(filepath.Join(out, "sub"), 0o755))
		must(t, os.WriteFile(filepath.Join(out, "b.txt"), []byte("bee"), 0o644))
		must(t, os.WriteFile(filepath.Join(out, "a.txt"), []byte("ay"), 0o644))
		must(t, os.WriteFile(filepath.Join(out, "empty"), nil, 0o644))
		must(t, os.WriteFile(filepath.Join(out, ".hidden"), []byte("h"), 0o644))
	})
	fs.mu.Lock()
	ups := strings.Join(fs.uploads, ",")
	fs.mu.Unlock()
	if ups != "a.txt=ay@1.0,b.txt=bee@1.0" {
		t.Fatalf("uploads %q", ups)
	}
	for _, n := range []string{"a.txt", "b.txt", "empty"} {
		if _, err := os.Stat(filepath.Join(out, sentDir, n)); err != nil {
			t.Errorf("%s not moved to .sent: %v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(out, ".hidden")); err != nil {
		t.Errorf("hidden file touched: %v", err)
	}
}

func TestOutboxUploadFailureKeepsFile(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()
	fs.uploadErr = true
	ev := slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "x"}
	var out string
	runTurn(t, Config{Router: r, API: fs.client()}, fa, ev, func(cwd string) {
		out = filepath.Join(cwd, OutboxDir)
		must(t, os.MkdirAll(out, 0o755))
		must(t, os.WriteFile(filepath.Join(out, "a.txt"), []byte("ay"), 0o644))
	})
	if _, err := os.Stat(filepath.Join(out, "a.txt")); err != nil {
		t.Fatalf("failed upload must stay for retry: %v", err)
	}
}

func TestOutboxNotADirectory(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()
	ev := slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "x"}
	runTurn(t, Config{Router: r, API: fs.client()}, fa, ev, func(cwd string) {
		must(t, os.WriteFile(filepath.Join(cwd, OutboxDir), []byte("x"), 0o644))
	})
	if len(fs.uploads) != 0 {
		t.Fatal("unexpected upload")
	}
}

func TestUploadOneErrors(t *testing.T) {
	h := New(Config{})
	dir := t.TempDir()
	if err := h.uploadOne(context.Background(), dir, "missing", "C", "1"); err == nil {
		t.Error("stat of missing file must fail")
	}
	must(t, os.WriteFile(filepath.Join(dir, "locked"), []byte("x"), 0))
	if err := h.uploadOne(context.Background(), dir, "locked", "C", "1"); err == nil {
		t.Error("open of unreadable file must fail")
	}
	// .sent exists as a file, so MkdirAll fails.
	must(t, os.WriteFile(filepath.Join(dir, sentDir), nil, 0o644))
	if err := moveSent(dir, "x"); err == nil {
		t.Error("moveSent must fail when .sent is a file")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestFileClientNeverFollowsRedirects(t *testing.T) {
	var leaked sync.Map
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Store("auth", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte("x"))
	}))
	defer other.Close()
	redir := httptest.NewServer(http.RedirectHandler(other.URL+"/f", http.StatusFound))
	defer redir.Close()

	resp, err := NewFileClient("xoxb-secret").Get(redir.URL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status %d, want the redirect itself", resp.StatusCode)
	}
	if v, ok := leaked.Load("auth"); ok {
		t.Fatalf("token followed a redirect: %q", v)
	}
}

func writeOutbox(t *testing.T, cwd, name, body string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Join(cwd, OutboxDir), 0o755))
	must(t, os.WriteFile(filepath.Join(cwd, OutboxDir, name), []byte(body), 0o644))
}

func uploads(fs *fakeSlack) string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return strings.Join(fs.uploads, ",")
}

func TestOutboxUploadedOnAbstain(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()
	cwd := filepath.Join(r.StateDir(), "threads", "C1", "1.0")
	fa.promptHook = func(_ context.Context, sid acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		writeOutbox(t, cwd, "chart.png", "png")
		fa.emit(sid, acp.SessionNotification{SessionId: sid, Update: acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.ContentBlock{Text: &acp.ContentBlockText{Text: "<<SILENT>>"}}}}})
		return acp.StopReasonEndTurn, nil
	}
	h := New(Config{Router: r, API: fs.client(), Ambient: true, SilentSentinel: "<<SILENT>>"})
	// Summon first so the ambient follow-up is admitted and abstainable.
	h.Handle(context.Background(), slackproto.Event{UserID: "U1", BotUserID: "BBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "<@BBOT> hi"})
	waitForIdle(t, h)
	h.Handle(context.Background(), slackproto.Event{UserID: "U2", BotUserID: "BBOT", ChannelID: "C1", ThreadTS: "1.0", TS: "2.0", Text: "chatter"})
	waitForIdle(t, h)
	if got := uploads(fs); got != "chart.png=png@1.0,chart.png=png@1.0" {
		t.Fatalf("uploads %q", got)
	}
}

func TestOutboxUploadedOnAgentError(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()
	ev := slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "x"}
	cwd := filepath.Join(r.StateDir(), "threads", "C1", "1.0")
	fa.promptHook = func(context.Context, acp.SessionId, []acp.ContentBlock) (acp.StopReason, error) {
		writeOutbox(t, cwd, "partial.txt", "p")
		return "", errors.New("boom")
	}
	h := New(Config{Router: r, API: fs.client()})
	h.Handle(context.Background(), ev)
	waitForIdle(t, h)
	if got := uploads(fs); got != "partial.txt=p@1.0" {
		t.Fatalf("uploads %q", got)
	}
}

func TestSupersededTurnLeavesOutbox(t *testing.T) {
	fa := newFakeAgent()
	r := newTestRouter(t, fa)
	fs := newFakeSlack()
	defer fs.close()
	cwd := filepath.Join(r.StateDir(), "threads", "C1", "1.0")
	started := make(chan struct{})
	var (
		calls      int
		stillThere bool
	)
	fa.promptHook = func(ctx context.Context, _ acp.SessionId, _ []acp.ContentBlock) (acp.StopReason, error) {
		calls++
		if calls == 1 {
			writeOutbox(t, cwd, "half.txt", "h")
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		}
		_, err := os.Stat(filepath.Join(cwd, OutboxDir, "half.txt"))
		stillThere = err == nil
		return acp.StopReasonEndTurn, nil
	}
	h := New(Config{Router: r, API: fs.client()})
	ev := slackproto.Event{UserID: "U1", ChannelID: "C1", ThreadTS: "1.0", TS: "1.0", Text: "first"}
	h.Handle(context.Background(), ev)
	<-started
	ev.TS, ev.Text = "2.0", "second"
	h.Handle(context.Background(), ev)
	waitForIdle(t, h)
	if !stillThere {
		t.Fatal("superseded turn uploaded the outbox under its own answer")
	}
	if got := uploads(fs); got != "half.txt=h@1.0" {
		t.Fatalf("uploads %q", got)
	}
}

func TestIngestFeedsLiveness(t *testing.T) {
	host := fileHost(t, "t", map[string]string{"/a": "a"})
	h := New(Config{FileClient: NewFileClient("t")})
	n := 0
	ev := slackproto.Event{TS: "1.0", Files: []slackproto.File{{Name: "a", URL: host.URL + "/a"}, {Name: "b", URL: host.URL + "/a"}}}
	if _, blocks := h.ingest(context.Background(), t.TempDir(), ev, func() { n++ }); len(blocks) != 2 {
		t.Fatalf("blocks %d", len(blocks))
	}
	if n != 3 {
		t.Fatalf("progress called %d times, want once per file and once after", n)
	}
}
