package slackfiles

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/mcphost"
	"github.com/slack-go/slack"
)

// LinkTTL is how long a minted download link stays usable.
const LinkTTL = 10 * time.Minute

// maxThreadPages bounds conversations.replies paging for one listing.
const maxThreadPages = 5

// DownloadPath prefixes every proxy link: <base>/dl/<token>.
const DownloadPath = "/dl/"

// RepliesAPI is the subset of *slack.Client the downloads use.
type RepliesAPI interface {
	GetConversationRepliesContext(ctx context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error)
}

// DownloadsConfig configures Downloads.
type DownloadsConfig struct {
	API RepliesAPI
	// Client fetches url_private; it authenticates as the bot and must
	// not follow redirects (see handler.NewFileClient).
	Client *http.Client
	// BaseURL is the public https URL that reaches Handler.
	BaseURL string
	Now     func() time.Time
	Logf    func(format string, v ...any)
}

type link struct {
	url, name, mime string
	size            int64
	exp             time.Time
}

// Downloads implements command.DownloadLinker with a relay-hosted,
// one-shot download proxy (ServeHTTP).
type Downloads struct {
	cfg DownloadsConfig

	mu    sync.Mutex
	links map[string]link // token -> pending download
}

var _ command.DownloadLinker = (*Downloads)(nil)

// NewDownloads builds a Downloads.
func NewDownloads(cfg DownloadsConfig) *Downloads {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Downloads{cfg: cfg, links: map[string]link{}}
}

// files lists the thread's downloadable files, newest first.
func (d *Downloads) files(convID string) ([]slack.File, error) {
	ch, ts, err := thread(convID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), CallTimeout)
	defer cancel()
	var out []slack.File
	cursor := ""
	for range maxThreadPages {
		msgs, more, next, err := d.cfg.API.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
			ChannelID: ch, Timestamp: ts, Cursor: cursor, Limit: 200,
		})
		if err != nil {
			return nil, fmt.Errorf("conversations.replies: %w", err)
		}
		for _, m := range msgs {
			for _, f := range m.Files {
				if f.ID != "" && f.URLPrivate != "" {
					out = append(out, f)
				}
			}
		}
		if !more || next == "" {
			break
		}
		cursor = next
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// AttachmentsIn implements command.DownloadLinker.
func (d *Downloads) AttachmentsIn(convID string) ([]command.InboundAttachment, error) {
	fs, err := d.files(convID)
	if err != nil {
		return nil, err
	}
	out := make([]command.InboundAttachment, len(fs))
	for i, f := range fs {
		out[i] = command.InboundAttachment{ID: f.ID, Name: f.Name, MimeType: f.Mimetype, Size: int64(f.Size)}
	}
	return out, nil
}

// DownloadLinkFor implements command.DownloadLinker. Only a file in the
// conversation's own thread can be linked: the id is looked up in the
// thread, never passed to files.info.
func (d *Downloads) DownloadLinkFor(convID, attachmentID string) (mcphost.DownloadTarget, error) {
	fs, err := d.files(convID)
	if err != nil {
		return mcphost.DownloadTarget{}, err
	}
	for _, f := range fs {
		if f.ID != attachmentID {
			continue
		}
		tok := rand.Text() // 128+ bits
		exp := d.cfg.Now().Add(LinkTTL)
		d.mu.Lock()
		for k, l := range d.links {
			if !d.cfg.Now().Before(l.exp) {
				delete(d.links, k)
			}
		}
		d.links[tok] = link{url: f.URLPrivate, name: f.Name, mime: f.Mimetype, size: int64(f.Size), exp: exp}
		d.mu.Unlock()
		d.cfg.Logf("slack-files: link conv=%s file=%s", convID, f.ID)
		return mcphost.DownloadTarget{
			URL: d.cfg.BaseURL + DownloadPath + tok, Name: f.Name, MimeType: f.Mimetype,
			Size: int64(f.Size), ExpiresAt: exp,
		}, nil
	}
	return mcphost.DownloadTarget{}, fmt.Errorf("no attachment %q in this conversation", attachmentID)
}

// take consumes a token: every link is single-use.
func (d *Downloads) take(tok string) (link, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	l, ok := d.links[tok]
	delete(d.links, tok)
	if !ok || !d.cfg.Now().Before(l.exp) {
		return link{}, false
	}
	return l, true
}

// ServeHTTP is the download proxy. It forwards none of the client's
// headers; the bot token is added only by Client, on the relay → Slack
// hop.
func (d *Downloads) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tok, ok := strings.CutPrefix(r.URL.Path, DownloadPath)
	if !ok || tok == "" || strings.Contains(tok, "/") {
		http.NotFound(w, r)
		return
	}
	l, ok := d.take(tok)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if !slackFileURL(l.url) {
		d.fail(w, "refusing a file URL outside https://*.slack.com")
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, l.url, nil) // URL parsed above
	resp, err := d.cfg.Client.Do(req)
	if err != nil {
		d.fail(w, "fetch: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		d.fail(w, "slack answered "+resp.Status)
		return
	}
	if resp.ContentLength >= 0 && resp.ContentLength != l.size {
		d.fail(w, fmt.Sprintf("slack sent %d bytes, file is %d", resp.ContentLength, l.size))
		return
	}
	h := w.Header()
	h.Set("Content-Type", l.mime)
	if l.mime == "" {
		h.Set("Content-Type", "application/octet-stream")
	}
	h.Set("Content-Length", strconv.FormatInt(l.size, 10))
	h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": command.SafeFileName(l.name)}))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	n, err := io.CopyN(w, resp.Body, l.size)
	if err != nil {
		d.cfg.Logf("slack-files: download short after %d of %d bytes: %v", n, l.size, err)
		return
	}
	d.cfg.Logf("slack-files: download ok bytes=%d", n)
}

func (d *Downloads) fail(w http.ResponseWriter, why string) {
	d.cfg.Logf("slack-files: download failed: %s", why)
	http.Error(w, "upstream fetch failed", http.StatusBadGateway)
}

// slackFileURL reports whether raw is an https URL on a Slack host: the
// bot token rides on that request, so it may go nowhere else.
func slackFileURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	h := u.Hostname()
	return h == "slack.com" || strings.HasSuffix(h, ".slack.com")
}
