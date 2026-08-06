package handler

import (
	"log"

	"github.com/kfet/slack-acp/internal/slackproto"
)

// defaultSelfDrivePerMinute is the hatch's rate cap when unset.
const defaultSelfDrivePerMinute = 4

// admitSelfDrive applies the rate cap to a hatch event and logs the
// decision loudly. Every acceptance is logged: the hatch deliberately
// reopens the bot-message boundary, so an operator must be able to see
// exactly what it let through.
func (h *Handler) admitSelfDrive(ev slackproto.Event) bool {
	if !h.selfDrive.Allow() {
		log.Printf("SELF-DRIVE REFUSED (rate cap %d/min exceeded): channel=%s ts=%s — dropping; check for a reply loop",
			h.cfg.SelfDrivePerMinute, ev.ChannelID, ev.TS)
		return false
	}
	log.Printf("SELF-DRIVE ACCEPTED: channel=%s ts=%s text=%q", ev.ChannelID, ev.TS, truncate(ev.Text, 80))
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
