package slackproto

import (
	"context"
	"log"
	"sync"

	"github.com/slack-go/slack"

	"github.com/kfet/slack-acp/internal/catchup"
	"github.com/kfet/slack-acp/internal/journal"
)

// CatchUpHandler is the optional interface a Handler implements to
// receive the catch-up's events one conversation at a time. A handler
// without it gets them one by one through Handle.
type CatchUpHandler interface {
	CatchUp(ctx context.Context, evs []Event)
}

// WithCatchUp enables the catch-up: every time Socket Mode connects —
// at startup and on every reconnect — it reads what was posted since
// each conversation's checkpoint, and live deliveries advance the
// checkpoints. cfg.API defaults to the client's own Web API client.
// Omit it to run without catch-up.
func WithCatchUp(cfg catchup.Config) Option {
	return func(c *Client) {
		if cfg.API == nil {
			cfg.API = c.api
		}
		c.catchUp = &catchUpState{scanner: catchup.New(cfg)}
	}
}

// catchUpState serialises catch-up runs against live delivery.
//
// While a run is reading history, live events are acknowledged as
// usual but held in pending. When the run ends they are released
// against the checkpoints the run left behind: an event at or below
// its conversation's checkpoint is a message the run already read and
// is dropped; anything newer is delivered. That is what makes a
// message posted around a reconnect arrive exactly once.
type catchUpState struct {
	scanner *catchup.Scanner

	mu      sync.Mutex
	running bool
	rerun   bool // a connect arrived during a run: read again after it
	pending []Event

	wg sync.WaitGroup
}

// WaitCatchUp blocks until no catch-up run is in progress.
func (c *Client) WaitCatchUp() {
	if c.catchUp != nil {
		c.catchUp.wg.Wait()
	}
}

// startCatchUp begins a run, or asks the running one to read again.
func (c *Client) startCatchUp(ctx context.Context) {
	cu := c.catchUp
	if cu == nil {
		return
	}
	cu.mu.Lock()
	defer cu.mu.Unlock()
	if cu.running {
		cu.rerun = true
		return
	}
	cu.running = true
	cu.wg.Add(1)
	go c.runCatchUp(ctx)
}

// deliverLive hands a live event on, holding it while a catch-up runs.
func (c *Client) deliverLive(ctx context.Context, ev Event) {
	if cu := c.catchUp; cu != nil {
		cu.mu.Lock()
		if cu.running {
			cu.pending = append(cu.pending, ev)
			cu.mu.Unlock()
			return
		}
		cu.mu.Unlock()
	}
	c.deliver(ctx, ev)
}

// observe advances channel's checkpoint past a live message, delivered
// or not: a message seen live never needs reading again, and on a
// clamped (1 call/min, 15 per page) installation re-reading a busy
// channel's chatter would make every catch-up slow. Not while a run is
// in flight — the run has not read that channel yet, and moving its
// checkpoint now would skip whatever it is about to read.
func (c *Client) observe(channel, ts string) {
	cu := c.catchUp
	if cu == nil {
		return
	}
	cu.mu.Lock()
	running := cu.running
	cu.mu.Unlock()
	if !running {
		c.advance(channel, ts)
	}
}

func (c *Client) advance(channel, ts string) {
	if err := c.catchUp.scanner.Store().Advance(channel, ts); err != nil {
		log.Printf("slack-acp: catch-up: WARN checkpoint %s: %v", channel, err)
	}
}

func (c *Client) runCatchUp(ctx context.Context) {
	cu := c.catchUp
	defer cu.wg.Done()
	for {
		st, err := cu.scanner.Run(ctx, func(ctx context.Context, conv catchup.Conversation, msgs []slack.Message) {
			var evs []Event
			for _, m := range msgs {
				c.ingestHistory(conv, m, func(ev Event) { evs = append(evs, ev) })
			}
			c.handOff(ctx, evs)
		})
		if err != nil {
			log.Printf("slack-acp: catch-up: WARN %v (%s)", err, st)
		} else {
			log.Printf("slack-acp: catch-up: %s", st)
		}
		if !c.releasePending(ctx) {
			return
		}
	}
}

// releasePending delivers the events held during a run. It reports
// whether another run is due; when it is not, the run is over and
// live delivery is direct again.
func (c *Client) releasePending(ctx context.Context) (again bool) {
	cu := c.catchUp
	store := cu.scanner.Store()
	for {
		cu.mu.Lock()
		held := cu.pending
		cu.pending = nil
		if len(held) == 0 {
			again, cu.rerun = cu.rerun, false
			cu.running = again
			cu.mu.Unlock()
			return again
		}
		cu.mu.Unlock()
		// Snapshot every checkpoint first: delivering one held event
		// advances its checkpoint, and must not make an older held
		// event in the same conversation look already read.
		marks := map[string]string{}
		for _, ev := range held {
			if _, ok := marks[ev.ChannelID]; !ok {
				marks[ev.ChannelID] = store.Mark(ev.ChannelID)
			}
		}
		for _, ev := range held {
			if ev.TS <= marks[ev.ChannelID] {
				journal.Log(journal.Record{
					Stage:    journal.StageProto,
					Path:     ev.JournalPath(),
					Decision: journal.DecisionDrop,
					Reason:   journal.ReasonCatchUpSeen,
					Channel:  ev.ChannelID,
					TS:       ev.TS,
					ThreadTS: ev.ThreadTS,
					User:     ev.UserID,
				})
				continue
			}
			c.deliver(ctx, ev)
			c.advance(ev.ChannelID, ev.TS)
		}
	}
}

// ingestHistory routes one history message through the live gates.
//
// A message that mentions the bot in a channel takes the app_mention
// path — the copy Slack would have delivered live; its message.* twin
// is dropped there as a duplicate anyway. Everything else takes the
// message.* path.
//
// edited is deliberately false: live, the ORIGINAL post was delivered
// and only the later message_changed was refused. Here the original
// was never delivered, and history holds it in its edited form.
func (c *Client) ingestHistory(conv catchup.Conversation, hm slack.Message, emit func(Event)) {
	m := inMsg{
		channel: conv.ID,
		im:      conv.IsIM,
		user:    hm.User,
		botID:   hm.BotID,
		ts:      hm.Timestamp,
		text:    hm.Text,
		subType: hm.SubType,
		files:   hm.Files,
		catchUp: true,
	}
	if hm.BotProfile != nil {
		m.appID = hm.BotProfile.AppID
	}
	// A thread PARENT carries thread_ts == ts in history; live, a
	// top-level post has no thread_ts at all.
	if hm.ThreadTimestamp != hm.Timestamp {
		m.threadTS = hm.ThreadTimestamp
	}
	if !m.im && mentionsBot(m.text, c.botUserID) {
		c.ingestMention(m, emit)
		return
	}
	c.ingestMessage(m, emit)
}

// handOff passes one conversation's catch-up events to the handler.
func (c *Client) handOff(ctx context.Context, evs []Event) {
	if len(evs) == 0 || c.handler == nil {
		return
	}
	for i := range evs {
		evs[i].BotUserID = c.botUserID
		evs[i].TeamID = c.teamID
	}
	if h, ok := c.handler.(CatchUpHandler); ok {
		h.CatchUp(ctx, evs)
		return
	}
	for _, ev := range evs {
		c.handler.Handle(ctx, ev)
	}
}
