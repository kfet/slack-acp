package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kfet/acp-kit/client"
)

func TestTurnLeafContainingTurn(t *testing.T) {
	r, _ := newTestRouter(t)
	k := ConvKey{ChannelID: "C1", ThreadTS: "100.000001"}
	if got := r.TurnLeaf(k, "100.5"); got != "" {
		t.Fatalf("no turns: %q", got)
	}
	for _, tr := range []Turn{
		{PromptTS: "100.000001", ReplyTS: "100.000002", Leaf: "a"},
		{PromptTS: "", ReplyTS: "", Leaf: "dropped"},        // no ts
		{PromptTS: "100.5", Leaf: ""},                       // no leaf
		{PromptTS: "100.5", Leaf: strings.Repeat("x", 257)}, // oversized
		{ReplyTS: "100.9", Leaf: "b"},                       // no prompt message: keyed by reply
		{PromptTS: "99.000001", Leaf: "old"},                // out of order: older
		{PromptTS: "1000.000001", ReplyTS: "1000.2", Leaf: "c"},
	} {
		if err := r.RecordTurn(k, tr); err != nil {
			t.Fatal(err)
		}
	}
	for ts, want := range map[string]string{
		"":           "",
		"99":         "",
		"99.5":       "old",
		"100.000001": "a",
		"100.000002": "a",
		"100.8":      "a",
		"100.9":      "b",
		"999.9":      "b", // between turns → previous turn
		"1000.2":     "c",
		"1000.0":     "b", // 1000.000000 < 1000.000001
	} {
		if got := r.TurnLeaf(k, ts); got != want {
			t.Errorf("TurnLeaf(%q) = %q, want %q", ts, got, want)
		}
	}
}

func TestRecordTurnBounded(t *testing.T) {
	r, _ := newTestRouter(t)
	k := ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	for i := 1; i <= MaxTurns+5; i++ {
		if err := r.RecordTurn(k, Turn{PromptTS: fmt.Sprintf("%d.0", i), Leaf: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.TurnLeaf(k, "5.0"); got != "" {
		t.Fatalf("evicted turn still found: %q", got)
	}
	if got := r.TurnLeaf(k, "6.0"); got != "6" {
		t.Fatalf("oldest kept = %q", got)
	}
}

func TestRecordTurnErrors(t *testing.T) {
	r, _ := newTestRouter(t)
	if err := r.RecordTurn(ConvKey{ChannelID: "../x", ThreadTS: "1.0"}, Turn{PromptTS: "1.0", Leaf: "a"}); err == nil {
		t.Fatal("bad key must fail")
	}
	if got := r.TurnLeaf(ConvKey{ChannelID: "../x", ThreadTS: "1.0"}, "1.0"); got != "" {
		t.Fatal(got)
	}
	k := ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	if _, err := r.cwdFor(k); err != nil {
		t.Fatal(err)
	}
	if err := r.root.WriteFile(r.turnsPath(k), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := r.TurnLeaf(k, "1.0"); got != "" {
		t.Fatalf("corrupt file: %q", got)
	}
	r.Close()
	if got := r.TurnLeaf(k, "1.0"); got != "" {
		t.Fatal(got)
	}
}

func TestForkAtTurnLeaf(t *testing.T) {
	ctx := context.Background()
	origin := ConvKey{ChannelID: "C1", ThreadTS: "1.0"}
	for _, tc := range []struct {
		name    string
		atErr   error
		at      string
		wantAts []string
		wantErr bool
	}{
		{name: "known turn", at: "1.5", wantAts: []string{"L1"}},
		{name: "unknown ts", at: "0.5", wantAts: []string{""}},
		{name: "stale leaf falls back", atErr: errors.New("no such entry"), at: "1.5", wantAts: []string{"L1", ""}},
		{name: "unsupported does not retry", atErr: client.ErrForkUnsupported, at: "1.5", wantAts: []string{"L1"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, fa := newTestRouter(t)
			fa.forkAtErr = tc.atErr
			if _, err := r.GetOrCreate(ctx, origin, discardSink{}); err != nil {
				t.Fatal(err)
			}
			if err := r.RecordTurn(origin, Turn{PromptTS: "1.0", ReplyTS: "1.1", Leaf: "L1"}); err != nil {
				t.Fatal(err)
			}
			_, err := r.Fork(ctx, origin, ConvKey{ChannelID: "C1", ThreadTS: "2.0"}, tc.at)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v", err)
			}
			if fmt.Sprint(fa.forkAts) != fmt.Sprint(tc.wantAts) {
				t.Fatalf("fork ats = %q, want %q", fa.forkAts, tc.wantAts)
			}
		})
	}
}
