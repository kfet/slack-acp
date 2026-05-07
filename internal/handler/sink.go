package handler

import (
	"context"
	"strings"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/slack-acp/internal/slackproto"
)

// streamingSink converts ACP session updates to Slack streaming text.
type streamingSink struct {
	stream *slackproto.PostStreamer
}

func newStreamingSink(s *slackproto.PostStreamer) *streamingSink {
	return &streamingSink{stream: s}
}

func (s *streamingSink) OnUpdate(ctx context.Context, n acp.SessionNotification) error {
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		if t := contentBlockText(u.AgentMessageChunk.Content); t != "" {
			return s.stream.Append(ctx, t)
		}
	case u.AgentThoughtChunk != nil:
		// Render thoughts in italics, kept compact.
		if t := contentBlockText(u.AgentThoughtChunk.Content); t != "" {
			return s.stream.Append(ctx, "_"+oneLine(t)+"_\n")
		}
	case u.ToolCall != nil:
		title := ""
		if u.ToolCall.Title != "" {
			title = u.ToolCall.Title
		}
		return s.stream.Append(ctx, "\n› `"+title+"`\n")
	case u.ToolCallUpdate != nil:
		// Optional: surface terminal status of tool calls.
		if u.ToolCallUpdate.Status != nil {
			st := string(*u.ToolCallUpdate.Status)
			if st == "completed" || st == "failed" {
				return s.stream.Append(ctx, "  _"+st+"_\n")
			}
		}
	case u.Plan != nil:
		// Render a short plan block.
		var b strings.Builder
		b.WriteString("\n*Plan:*\n")
		for _, e := range u.Plan.Entries {
			b.WriteString("• " + e.Content + "\n")
		}
		return s.stream.Append(ctx, b.String())
	}
	return nil
}

func contentBlockText(c acp.ContentBlock) string {
	if c.Text != nil {
		return c.Text.Text
	}
	return ""
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
