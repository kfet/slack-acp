package verify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSlackServer serves the handful of Web API methods the harness
// calls, so the adapter is exercised over real HTTP rather than
// stubbed out. Redirection uses SLACK_API_BASE, the same test-only
// outbound hook slackproto already honours.
func fakeSlackServer(t *testing.T, ok bool) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, body map[string]any) {
		body["ok"] = ok
		if !ok {
			body["error"] = "invalid_auth"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
	mux.HandleFunc("/auth.test", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "auth.test")
		reply(w, map[string]any{"user_id": "U123"})
	})
	mux.HandleFunc("/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		seen = append(seen, "chat.postMessage thread_ts="+r.Form.Get("thread_ts"))
		reply(w, map[string]any{"channel": "C1", "ts": "1.1"})
	})
	mux.HandleFunc("/chat.update", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "chat.update")
		reply(w, map[string]any{"channel": "C1", "ts": "1.1"})
	})
	mux.HandleFunc("/chat.delete", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "chat.delete")
		reply(w, map[string]any{"channel": "C1", "ts": "1.1"})
	})
	mux.HandleFunc("/conversations.replies", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "conversations.replies")
		reply(w, map[string]any{"messages": []map[string]any{
			{"ts": "1.0", "user": "UHUMAN", "text": "hi"},
			{"ts": "1.1", "user": "UBOT", "bot_id": "B1", "subtype": "bot_message", "text": "answer"},
		}})
	})
	mux.HandleFunc("/conversations.open", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "conversations.open")
		reply(w, map[string]any{"channel": map[string]any{"id": "D1"}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &seen
}

func newTestSlack(t *testing.T, ok bool) (Slack, *[]string) {
	t.Helper()
	srv, seen := fakeSlackServer(t, ok)
	t.Setenv("SLACK_API_BASE", srv.URL+"/")
	return NewSlack("xoxb-test"), seen
}

func TestSlackAdapterHappyPath(t *testing.T) {
	api, seen := newTestSlack(t, true)
	ctx := context.Background()

	uid, err := api.AuthTest(ctx)
	if err != nil || uid != "U123" {
		t.Fatalf("AuthTest = %q, %v", uid, err)
	}

	ts, err := api.Post(ctx, "C1", "", "hello")
	if err != nil || ts != "1.1" {
		t.Fatalf("Post = %q, %v", ts, err)
	}
	if _, err := api.Post(ctx, "C1", "1.0", "threaded"); err != nil {
		t.Fatal(err)
	}

	msgs, err := api.Replies(ctx, "C1", "1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %+v", msgs)
	}
	want := Message{TS: "1.1", User: "UBOT", BotID: "B1", SubType: "bot_message", Text: "answer"}
	if msgs[1] != want {
		t.Fatalf("got %+v want %+v", msgs[1], want)
	}

	if err := api.Update(ctx, "C1", "1.1", "edited"); err != nil {
		t.Fatal(err)
	}
	if err := api.Delete(ctx, "C1", "1.1"); err != nil {
		t.Fatal(err)
	}
	dm, err := api.OpenDM(ctx, "UHUMAN")
	if err != nil || dm != "D1" {
		t.Fatalf("OpenDM = %q, %v", dm, err)
	}

	// The threaded post must carry thread_ts; the top-level one must not.
	joined := strings.Join(*seen, "\n")
	if !strings.Contains(joined, "chat.postMessage thread_ts=1.0") || !strings.Contains(joined, "chat.postMessage thread_ts=\n") && !strings.HasSuffix(strings.Split(joined, "\n")[1], "thread_ts=") {
		t.Fatalf("thread_ts not threaded through correctly:\n%s", joined)
	}
}

func TestSlackAdapterSurfacesAPIErrors(t *testing.T) {
	api, _ := newTestSlack(t, false)
	ctx := context.Background()

	if _, err := api.AuthTest(ctx); err == nil {
		t.Error("AuthTest must surface the API error")
	}
	if _, err := api.Post(ctx, "C1", "", "x"); err == nil || !strings.Contains(err.Error(), "chat.postMessage") {
		t.Errorf("Post: got %v", err)
	}
	if err := api.Update(ctx, "C1", "1.1", "x"); err == nil || !strings.Contains(err.Error(), "chat.update") {
		t.Errorf("Update: got %v", err)
	}
	if err := api.Delete(ctx, "C1", "1.1"); err == nil || !strings.Contains(err.Error(), "chat.delete") {
		t.Errorf("Delete: got %v", err)
	}
	if _, err := api.Replies(ctx, "C1", "1.0"); err == nil || !strings.Contains(err.Error(), "conversations.replies") {
		t.Errorf("Replies: got %v", err)
	}
	if _, err := api.OpenDM(ctx, "U1"); err == nil || !strings.Contains(err.Error(), "conversations.open") {
		t.Errorf("OpenDM: got %v", err)
	}
}

// TestNewSlackWithoutAPIBase pins that the adapter is usable with no
// redirect configured (the production shape) — it must construct
// without reaching for the network.
func TestNewSlackWithoutAPIBase(t *testing.T) {
	t.Setenv("SLACK_API_BASE", "")
	if NewSlack("xoxb-test") == nil {
		t.Fatal("NewSlack returned nil")
	}
}
