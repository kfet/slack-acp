package slackproto

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// msgEvent decodes a message event the way Slack delivers it, so the
// top-level files land in ev.Message.Files.
func msgEvent(t *testing.T, raw string) *slackevents.MessageEvent {
	t.Helper()
	var ev slackevents.MessageEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	return &ev
}

func dispatchInner(c *Client, data any) {
	c.handleEventsAPI(context.Background(), slackevents.EventsAPIEvent{
		Type:       slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Data: data},
	}, "")
}

const filesJSON = `"files":[
	{"id":"F1","name":"a.png","mimetype":"image/png","size":3,"url_private":"https://files.slack.com/a","url_private_download":"https://files.slack.com/a?dl"},
	{"id":"F2","name":"b.txt","size":1,"url_private":"https://files.slack.com/b"},
	{"id":"F3","name":"ext.doc","mode":"external"},
	{"id":"F4","name":"evil","url_private":"https://evil.example.com/x"},
	{"id":"F5","name":"plain","url_private":"http://files.slack.com/x"},
	{"id":"F6","name":"bad","url_private":"https://%zz"},
	{"id":"F7","name":"gov.txt","url_private":"https://files.slack-gov.com/g"},
	{"id":"F8","name":"lookalike","url_private":"https://notslack.com/x"}]`

func TestFileShareDMDelivered(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	dispatchInner(c, msgEvent(t, `{"type":"message","subtype":"file_share","user":"U1","channel":"D1","channel_type":"im","ts":"5.0","text":"see",`+filesJSON+`}`))
	got := h.seen()
	if len(got) != 1 {
		t.Fatalf("file_share DM not delivered: %+v", got)
	}
	want := []File{
		{ID: "F1", Name: "a.png", Mimetype: "image/png", Size: 3, URL: "https://files.slack.com/a?dl"},
		{ID: "F2", Name: "b.txt", Size: 1, URL: "https://files.slack.com/b"},
	}
	want = append(want, File{ID: "F7", Name: "gov.txt", URL: "https://files.slack-gov.com/g"})
	if len(got[0].Files) != 3 || got[0].Files[0] != want[0] || got[0].Files[1] != want[1] || got[0].Files[2] != want[2] {
		t.Fatalf("files %+v", got[0].Files)
	}
}

func TestFileShareThreadReplyDelivered(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	dispatchInner(c, msgEvent(t, `{"type":"message","subtype":"file_share","user":"U1","channel":"C1","channel_type":"channel","ts":"6.0","thread_ts":"5.0",`+filesJSON+`}`))
	if got := h.seen(); len(got) != 1 || len(got[0].Files) != 3 {
		t.Fatalf("got %+v", got)
	}
}

// The relay's own outbox uploads come back as file_share from the bot
// user; they must never trigger a turn.
func TestFileShareFromSelfDropped(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	dispatchInner(c, msgEvent(t, `{"type":"message","subtype":"file_share","user":"Ubot","channel":"D1","channel_type":"im","ts":"7.0",`+filesJSON+`}`))
	if got := h.seen(); len(got) != 0 {
		t.Fatalf("self upload delivered: %+v", got)
	}
}

func TestOtherSubtypesStillDropped(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	dispatchInner(c, msgEvent(t, `{"type":"message","subtype":"file_comment","user":"U1","channel":"D1","channel_type":"im","ts":"8.0"}`))
	if got := h.seen(); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestAppMentionCarriesFiles(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	dispatchInner(c, &slackevents.AppMentionEvent{User: "U1", Channel: "C1", TimeStamp: "9.0", Text: "<@Ubot> look",
		Files: []slack.File{{ID: "F1", Name: "a", URLPrivate: "https://files.slack.com/a"}}})
	if got := h.seen(); len(got) != 1 || len(got[0].Files) != 1 || got[0].Files[0].URL != "https://files.slack.com/a" {
		t.Fatalf("got %+v", got)
	}
}

func TestSelfDriveCarriesFiles(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	c.selfDrive = NewSelfDrive("[[go]]")
	dispatchInner(c, msgEvent(t, `{"type":"message","subtype":"file_share","user":"U9","bot_id":"B1","channel":"C1","channel_type":"channel","ts":"10.0","text":"[[go]] hi",`+filesJSON+`}`))
	if got := h.seen(); len(got) != 1 || !got[0].SelfDrive || len(got[0].Files) != 3 {
		t.Fatalf("got %+v", got)
	}
}
