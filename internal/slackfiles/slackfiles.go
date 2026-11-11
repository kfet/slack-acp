// Package slackfiles implements acp-kit's optional MCP files
// capabilities for Slack, following acp-kit docs/files-oob.md:
//
//   - command.FilePoster: an inline send_file goes up through
//     files.getUploadURLExternal + files.completeUploadExternal.
//   - command.UploadTargeter (and mcphost.UploadProvider): a large
//     send_file is uploaded by the agent's MCP client STRAIGHT to the
//     pre-signed upload_url Slack returns. That URL needs no auth header,
//     so the bot token never reaches the client. The relay then shares
//     the file into the thread with files.completeUploadExternal.
//   - command.DownloadLinker (Downloads): list_attachments /
//     fetch_attachment. url_private needs the bot token, so it is never
//     handed out; the client gets a one-shot, short-lived link to the
//     relay's own download proxy, which fetches url_private with the bot
//     token and streams the bytes back.
//
// Every capability is bound to the session's own thread: the
// conversation id is the mcphost session key ("channel/thread_ts[/user]"),
// derived server-side from the connection token, so the agent cannot
// address another thread.
package slackfiles

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/mcphost"
	"github.com/slack-go/slack"

	"github.com/kfet/slack-acp/internal/ratelimit"
	"github.com/kfet/slack-acp/internal/slackproto"
)

// CallTimeout bounds the Slack API work of one capability call.
const CallTimeout = 60 * time.Second

// defaultPostsPerMinute caps file sends when Config.PostsPerMinute is 0.
const defaultPostsPerMinute = 10

// API is the subset of *slack.Client the uploads use.
type API interface {
	GetUploadURLExternalContext(ctx context.Context, params slack.GetUploadURLExternalParameters) (*slack.GetUploadURLExternalResponse, error)
	UploadToURL(ctx context.Context, params slack.UploadToURLParameters) error
	CompleteUploadExternalContext(ctx context.Context, params slack.CompleteUploadExternalParameters) (*slack.CompleteUploadExternalResponse, error)
}

// Config configures Files.
type Config struct {
	API API
	// SelfDrive is the operator's self-drive hatch (nil when off). A
	// caption is held to the same guards slack_post uses.
	SelfDrive *slackproto.SelfDrive
	// PostsPerMinute caps file sends across all sessions. 0 → default.
	PostsPerMinute int
	Now            func() time.Time
	Logf           func(format string, v ...any)
}

// Files implements command.FilePoster, command.UploadTargeter and
// mcphost.UploadProvider over Slack's external upload API.
type Files struct {
	cfg   Config
	posts *ratelimit.Bucket
}

// New builds a Files.
func New(cfg Config) *Files {
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Files{cfg: cfg, posts: ratelimit.New(cfg.PostsPerMinute, defaultPostsPerMinute, cfg.Now)}
}

var (
	_ command.FilePoster     = (*Files)(nil)
	_ command.UploadTargeter = (*Files)(nil)
	_ mcphost.UploadProvider = (*Files)(nil)
)

// thread splits a conversation id ("channel/thread_ts[/user]") into the
// thread it names.
func thread(convID string) (channel, ts string, err error) {
	parts := strings.Split(convID, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("conversation %q names no Slack thread", convID)
	}
	return parts[0], parts[1], nil
}

// PostFileTo implements command.FilePoster: one inline file into the
// conversation's thread.
func (f *Files) PostFileTo(convID string, o command.OutFile) ([]string, error) {
	ch, ts, err := thread(convID)
	if err != nil {
		return nil, err
	}
	caption, err := f.admit(o.Caption)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
	defer cancel()
	up, err := f.cfg.API.GetUploadURLExternalContext(ctx, slack.GetUploadURLExternalParameters{FileName: o.Name, FileSize: len(o.Data)})
	if err != nil {
		return nil, fmt.Errorf("files.getUploadURLExternal: %w", err)
	}
	if err := f.cfg.API.UploadToURL(ctx, slack.UploadToURLParameters{UploadURL: up.UploadURL, Reader: bytes.NewReader(o.Data), Filename: o.Name}); err != nil {
		return nil, fmt.Errorf("upload: %w", err)
	}
	return f.complete(ctx, ch, ts, up.FileID, o.Name, caption)
}

