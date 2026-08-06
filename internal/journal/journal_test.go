package journal

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"
)

func TestLogWritesPrefixedJSONToSink(t *testing.T) {
	var buf bytes.Buffer
	restore := SetOutput(&buf)
	defer restore()

	Log(Record{
		Stage:    StageProto,
		Path:     PathAppMention,
		Decision: DecisionDrop,
		Reason:   ReasonBotAuthored,
		Channel:  "C1",
		TS:       "100.5",
		ThreadTS: "100.0",
		User:     "U1",
	})

	line := buf.String()
	if !strings.HasPrefix(line, Prefix) {
		t.Fatalf("missing prefix: %q", line)
	}
	if !strings.HasSuffix(line, "\n") {
		t.Fatalf("record must be newline-terminated: %q", line)
	}
	var got Record
	if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(line), Prefix)), &got); err != nil {
		t.Fatalf("payload is not JSON: %v (%q)", err, line)
	}
	want := Record{StageProto, PathAppMention, DecisionDrop, ReasonBotAuthored, "C1", "100.5", "100.0", "U1"}
	if got != want {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// TestLogOmitsOptionalFields pins the omitempty behaviour: a record
// with no thread or user must not carry empty strings, so a consumer
// can distinguish "absent" from "empty".
func TestLogOmitsOptionalFields(t *testing.T) {
	var buf bytes.Buffer
	defer SetOutput(&buf)()

	Log(Record{Stage: StageHandler, Path: PathMessage, Decision: DecisionRun, Reason: ReasonPrompt, Channel: "C1", TS: "1.0"})

	if s := buf.String(); strings.Contains(s, "thread_ts") || strings.Contains(s, "user") {
		t.Fatalf("optional empty fields must be omitted: %q", s)
	}
}

// TestLogFallsBackToStandardLogger pins the production sink: with no
// SetOutput override the record goes through log.Print, which is what
// journald captures from the unit.
func TestLogFallsBackToStandardLogger(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	}()

	// Explicitly clear any sink, and confirm the restore func puts the
	// previous one back.
	restore := SetOutput(nil)
	Log(Record{Stage: StageProto, Path: PathMessageIM, Decision: DecisionDeliver, Reason: ReasonDM, Channel: "D1", TS: "2.0"})
	restore()

	if got := buf.String(); !strings.Contains(got, Prefix) || !strings.Contains(got, `"reason":"dm"`) {
		t.Fatalf("standard logger did not receive the record: %q", got)
	}
}

// TestSetOutputRestoresPrevious pins that restore funcs nest, so a
// test helper can divert the journal without clobbering an outer
// diversion.
func TestSetOutputRestoresPrevious(t *testing.T) {
	var outer, inner bytes.Buffer
	restoreOuter := SetOutput(&outer)
	defer restoreOuter()

	restoreInner := SetOutput(&inner)
	Log(Record{Stage: StageProto, Reason: "a"})
	restoreInner()
	Log(Record{Stage: StageProto, Reason: "b"})

	if !strings.Contains(inner.String(), `"reason":"a"`) || strings.Contains(inner.String(), `"reason":"b"`) {
		t.Fatalf("inner sink wrong: %q", inner.String())
	}
	if !strings.Contains(outer.String(), `"reason":"b"`) || strings.Contains(outer.String(), `"reason":"a"`) {
		t.Fatalf("outer sink not restored: %q", outer.String())
	}
}

func TestParse(t *testing.T) {
	rec := Record{Stage: StageHandler, Path: PathSelfDrive, Decision: DecisionRun, Reason: ReasonPrompt, Channel: "C9", TS: "3.0"}
	var buf bytes.Buffer
	defer SetOutput(&buf)()
	Log(rec)
	line := strings.TrimSpace(buf.String())

	// Bare line.
	got, ok := Parse(line)
	if !ok || got != rec {
		t.Fatalf("Parse(bare) = %+v, %v", got, ok)
	}
	// journald/standard-logger prefixed line — the realistic case.
	got, ok = Parse("Aug 06 12:00:00 kopitwo slack-acp[1]: 2026/08/06 12:00:00 " + line)
	if !ok || got != rec {
		t.Fatalf("Parse(journald) = %+v, %v", got, ok)
	}
}

func TestParseRejectsNonRecords(t *testing.T) {
	for name, line := range map[string]string{
		"unrelated log line": "slack-acp: connecting to Slack…",
		"prefix but garbage": Prefix + "{not json",
		"empty":              "",
	} {
		if _, ok := Parse(line); ok {
			t.Errorf("%s: Parse must reject %q", name, line)
		}
	}
}
