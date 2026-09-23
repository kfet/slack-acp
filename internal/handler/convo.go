package handler

import (
	"context"
	"strings"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/kfet/acp-kit/client"
	"github.com/kfet/acp-kit/convo"
	kitlog "github.com/kfet/acp-kit/log"
	"github.com/kfet/slack-acp/internal/router"
	"github.com/kfet/slack-acp/internal/slackproto"
)

// newConvo builds the handler's acp-kit convo Manager: the standard
// relay commands in front of every turn, and a Supersede turn runner
// behind them.
func (h *Handler) newConvo() (*convo.Manager, error) {
	var agent convo.Agent = noAgent{}
	var sessions convo.Sessions
	if h.cfg.Router != nil {
		agent, sessions = h.cfg.Router.Agent(), routerSessions{h.cfg.Router}
	}
	return convo.New(convo.Config{
		Agent:    agent,
		Sessions: sessions,
		Mode:     convo.Supersede,
		Liveness: convo.ProgressClock{
			NoProgressTimeout: h.cfg.NoProgressTimeout,
			MaxTurnDuration:   h.cfg.TurnCeiling,
		},
		Sink: convo.SinkFunc(h.reply),
		Run: func(ctx context.Context, _ *convo.Turn, in *convo.In, prompt string) error {
			ev := in.Meta.(slackproto.Event)
			return h.run(ctx, ev, router.ConvKey{ChannelID: ev.ChannelID, ThreadTS: ev.ThreadTS}, prompt)
		},
		OnError: func(_ string, err error) { kitlog.Debugf("handler: prompt error: %v", err) },
		Logf:    kitlog.Debugf,
		Now:     h.cfg.Now,
	})
}

// reply posts a command's answer into the thread it came from. It goes
// through a PostStreamer like any turn's output, so the self-drive
// scrub and own-ts memory apply to it too.
func (h *Handler) reply(ctx context.Context, in *convo.In, text string) error {
	ev := in.Meta.(slackproto.Event)
	stream := slackproto.NewPostStreamer(h.cfg.API, ev.ChannelID, ev.ThreadTS)
	stream.SetSelfDrive(h.cfg.SelfDrive)
	if err := stream.Append(ctx, toMrkdwn(text)); err != nil {
		return err
	}
	return stream.Close(ctx, "")
}

// toMrkdwn adapts the broker's CommonMark-ish replies to Slack mrkdwn,
// whose bold is a single asterisk.
func toMrkdwn(s string) string { return strings.ReplaceAll(s, "**", "*") }

// routerSessions adapts the router's thread-keyed session map to
// convo.Sessions, whose conversation id is ConvKey.String().
type routerSessions struct{ r *router.Router }

func parseKey(conv string) router.ConvKey {
	ch, ts, _ := strings.Cut(conv, "/")
	return router.ConvKey{ChannelID: ch, ThreadTS: ts}
}

func (s routerSessions) Live(conv string) (acp.SessionId, time.Time, bool) {
	return s.r.Live(parseKey(conv))
}
func (s routerSessions) Cancel(ctx context.Context, conv string) { s.r.Cancel(ctx, parseKey(conv)) }
func (s routerSessions) Len() int                                { return s.r.Len() }
func (s routerSessions) Reset(conv string) error {
	s.r.Reset(parseKey(conv))
	return nil
}

// noAgent stands in for a handler built without a router (tests that
// only exercise the ingress gates).
type noAgent struct{}

func (noAgent) Models() ([]client.ModelInfo, string)    { return nil, "" }
func (noAgent) AvailableCommands() []client.CommandInfo { return nil }
