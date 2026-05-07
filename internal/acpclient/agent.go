// Package acpclient wraps acp-go-sdk's low-level Connection and manages a
// single stdio child agent process (e.g. `fir --mode acp`, claude-code, etc.).
//
// One AgentProc runs one ACP child process. It can serve many sessions
// concurrently — each NewSession registers a per-session sink that
// receives the stream of session/update notifications.
package acpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

// SessionUpdateSink receives streaming updates for a single ACP session.
type SessionUpdateSink interface {
	OnUpdate(ctx context.Context, n acp.SessionNotification) error
}

// PermissionPolicy decides how to respond to session/request_permission.
type PermissionPolicy interface {
	Decide(ctx context.Context, req acp.RequestPermissionRequest) acp.RequestPermissionResponse
}

// Caps captures the agent capabilities we care about.
type Caps struct {
	// LoadSession is the standard agentCapabilities.loadSession bool.
	// Currently parsed but unused: see Roadmap in docs/design.md for the
	// fallback path that would call session/load when list/resume aren't
	// advertised.
	LoadSession bool
	// ListSessions reflects agentCapabilities.sessionCapabilities.list
	// (unstable RFD).
	ListSessions bool
	// ResumeSession reflects agentCapabilities.sessionCapabilities.resume
	// (unstable RFD).
	ResumeSession   bool
	EmbeddedContext bool
}

// SessionInfo is one entry from a session/list response.
type SessionInfo struct {
	SessionId string  `json:"sessionId"`
	Cwd       string  `json:"cwd,omitempty"`
	Title     *string `json:"title,omitempty"`
	UpdatedAt string  `json:"updatedAt,omitempty"`
}

// listSessionsRequest mirrors the unstable RFD for session/list.
type listSessionsRequest struct {
	Cwd string `json:"cwd,omitempty"`
}

type listSessionsResponse struct {
	Sessions []SessionInfo `json:"sessions"`
}

// resumeSessionRequest mirrors the unstable RFD for session/resume.
type resumeSessionRequest struct {
	SessionId  string          `json:"sessionId"`
	Cwd        string          `json:"cwd,omitempty"`
	McpServers []acp.McpServer `json:"mcpServers,omitempty"`
}

// Config configures an AgentProc.
type Config struct {
	// Command is the argv used to spawn the agent.
	Command []string
	// Cwd is the working directory for the child process.
	Cwd string
	// Env is the environment for the child. If nil, os.Environ() is used.
	Env []string
	// Policy decides permission responses.
	Policy PermissionPolicy
	// Stderr is where the child's stderr is forwarded. If nil, os.Stderr.
	Stderr io.Writer
}

// AgentProc wraps a single stdio-connected ACP agent process.
type AgentProc struct {
	cfg Config

	cmd  *exec.Cmd
	conn *acp.Connection
	caps Caps

	mu    sync.Mutex
	sinks map[acp.SessionId]SessionUpdateSink
}

// pipeFns is the indirection used by Start to acquire the agent's
// stdio. exec.Cmd.{Stdin,Stdout}Pipe error only when the corresponding
// stream is already set, which Start never does — making those branches
// structurally unreachable in production. Tests override these hooks to
// exercise the error paths.
var (
	stdinPipeFn  = (*exec.Cmd).StdinPipe
	stdoutPipeFn = (*exec.Cmd).StdoutPipe
)

// Start launches the agent process and performs Initialize.
func Start(ctx context.Context, cfg Config) (*AgentProc, error) {
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("acpclient: empty Command")
	}
	if cfg.Policy == nil {
		return nil, fmt.Errorf("acpclient: nil Policy")
	}
	if cfg.Cwd == "" {
		cfg.Cwd = os.TempDir()
	}

	cmd := exec.CommandContext(ctx, cfg.Command[0], cfg.Command[1:]...) //nolint:gosec
	cmd.Dir = cfg.Cwd
	if cfg.Env != nil {
		cmd.Env = cfg.Env
	}
	if cfg.Stderr != nil {
		cmd.Stderr = cfg.Stderr
	} else {
		cmd.Stderr = os.Stderr
	}
	stdin, err := stdinPipeFn(cmd)
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := stdoutPipeFn(cmd)
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}

	return connect(ctx, cfg, cmd, stdin, stdout)
}

