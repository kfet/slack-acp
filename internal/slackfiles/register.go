package slackfiles

import (
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/mcphost"
	"github.com/kfet/acp-kit/relaytool"
)

// ToolsConfig selects which files tools Register installs.
type ToolsConfig struct {
	// Files backs send_file (inline and out-of-band). nil: no send_file.
	Files *Files
	// Downloads backs list_attachments / fetch_attachment. nil: none.
	Downloads *Downloads
	// MaxFileBytes caps one send_file. 0 → relaytool default.
	MaxFileBytes int64
	Logf         func(format string, v ...any)
}

// fileTools are the relaytool tools this relay takes from acp-kit; the
// rest (status, post, ...) overlap the relay's own `slack` tools.
var fileTools = map[string]bool{
	relaytool.ToolSendFile:        true,
	relaytool.ToolListAttachments: true,
	relaytool.ToolFetchAttachment: true,
}

// Register installs acp-kit's send_file / list_attachments /
// fetch_attachment tools on h, backed by cfg, and the out-of-band upload
// provider when Files is set. Session keys are the conversation ids.
func Register(h *mcphost.Host, cfg ToolsConfig) {
	if cfg.Files != nil {
		h.SetUploads(cfg.Files)
	}
	for _, x := range tools(cfg) {
		h.ToolCall(x.Name, x.Description, x.Schema, x.Call)
	}
}

// tools builds the files tools for cfg.
func tools(cfg ToolsConfig) []relaytool.Tool {
	b := command.New(nil)
	if cfg.Files != nil {
		b.SetFilePoster(cfg.Files)
		b.SetUploadTargeter(cfg.Files)
	}
	if cfg.Downloads != nil {
		b.SetDownloadLinker(cfg.Downloads)
	}
	// New fails only without a Broker, and b is always set.
	t, _ := relaytool.New(relaytool.Config{Broker: b, Logf: cfg.Logf, MaxFileBytes: cfg.MaxFileBytes})
	var out []relaytool.Tool
	for _, x := range t.Tools() {
		if fileTools[x.Name] {
			out = append(out, x)
		}
	}
	return out
}

// Serve starts the download proxy on addr: "unix:/path" for a unix
// socket (a stale one is replaced), anything else as a TCP address. The
// caller closes the returned server.
func Serve(addr string, h http.Handler) (*http.Server, error) {
	network := "tcp"
	if p, ok := strings.CutPrefix(addr, "unix:"); ok {
		network, addr = "unix", p
		_ = os.Remove(p)
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return srv, nil
}
