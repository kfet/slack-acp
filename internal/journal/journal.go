// Package journal emits the relay's ingest decisions as a stable,
// machine-readable stream — one JSONL line per inbound Slack event,
// recording whether it was delivered onward or dropped, and why.
//
// This exists for two reasons, in this order:
//
//  1. Operations. "The bot didn't answer" is the single most common
//     report, and until now the only way to distinguish "Slack never
//     delivered it", "a guard dropped it", and "the agent abstained"
//     was to turn on debug logging and re-provoke the problem. Every
//     drop is now a positive, timestamped record at normal log level.
//
//  2. Verifiability. A negative test ("this event MUST be dropped")
//     asserted by silence alone is unfalsifiable — indistinguishable
//     from the relay being down or slow. Asserting on the drop record
//     for a specific (channel, ts) turns it into positive evidence.
//     See cmd/slack-acp-verify.
//
// Stability contract: the field names and the Path/Decision/Reason
// vocabularies below are a public interface. Add to them freely;
// renaming or repurposing an existing value is a breaking change for
// anything parsing the stream.
//
// Deliberately absent: message text. The journal records routing
// metadata only, so enabling it can never spill conversation content
// (or a token pasted into a thread) into journald.
package journal

import (
	"encoding/json"
	"io"
	"log"
	"strings"
	"sync/atomic"
)

// Prefix marks a journal line in an otherwise free-form log stream, so
// a consumer can select records with a plain substring match
// (`journalctl … | grep SLACK-ACP-INGEST`) and never has to guess
// whether a line is JSON.
const Prefix = "SLACK-ACP-INGEST "

// Path identifies which inbound Slack surface an event arrived on.
type Path string

// Inbound surfaces. These mirror the Slack event types the relay
// subscribes to; SelfDrive is the bot-authored escape hatch, which
// arrives as a message.* event but is routed distinctly.
const (
	PathAppMention Path = "app_mention"
	PathMessageIM  Path = "message_im"
	PathMessage    Path = "message_channel"
	PathSelfDrive  Path = "self_drive"
)

// Decision is the outcome for an event.
type Decision string

// Outcomes. Deliver/Drop are recorded by the protocol layer; Run/Drop
// again by the handler, which applies the allowlists and the ambient
// gate. An event that is fully processed therefore produces two lines.
const (
	DecisionDeliver Decision = "deliver"
	DecisionDrop    Decision = "drop"
	DecisionRun     Decision = "run"
)

// Stage identifies which layer made the decision.
type Stage string

// Layers.
const (
	StageProto   Stage = "slackproto"
	StageHandler Stage = "handler"
)

// Reason vocabulary. Every emitted record carries exactly one of these.
const (
	// slackproto, app_mention path.
	ReasonBotAuthored = "bot_authored" // self-authorship: our own bot, no author, or an edit
	ReasonAPIAuthored = "api_authored" // carries a bot_id and the author is not a named human
	// ReasonForeignApp: the author IS a named human, but a different
	// app posted on their behalf. Naming a human must not hand trust
	// to every app that person ever installed.
	ReasonForeignApp = "foreign_app"
	// ReasonHumanAuthorRateCap: the reclassification's loop backstop.
	ReasonHumanAuthorRateCap = "human_author_rate_cap"
	ReasonMention            = "mention"

	// slackproto, message.* path.
	ReasonSubType            = "subtype"                 // edit / join / our own chat.update
	ReasonSelfDriveNotAccept = "self_drive_not_accepted" // bot-authored, no leading sentinel
	ReasonSelfPostedTS       = "self_posted_ts"          // echo of a ts we wrote
	ReasonDM                 = "dm"
	ReasonSelfDrive          = "self_drive"
	ReasonAmbientThreadReply = "ambient_thread_reply"
	ReasonNotThreadReply     = "not_thread_reply"  // top-level channel chatter
	ReasonMentionDuplicate   = "mention_duplicate" // app_mention is the canonical copy

	// handler stage.
	ReasonAllowlist          = "allowlist"
	ReasonSelfDriveRateCap   = "self_drive_rate_cap"
	ReasonEmptyText          = "empty_text"
	ReasonAmbientUnknownThrd = "ambient_unknown_thread"
	ReasonPrompt             = "prompt"
)

// Record is one ingest decision. Field names are part of the stability
// contract; see the package doc.
type Record struct {
	Stage    Stage    `json:"stage"`
	Path     Path     `json:"path"`
	Decision Decision `json:"decision"`
	Reason   string   `json:"reason"`
	Channel  string   `json:"channel"`
	TS       string   `json:"ts"`
	ThreadTS string   `json:"thread_ts,omitempty"`
	User     string   `json:"user,omitempty"`
}

// out is the sink. Nil (the default) routes through the standard
// logger, which is what production wants — journald captures it with
// the unit's own timestamps. Tests swap in a buffer.
//
// atomic.Pointer rather than a mutex: this is written once at test
// setup and read on every inbound event.
var out atomic.Pointer[io.Writer]

// SetOutput redirects journal records to w, returning a function that
// restores the previous sink. Intended for tests; production leaves it
// alone and gets log.Printf.
func SetOutput(w io.Writer) func() {
	prev := out.Load()
	if w == nil {
		out.Store(nil)
	} else {
		out.Store(&w)
	}
	return func() { out.Store(prev) }
}

// Log emits one record. Marshalling a Record of plain strings cannot
// fail, so the error is discarded rather than dressed up as a fallible
// path nobody can exercise.
func Log(r Record) {
	b, _ := json.Marshal(r) //nolint:errchkjson // struct of strings; cannot fail
	line := Prefix + string(b)
	if w := out.Load(); w != nil {
		//nolint:errcheck // a test buffer write cannot meaningfully fail
		_, _ = io.WriteString(*w, line+"\n")
		return
	}
	log.Print(line)
}

// Parse extracts a Record from one line of a log stream, reporting
// whether the line was a journal record at all. Lines are matched on
// Prefix anywhere in the line, because journald (and the standard
// logger) prepend their own timestamps.
func Parse(line string) (Record, bool) {
	i := strings.Index(line, Prefix)
	if i < 0 {
		return Record{}, false
	}
	var r Record
	if err := json.Unmarshal([]byte(line[i+len(Prefix):]), &r); err != nil {
		return Record{}, false
	}
	return r, true
}
