package slackproto

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/slack-go/slack"
)

type pagedReplies struct {
	pages   [][]slack.Message
	err     error
	cursors []string
}

func (p *pagedReplies) GetConversationRepliesContext(_ context.Context, params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	if p.err != nil {
		return nil, false, "", p.err
	}
	p.cursors = append(p.cursors, params.Cursor)
	i := len(p.cursors) - 1
	more := i+1 < len(p.pages)
	next := ""
	if more {
		next = fmt.Sprint("c", i+1)
	}
	return p.pages[i], more, next, nil
}

func page(from, to int) []slack.Message {
	var out []slack.Message
	for i := from; i < to; i++ {
		var m slack.Message
		m.Timestamp = fmt.Sprint(i)
		out = append(out, m)
	}
	return out
}

func TestThreadTailKeepsTheEnd(t *testing.T) {
	api := &pagedReplies{pages: [][]slack.Message{page(0, 3), page(3, 6), page(6, 8)}}
	got, err := ThreadTail(context.Background(), api, slack.GetConversationRepliesParameters{ChannelID: "C1", Timestamp: "0"}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].Timestamp != "4" || got[3].Timestamp != "7" {
		t.Fatalf("got %+v", got)
	}
	if fmt.Sprint(api.cursors) != "[ c1 c2]" {
		t.Fatalf("cursors %v", api.cursors)
	}
}

func TestThreadTailBounds(t *testing.T) {
	pages := make([][]slack.Message, maxThreadPages+5)
	for i := range pages {
		pages[i] = page(i, i+1)
	}
	api := &pagedReplies{pages: pages}
	got, _ := ThreadTail(context.Background(), api, slack.GetConversationRepliesParameters{}, 100)
	if len(api.cursors) != maxThreadPages || len(got) != maxThreadPages {
		t.Fatalf("walked %d pages, got %d", len(api.cursors), len(got))
	}
	if _, err := ThreadTail(context.Background(), &pagedReplies{err: errors.New("x")}, slack.GetConversationRepliesParameters{}, 1); err == nil {
		t.Fatal("error must surface")
	}
}