// admit applies the caption guards and the rate cap, in that order, so a
// refused caption does not spend a send.
func (f *Files) admit(caption string) (string, error) {
	if _, isDrive := f.cfg.SelfDrive.Accept(caption); isDrive {
		return "", errors.New("refusing a caption beginning with the operator's self-drive sentinel")
	}
	caption = slackproto.StripBroadcastPings(f.cfg.SelfDrive.Scrub(caption))
	if !f.posts.Allow() {
		return "", errors.New("send_file rate cap exceeded; try again shortly")
	}
	return caption, nil
}

// complete shares an uploaded file into the thread.
func (f *Files) complete(ctx context.Context, ch, ts, fileID, title, caption string) ([]string, error) {
	res, err := f.cfg.API.CompleteUploadExternalContext(ctx, slack.CompleteUploadExternalParameters{
		Files:           []slack.FileSummary{{ID: fileID, Title: title}},
		Channel:         ch,
		ThreadTimestamp: ts,
		InitialComment:  caption,
	})
	if err != nil {
		f.cfg.Logf("slack-files: send channel=%s thread_ts=%s file=%s outcome=error err=%v", ch, ts, fileID, err)
		return nil, fmt.Errorf("files.completeUploadExternal: %w", err)
	}
	ids := make([]string, 0, len(res.Files))
	for _, s := range res.Files {
		ids = append(ids, s.ID)
	}
	f.cfg.Logf("slack-files: send channel=%s thread_ts=%s file=%s outcome=ok", ch, ts, fileID)
	return ids, nil
}

// UploadTargetFor implements command.UploadTargeter: a pre-signed Slack
// upload_url, minted server-side with the bot token. The URL itself is
// the capability; no header is needed, so none is sent.
func (f *Files) UploadTargetFor(convID string, spec mcphost.UploadSpec) (mcphost.UploadTarget, error) {
	if _, _, err := thread(convID); err != nil {
		return mcphost.UploadTarget{}, err
	}
	if spec.Size > math.MaxInt32 {
		return mcphost.UploadTarget{}, fmt.Errorf("file is %d bytes; this relay uploads at most %d", spec.Size, math.MaxInt32)
	}
	ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
	defer cancel()
	up, err := f.cfg.API.GetUploadURLExternalContext(ctx, slack.GetUploadURLExternalParameters{
		FileName: command.SafeFileName(spec.Name),
		FileSize: int(spec.Size),
	})
	if err != nil {
		return mcphost.UploadTarget{}, fmt.Errorf("files.getUploadURLExternal: %w", err)
	}
	return mcphost.UploadTarget{URL: up.UploadURL, Method: "POST", Ref: up.FileID}, nil
}

// FinishUploadFor implements command.UploadTargeter. Slack reports no
// size before completion, so the claimed size stands;
// completeUploadExternal fails in PostUploadedTo if the bytes never
// arrived.
func (f *Files) FinishUploadFor(convID string, spec mcphost.UploadSpec, ref string) (mcphost.UploadedFile, error) {
	if ref == "" {
		return mcphost.UploadedFile{}, errors.New("no upload was started for this file")
	}
	return mcphost.UploadedFile{Ref: ref, Name: spec.Name, MimeType: spec.MimeType, Size: spec.Size}, nil
}

// PostUploadedTo implements command.UploadTargeter.
func (f *Files) PostUploadedTo(convID string, up mcphost.UploadedFile, caption string) ([]string, error) {
	ch, ts, err := thread(convID)
	if err != nil {
		return nil, err
	}
	caption, err = f.admit(caption)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
	defer cancel()
	return f.complete(ctx, ch, ts, up.Ref, up.Name, caption)
}

// UploadTarget implements mcphost.UploadProvider. The session key is the
// conversation id.
func (f *Files) UploadTarget(sessionKey string, spec mcphost.UploadSpec) (mcphost.UploadTarget, error) {
	return f.UploadTargetFor(sessionKey, spec)
}

// FinishUpload implements mcphost.UploadProvider.
func (f *Files) FinishUpload(sessionKey string, spec mcphost.UploadSpec, ref string) (mcphost.UploadedFile, error) {
	return f.FinishUploadFor(sessionKey, spec, ref)
}
