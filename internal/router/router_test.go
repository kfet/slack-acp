package router

import "testing"

func TestConvKeyString(t *testing.T) {
	k := ConvKey{ChannelID: "C1", ThreadTS: "100.0"}
	if k.String() != "C1/100.0" {
		t.Fatalf("got %q", k.String())
	}
}
