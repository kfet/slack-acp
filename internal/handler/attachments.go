// Attachments in and out of a Slack thread.
//
// Inbound: files a human shares with a message are downloaded (with the
// bot token, which needs files:read) into <session cwd>/inbox/<ts>/ and
// handed to the agent as ACP resource_link blocks plus a one-line note
// naming the local paths. Files persist with the thread's cwd, like the
// rest of its state. Every failure degrades to a note: a turn never
// fails because a download did.
//
// Outbound: at the end of a turn every regular file the agent left in
// <session cwd>/outbox/ is uploaded into the thread (files:write) and
// moved to outbox/.sent/ so the next turn does not re-upload it.
package handler

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/attachments"
	"github.com/slack-go/slack"

	kitlog "github.com/kfet/acp-kit/log"
	"github.com/kfet/slack-acp/internal/slackproto"
)

const (
	// InboxDir receives inbound attachments, relative to the session cwd.
	InboxDir = "inbox"
	// OutboxDir is where the agent leaves files to upload.
	OutboxDir = "outbox"
	// sentDir (inside OutboxDir) holds files already uploaded.
	sentDir = ".sent"
	// DefaultMaxAttachmentBytes caps one inbound file.
	DefaultMaxAttachmentBytes int64 = 20 << 20
	// maxAttachments bounds how many files one message can pull in.
	maxAttachments = 10
	// attachTimeout bounds all downloads for one message.
	attachTimeout = 60 * time.Second
	// uploadTimeout bounds one outbox upload; the session is locked
	// while it runs.
	uploadTimeout = 2 * time.Minute
)

// bearer adds the bot token to every request it carries, so the client
// using it must never follow a redirect (see NewFileClient).
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

// NewFileClient returns the HTTP client inbound downloads use: it
// authenticates as the bot. Redirects are not followed: the transport
// would re-attach the token to whatever host one points at.
func NewFileClient(botToken string) *http.Client {
	return &http.Client{
		Transport:     bearer{token: botToken, next: http.DefaultTransport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// ingest downloads ev's files into cwd/inbox and returns the prompt
// blocks to append after the text, plus a note for the text itself.
// progress is called after each file so downloads, which run on the
// turn's liveness clock, do not read as a wedged agent.
func (h *Handler) ingest(ctx context.Context, cwd string, ev slackproto.Event, progress func()) (string, []acp.ContentBlock) {
	if len(ev.Files) == 0 {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, attachTimeout)
	defer cancel()
	store := attachments.Store{DirName: InboxDir, MaxBytes: h.maxAttachmentBytes(), Client: h.cfg.FileClient}
	used := map[string]struct{}{}
	var (
		blocks []acp.ContentBlock
		saved  []string
		failed []string
	)
	for i, f := range ev.Files {
		progress()
		if i >= maxAttachments {
			failed = append(failed, fmt.Sprintf("%s (over the %d-file limit)", f.Name, maxAttachments))
			continue
		}
		if int64(f.Size) > store.MaxBytes {
			failed = append(failed, fmt.Sprintf("%s (%d bytes, over the %d-byte cap)", f.Name, f.Size, store.MaxBytes))
			continue
		}
		got, err := store.Download(ctx, cwd, ev.TS, used, attachments.Attachment{URL: f.URL, Name: f.Name, ContentType: f.Mimetype})
		if err != nil {
			kitlog.Debugf("handler: attachment %s: %v", f.ID, err)
			failed = append(failed, fmt.Sprintf("%s (%v)", f.Name, err))
			continue
		}
		blocks = append(blocks, got.ResourceLinkBlock())
		saved = append(saved, got.Path)
	}
	progress()
	var note strings.Builder
	if len(saved) > 0 {
		note.WriteString("\n\n[relay] Attached files were saved to: " + strings.Join(saved, ", "))
	}
	if len(failed) > 0 {
		note.WriteString("\n\n[relay] Attached files that could not be fetched: " + strings.Join(failed, "; "))
	}
	return note.String(), blocks
}

func (h *Handler) maxAttachmentBytes() int64 {
	if h.cfg.MaxAttachmentBytes <= 0 {
		return DefaultMaxAttachmentBytes
	}
	return h.cfg.MaxAttachmentBytes
}

// uploadOutbox uploads every regular, non-hidden file in cwd/outbox to
// the thread, in name order. End of turn only: uploading earlier would
// race the agent still writing the file.
func (h *Handler) uploadOutbox(ctx context.Context, cwd, channel, threadTS string) {
	dir := filepath.Join(cwd, OutboxDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			kitlog.Debugf("handler: reading outbox %s: %v", dir, err)
		}
		return
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if err := h.uploadOne(ctx, dir, name, channel, threadTS); err != nil {
			kitlog.Debugf("handler: uploading %s: %v", name, err)
		}
	}
}

func (h *Handler) uploadOne(ctx context.Context, dir, name, channel, threadTS string) error {
	path := filepath.Join(dir, name)
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Size() == 0 {
		// files.getUploadURLExternal rejects empty files; leave it be
		// rather than retrying it every turn.
		return moveSent(dir, name)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	_, err = h.cfg.API.UploadFileContext(ctx, slack.UploadFileParameters{
		Reader:          f,
		Filename:        name,
		Title:           name,
		FileSize:        int(st.Size()),
		Channel:         channel,
		ThreadTimestamp: threadTS,
	})
	_ = f.Close()
	if err != nil {
		return err
	}
	return moveSent(dir, name)
}

func moveSent(dir, name string) error {
	sent := filepath.Join(dir, sentDir)
	if err := os.MkdirAll(sent, 0o755); err != nil {
		return err
	}
	return os.Rename(filepath.Join(dir, name), filepath.Join(sent, name))
}
