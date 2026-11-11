package slackfiles

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/mcphost"
	"github.com/slack-go/slack"

	"github.com/kfet/slack-acp/internal/slackmcp"
	"github.com/kfet/slack-acp/internal/slackproto"
)

type fakeAPI struct {
	getErr, upErr, doneErr error
	gotGet                 slack.GetUploadURLExternalParameters
	uploaded               string
	done                   []slack.CompleteUploadExternalParameters
}

func (a *fakeAPI) GetUploadURLExternalContext(_ context.Context, p slack.GetUploadURLExternalParameters) (*slack.GetUploadURLExternalResponse, error) {
	a.gotGet = p
	if a.getErr != nil {
		return nil, a.getErr
	}
	return &slack.GetUploadURLExternalResponse{UploadURL: "https://files.slack.com/upload/v1/X", FileID: "F1"}, nil
}

func (a *fakeAPI) UploadToURL(_ context.Context, p slack.UploadToURLParameters) error {
	b, _ := io.ReadAll(p.Reader)
	a.uploaded = string(b)
	return a.upErr
}

func (a *fakeAPI) CompleteUploadExternalContext(_ context.Context, p slack.CompleteUploadExternalParameters) (*slack.CompleteUploadExternalResponse, error) {
	a.done = append(a.done, p)
	if a.doneErr != nil {
		return nil, a.doneErr
	}
	return &slack.CompleteUploadExternalResponse{Files: p.Files}, nil
}

const conv = "C1/1700.1/U9"

func TestPostFileTo(t *testing.T) {
	api := &fakeAPI{}
	f := New(Config{API: api, SelfDrive: slackproto.NewSelfDrive("drive-me-9f3a")})
	ids, err := f.PostFileTo(conv, command.OutFile{Name: "a.txt", Data: []byte("hi"), Caption: "see <!channel> drive-me-9f3a x"})
	if err != nil || len(ids) != 1 || ids[0] != "F1" {
		t.Fatalf("%v %v", ids, err)
	}
	d := api.done[0]
	if api.uploaded != "hi" || api.gotGet.FileSize != 2 || d.Channel != "C1" || d.ThreadTimestamp != "1700.1" ||
		strings.Contains(d.InitialComment, "drive-me") || strings.Contains(d.InitialComment, "<!channel>") {
		t.Fatalf("%+v %+v", api, d)
	}
}

func TestPostFileToErrors(t *testing.T) {
	cases := map[string]struct {
		api  *fakeAPI
		conv string
		cap  string
	}{
		"conv":     {&fakeAPI{}, "nothread", ""},
		"drive":    {&fakeAPI{}, conv, "drive-me-9f3a go"},
		"get":      {&fakeAPI{getErr: errors.New("x")}, conv, ""},
		"upload":   {&fakeAPI{upErr: errors.New("x")}, conv, ""},
		"complete": {&fakeAPI{doneErr: errors.New("x")}, conv, ""},
	}
	for name, c := range cases {
		f := New(Config{API: c.api, SelfDrive: slackproto.NewSelfDrive("drive-me-9f3a")})
		if _, err := f.PostFileTo(c.conv, command.OutFile{Name: "a", Data: []byte("x"), Caption: c.cap}); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestRateCap(t *testing.T) {
	f := New(Config{API: &fakeAPI{}, PostsPerMinute: 1, Now: func() time.Time { return time.Unix(0, 0) }})
	if _, err := f.PostFileTo(conv, command.OutFile{Name: "a", Data: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.PostFileTo(conv, command.OutFile{Name: "a", Data: []byte("x")}); err == nil || !strings.Contains(err.Error(), "rate cap") {
		t.Fatal(err)
	}
}

func TestUploads(t *testing.T) {
	api := &fakeAPI{}
	f := New(Config{API: api})
	spec := mcphost.UploadSpec{Name: "../big.bin", MimeType: "application/x", Size: 99}
	tgt, err := f.UploadTarget(conv, spec)
	if err != nil || tgt.URL != "https://files.slack.com/upload/v1/X" || tgt.Method != "POST" || tgt.Ref != "F1" || len(tgt.Headers) != 0 {
		t.Fatalf("%+v %v", tgt, err)
	}
	if api.gotGet.FileName != "big.bin" || api.gotGet.FileSize != 99 {
		t.Fatalf("%+v", api.gotGet)
	}
	up, err := f.FinishUpload(conv, spec, "F1")
	if err != nil || up.Ref != "F1" || up.Size != 99 {
		t.Fatalf("%+v %v", up, err)
	}
	if _, err := f.FinishUpload(conv, spec, ""); err == nil {
		t.Fatal("empty ref")
	}
	ids, err := f.PostUploadedTo(conv, up, "cap")
	if err != nil || ids[0] != "F1" || api.done[0].InitialComment != "cap" || api.done[0].Files[0].Title != "../big.bin" {
		t.Fatalf("%v %v %+v", ids, err, api.done)
	}
	if _, err := f.UploadTarget("bad", spec); err == nil {
		t.Fatal("bad conv")
	}
	if _, err := f.PostUploadedTo("bad", up, ""); err == nil {
		t.Fatal("bad conv")
	}
	if _, err := f.UploadTarget(conv, mcphost.UploadSpec{Name: "x", Size: 1 << 32}); err == nil {
		t.Fatal("oversize")
	}
	if _, err := New(Config{API: &fakeAPI{getErr: errors.New("x")}}).UploadTarget(conv, spec); err == nil {
		t.Fatal("get err")
	}
	if _, err := New(Config{API: api, SelfDrive: slackproto.NewSelfDrive("drive-me-9f3a")}).PostUploadedTo(conv, up, "drive-me-9f3a"); err == nil {
		t.Fatal("drive caption")
	}
}

func TestToolsAndRegister(t *testing.T) {
	names := func(cfg ToolsConfig) (n []string) {
		for _, x := range tools(cfg) {
			n = append(n, x.Name)
		}
		return
	}
	if n := names(ToolsConfig{}); len(n) != 0 {
		t.Fatal(n)
	}
	api := &fakeAPI{}
	cfg := ToolsConfig{Files: New(Config{API: api}), Downloads: NewDownloads(DownloadsConfig{}), MaxFileBytes: 10}
	if n := strings.Join(names(cfg), ","); n != "send_file,list_attachments,fetch_attachment" {
		t.Fatal(n)
	}
	// send_file goes through the Broker to Files.
	ts := tools(cfg)
	args, _ := json.Marshal(map[string]any{"file": map[string]any{"id": "f1", "size": 2, "name": "n.txt", "mimeType": "text/plain",
		"data": base64.StdEncoding.EncodeToString([]byte("yo"))}})
	if _, err := ts[0].Call(mcphost.Call{SessionKey: conv, Args: args}); err != nil || api.uploaded != "yo" {
		t.Fatalf("%v %q", err, api.uploaded)
	}
	h, err := mcphost.New(slackmcp.HostConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	Register(h, cfg)
}

func TestServe(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "dl.sock")
	srv, err := Serve("unix:"+sock, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	c := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	resp, err := c.Get("http://x/dl/a")
	if err != nil || resp.StatusCode != 404 {
		t.Fatal(resp, err)
	}
	resp.Body.Close()
	srv.Close()
	srv, err = Serve("127.0.0.1:0", http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if _, err := Serve("256.0.0.1:x", nil); err == nil {
		t.Fatal("bad addr")
	}
}
