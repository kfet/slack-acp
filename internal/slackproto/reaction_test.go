package slackproto

import (
	"context"
	"testing"

	"github.com/slack-go/slack/slackevents"
)

type reactionStub struct {
	stubHandler
	reactions []Reaction
}

func (h *reactionStub) HandleReaction(_ context.Context, r Reaction) {
	h.mu.Lock()
	h.reactions = append(h.reactions, r)
	h.mu.Unlock()
}

func reactionEvent(user, name, itemType string) slackevents.EventsAPIEvent {
	ev := &slackevents.ReactionAddedEvent{User: user, Reaction: name}
	ev.Item.Type, ev.Item.Channel, ev.Item.Timestamp = itemType, "C1", "1.4"
	return slackevents.EventsAPIEvent{
		Type:       slackevents.CallbackEvent,
		InnerEvent: slackevents.EventsAPIInnerEvent{Data: ev},
	}
}

func TestBranchReactionDelivered(t *testing.T) {
	h := &reactionStub{}
	c := newClientForDispatch(t, h)
	ctx := context.Background()
	c.handleEventsAPI(ctx, reactionEvent("U1", BranchReaction, "message"), "")
	c.handleEventsAPI(ctx, reactionEvent("U1", "thumbsup", "message"), "")
	c.handleEventsAPI(ctx, reactionEvent("U1", BranchReaction, "file"), "")
	c.handleEventsAPI(ctx, reactionEvent("Ubot", BranchReaction, "message"), "")
	c.handleEventsAPI(ctx, reactionEvent("", BranchReaction, "message"), "")
	want := Reaction{UserID: "U1", BotUserID: "Ubot", ChannelID: "C1", TS: "1.4"}
	if len(h.reactions) != 1 || h.reactions[0] != want {
		t.Fatalf("reactions = %+v", h.reactions)
	}
	if len(h.seen()) != 0 {
		t.Fatal("a reaction is not a message")
	}
}

func TestBranchReactionWithoutReactionHandler(t *testing.T) {
	h := &stubHandler{}
	c := newClientForDispatch(t, h)
	c.handleEventsAPI(context.Background(), reactionEvent("U1", BranchReaction, "message"), "")
	if len(h.seen()) != 0 {
		t.Fatal("a handler without HandleReaction sees nothing")
	}
}
