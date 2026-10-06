package slackmcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/slack-go/slack"

	"github.com/kfet/slack-acp/internal/slackproto"
)

// Origin locates the thread a conversation was branched out of.
type Origin struct {
	ChannelID string
	ThreadTS  string
	// BranchTS is the branch point; history(origin=true) stops there.
	BranchTS string
}

// History reads the caller's own thread, or with origin the thread it
// was branched out of, clamped at the branch point.
//
// The channel allowlist is NOT consulted: the session key names a
// thread this session is already conversing in, and its origin is a
// thread in the same channel that a person (or the agent through
// `branch`) explicitly branched out of. Neither can be steered at any
// other conversation. The read rate cap still applies.
func (r *Relay) History(ctx context.Context, sessionKey string, origin bool, limit int) (string, error) {
	channel, threadTS, ok := strings.Cut(sessionKey, "/")
	if !ok || channel == "" || threadTS == "" {
		return "", r.fail(ToolHistory, sessionKey, "", fmt.Errorf("session %q is not a Slack thread", sessionKey))
	}
	params := slack.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: threadTS,
		Inclusive: true,
	}
	which := "this thread"
	if origin {
		o, found := Origin{}, false
		if r.cfg.Origin != nil {
			o, found = r.cfg.Origin(sessionKey)
		}
		if !found {
			return "", r.fail(ToolHistory, sessionKey, channel, errors.New(
				"there is no origin thread to read: this thread was not branched out of another one. "+
					"Call history without origin to read this thread instead"))
		}
		params.ChannelID, params.Timestamp, params.Latest = o.ChannelID, o.ThreadTS, o.BranchTS
		channel, which = o.ChannelID, "the origin thread"
	}
	if err := r.admitRead(ToolHistory, sessionKey, channel); err != nil {
		return "", err
	}
	// The newest messages — up to the branch point for the origin —
	// are the ones that matter; a single page would be the oldest.
	msgs, err := slackproto.ThreadTail(ctx, r.cfg.API, params, clampLimit(limit))
	if err != nil {
		return "", r.fail(ToolHistory, sessionKey, channel, fmt.Errorf("conversations.replies: %w", err))
	}
	if origin {
		// Slack's `latest` bound is advisory for the parent message;
		// enforce the branch point here so nothing after it leaks.
		kept := msgs[:0]
		for _, m := range msgs {
			if m.Timestamp <= params.Latest {
				kept = append(kept, m)
			}
		}
		msgs = kept
	}
	out := r.render(ctx, msgs)
	r.cfg.Logf("slack-mcp: tool=%s session=%s channel=%s thread_ts=%s outcome=ok messages=%d (%s)",
		ToolHistory, sessionKey, channel, params.Timestamp, len(out), which)
	return encode(out)
}