// connect performs the post-spawn ACP handshake on a pre-built pair of
// pipes. Extracted from Start so tests can drive it over an in-process
// io.Pipe instead of launching a real subprocess. cmd may be nil — in
// that case Close becomes a no-op.
func connect(ctx context.Context, cfg Config, cmd *exec.Cmd, stdin io.WriteCloser, stdout io.Reader) (*AgentProc, error) {
	a := &AgentProc{
		cfg:   cfg,
		cmd:   cmd,
		sinks: make(map[acp.SessionId]SessionUpdateSink),
	}
	a.conn = acp.NewConnection(a.dispatch, stdin, stdout)

	initParams := acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Fs: acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
		},
	}
	raw, err := acp.SendRequest[json.RawMessage](a.conn, ctx, acp.AgentMethodInitialize, initParams)
	if err != nil {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return nil, fmt.Errorf("acp initialize: %w", err)
	}
	a.caps = parseCaps(raw)
	return a, nil
}

func parseCaps(raw json.RawMessage) Caps {
	var env struct {
		AgentCapabilities struct {
			LoadSession         bool `json:"loadSession"`
			SessionCapabilities struct {
				List   *json.RawMessage `json:"list"`
				Resume *json.RawMessage `json:"resume"`
			} `json:"sessionCapabilities"`
			PromptCapabilities struct {
				EmbeddedContext bool `json:"embeddedContext"`
			} `json:"promptCapabilities"`
		} `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(raw, &env)
	return Caps{
		LoadSession:     env.AgentCapabilities.LoadSession,
		ListSessions:    env.AgentCapabilities.SessionCapabilities.List != nil,
		ResumeSession:   env.AgentCapabilities.SessionCapabilities.Resume != nil,
		EmbeddedContext: env.AgentCapabilities.PromptCapabilities.EmbeddedContext,
	}
}

// Caps returns the agent's advertised capabilities.
func (a *AgentProc) Caps() Caps { return a.caps }

// NewSession creates a new ACP session and wires the given sink.
func (a *AgentProc) NewSession(ctx context.Context, cwd string, sink SessionUpdateSink) (acp.SessionId, error) {
	resp, err := acp.SendRequest[acp.NewSessionResponse](a.conn, ctx, acp.AgentMethodSessionNew, acp.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	a.sinks[resp.SessionId] = sink
	a.mu.Unlock()
	return resp.SessionId, nil
}

// ListSessions calls the unstable session/list. Caller must check
// Caps().ListSessions first; on agents that don't advertise the cap the
// agent will reject the method.
func (a *AgentProc) ListSessions(ctx context.Context, cwd string) ([]SessionInfo, error) {
	resp, err := acp.SendRequest[listSessionsResponse](a.conn, ctx, "session/list", listSessionsRequest{Cwd: cwd})
	if err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

// ResumeSession calls the unstable session/resume and registers the sink
// for the resumed session. Caller must check Caps().ResumeSession first.
// sid is the agent-returned identifier (as listed by ListSessions).
func (a *AgentProc) ResumeSession(ctx context.Context, cwd string, sid acp.SessionId, sink SessionUpdateSink) error {
	_, err := acp.SendRequest[json.RawMessage](a.conn, ctx, "session/resume", resumeSessionRequest{
		SessionId:  string(sid),
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.sinks[sid] = sink
	a.mu.Unlock()
	return nil
}

// Prompt sends a user message to the session. Returns the stop reason.
func (a *AgentProc) Prompt(ctx context.Context, sid acp.SessionId, prompt []acp.ContentBlock) (acp.StopReason, error) {
	resp, err := acp.SendRequest[acp.PromptResponse](a.conn, ctx, acp.AgentMethodSessionPrompt, acp.PromptRequest{
		SessionId: sid,
		Prompt:    prompt,
	})
	if err != nil {
		return "", err
	}
	return resp.StopReason, nil
}

// Cancel requests cancellation of an in-flight prompt.
func (a *AgentProc) Cancel(ctx context.Context, sid acp.SessionId) error {
	return a.conn.SendNotification(ctx, acp.AgentMethodSessionCancel, acp.CancelNotification{SessionId: sid})
}

// DropSession removes the sink for a session.
func (a *AgentProc) DropSession(sid acp.SessionId) {
	a.mu.Lock()
	delete(a.sinks, sid)
	a.mu.Unlock()
}

// RebindSink replaces the sink for an existing session id. Used when a
// new prompt arrives for a known thread and the previous sink is no longer
// the right destination.
func (a *AgentProc) RebindSink(sid acp.SessionId, sink SessionUpdateSink) {
	a.mu.Lock()
	a.sinks[sid] = sink
	a.mu.Unlock()
}

// closeGrace is the time we wait for the agent to exit after sending
// SIGINT before escalating to SIGKILL. Variable so tests can override.
var closeGrace = 2 * time.Second

// Close terminates the agent process.
func (a *AgentProc) Close() error {
	if a.cmd == nil || a.cmd.Process == nil {
		return nil
	}
	_ = a.cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- a.cmd.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(closeGrace):
		_ = a.cmd.Process.Kill()
		<-done
		return nil
	}
}

// ---- Inbound dispatch ----

func (a *AgentProc) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
	switch method {
	case acp.ClientMethodSessionUpdate:
		var p acp.SessionNotification
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}
		if s := a.sinkFor(p.SessionId); s != nil {
			if err := s.OnUpdate(ctx, p); err != nil {
				return nil, acp.NewInternalError(map[string]any{"error": err.Error()})
			}
		}
		return nil, nil
	case acp.ClientMethodSessionRequestPermission:
		var p acp.RequestPermissionRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}
		return a.cfg.Policy.Decide(ctx, p), nil
	case acp.ClientMethodFsReadTextFile:
		var p acp.ReadTextFileRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}
		resp, err := readTextFile(p)
		if err != nil {
			return nil, acp.NewInternalError(map[string]any{"error": err.Error()})
		}
		return resp, nil
	case acp.ClientMethodFsWriteTextFile:
		var p acp.WriteTextFileRequest
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}
		if err := writeTextFile(p); err != nil {
			return nil, acp.NewInternalError(map[string]any{"error": err.Error()})
		}
		return acp.WriteTextFileResponse{}, nil
	default:
		return nil, acp.NewMethodNotFound(method)
	}
}

func (a *AgentProc) sinkFor(sid acp.SessionId) SessionUpdateSink {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sinks[sid]
}

func readTextFile(params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	if !filepath.IsAbs(params.Path) {
		return acp.ReadTextFileResponse{}, fmt.Errorf("path must be absolute: %s", params.Path)
	}
	b, err := os.ReadFile(params.Path) //nolint:gosec
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}
	content := string(b)
	if params.Line != nil || params.Limit != nil {
		lines := strings.Split(content, "\n")
		start := 0
		if params.Line != nil && *params.Line > 0 {
			start = *params.Line - 1
			if start > len(lines) {
				start = len(lines)
			}
		}
		end := len(lines)
		if params.Limit != nil && *params.Limit > 0 && start+*params.Limit < end {
			end = start + *params.Limit
		}
		content = strings.Join(lines[start:end], "\n")
	}
	return acp.ReadTextFileResponse{Content: content}, nil
}

func writeTextFile(params acp.WriteTextFileRequest) error {
	if !filepath.IsAbs(params.Path) {
		return fmt.Errorf("path must be absolute: %s", params.Path)
	}
	if err := os.MkdirAll(filepath.Dir(params.Path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(params.Path, []byte(params.Content), 0o644) //nolint:gosec
}
