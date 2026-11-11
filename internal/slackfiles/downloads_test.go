package slackfiles

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

type fakeReplies struct {
	pages [][]slack.Message
	err   error
	calls []slack.GetConversationRepliesParameters
}

func (f *fakeReplies) GetConversationRepliesContext(_ context.Context, p *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	f.calls = append(f.calls, *p)
	if f.err != nil {
		return nil, false, "", f.err
	}
	i := len(f.calls) - 1
	more := i+1 < len(f.pages)
	next := ""
	if more {
		next = "c"
	}
	return f.pages[i], more, next, nil
}

func msg(files ...slack.File) slack.Message {
	m := slack.Message{}
	m.Files = files
	return m
}

// rt is a RoundTripper standing in for the bot-authenticated file client.
type rt func(*http.Request) (*http.Response, error)

func (f rt) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func resp(code int, body string, n int64) *http.Response {
	return &http.Response{StatusCode: code, Status: http.StatusText(code), Body: io.NopCloser(strings.NewReader(body)), ContentLength: n}
}

func TestAttachmentsAndLinks(t *testing.T) {
	now := time.Unix(1000, 0)
	api := &fakeReplies{pages: [][]slack.Message{
		{msg(slack.File{ID: "F1", Name: "a.txt", Mimetype: "text/plain", Size: 5, URLPrivate: "https://files.slack.com/a"})},
		{msg(slack.File{ID: "F2", Name: "b.png", Size: 3, URLPrivate: "https://files.slack.com/b"}, slack.File{ID: "F3"})},
	}}
	var fetched []string
	client := &http.Client{Transport: rt(func(r *http.Request) (*http.Response, error) {
		fetched = append(fetched, r.URL.String())
		if r.Header.Get("X-Evil") != "" {
			t.Error("client header forwarded")
		}
		return resp(200, "hello", 5), nil
	})}
	d := NewDownloads(DownloadsConfig{API: api, Client: client, BaseURL: "https://relay.example/x/", Now: func() time.Time { return now }})
	as, err := d.AttachmentsIn(conv)
	if err != nil || len(as) != 2 || as[0].ID != "F2" || as[1].Size != 5 {
		t.Fatalf("%+v %v", as, err)
	}
	if api.calls[0].ChannelID != "C1" || api.calls[0].Timestamp != "1700.1" || api.calls[1].Cursor != "c" {
		t.Fatalf("%+v", api.calls)
	}

	api.calls = nil
	tgt, err := d.DownloadLinkFor(conv, "F1")
	if err != nil || !strings.HasPrefix(tgt.URL, "https://relay.example/x/dl/") || tgt.Size != 5 || !tgt.ExpiresAt.Equal(now.Add(LinkTTL)) {
		t.Fatalf("%+v %v", tgt, err)
	}
	path := strings.TrimPrefix(tgt.URL, "https://relay.example/x")

	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("X-Evil", "1")
	w := httptest.NewRecorder()
	d.ServeHTTP(w, req)
	if w.Code != 200 || w.Body.String() != "hello" || w.Header().Get("Content-Type") != "text/plain" ||
		!strings.Contains(w.Header().Get("Content-Disposition"), "a.txt") || fetched[0] != "https://files.slack.com/a" {
		t.Fatalf("%d %q %v %v", w.Code, w.Body, w.Header(), fetched)
	}
	// Single use.
	w = httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	if w.Code != 404 {
		t.Fatal(w.Code)
	}

	api.calls = nil
	if _, err := d.DownloadLinkFor(conv, "F9"); err == nil {
		t.Fatal("unknown id")
	}
	// Expired links 404 and are pruned on the next mint.
	api.calls = nil
	tgt, _ = d.DownloadLinkFor(conv, "F2")
	now = now.Add(LinkTTL)
	w = httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest("GET", strings.TrimPrefix(tgt.URL, "https://relay.example/x"), nil))
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	api.calls = nil
	d.DownloadLinkFor(conv, "F2")
	now = now.Add(LinkTTL)
	api.calls = nil
	d.DownloadLinkFor(conv, "F2")
	if len(d.links) != 1 {
		t.Fatal(len(d.links))
	}
}

func TestListErrors(t *testing.T) {
	d := NewDownloads(DownloadsConfig{API: &fakeReplies{err: errors.New("x")}})
	if _, err := d.AttachmentsIn(conv); err == nil {
		t.Fatal("api err")
	}
	if _, err := d.AttachmentsIn("bad"); err == nil {
		t.Fatal("bad conv")
	}
	if _, err := d.DownloadLinkFor("bad", "F1"); err == nil {
		t.Fatal("bad conv")
	}
}

func TestServeHTTPFailures(t *testing.T) {
	mint := func(d *Downloads, url string, size int64) string {
		d.links["tok"] = link{url: url, size: size, exp: time.Now().Add(time.Hour)}
		return "/dl/tok"
	}
	do := func(d *Downloads, method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		d.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	ok := &http.Client{Transport: rt(func(*http.Request) (*http.Response, error) { return resp(200, "abc", -1), nil })}
	d := NewDownloads(DownloadsConfig{Client: ok})
	if w := do(d, "POST", "/dl/x"); w.Code != 405 {
		t.Fatal(w.Code)
	}
	for _, p := range []string{"/x", "/dl/", "/dl/a/b"} {
		if w := do(d, "GET", p); w.Code != 404 {
			t.Fatal(p, w.Code)
		}
	}
	for _, u := range []string{"http://files.slack.com/a", "https://evil.example/a", "https://files.slack.com.evil.example/a",
		"https://u:p@files.slack.com/a", "%zz"} {
		if w := do(d, "GET", mint(d, u, 3)); w.Code != 502 {
			t.Fatal(u, w.Code)
		}
	}
	if w := do(d, "GET", mint(d, "https://files.slack.com/a", 3)); w.Code != 200 || w.Body.String() != "abc" ||
		w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatal(w.Code, w.Header())
	}
	// Short body: headers are out, the copy stops early.
	if w := do(d, "GET", mint(d, "https://files.slack.com/a", 9)); w.Body.String() != "abc" {
		t.Fatal(w.Body)
	}
	for name, c := range map[string]*http.Client{
		"err":    {Transport: rt(func(*http.Request) (*http.Response, error) { return nil, errors.New("down") })},
		"status": {Transport: rt(func(*http.Request) (*http.Response, error) { return resp(302, "", 0), nil })},
		"length": {Transport: rt(func(*http.Request) (*http.Response, error) { return resp(200, "ab", 2), nil })},
	} {
		d := NewDownloads(DownloadsConfig{Client: c})
		if w := do(d, "GET", mint(d, "https://files.slack.com/a", 3)); w.Code != 502 {
			t.Fatal(name, w.Code)
		}
	}
}
