package ratelimit

import (
	"testing"
	"time"
)

func TestBucketAdmitsUpToCapacityThenRefuses(t *testing.T) {
	now := time.Now()
	b := New(3, 4, func() time.Time { return now })
	for i := 0; i < 3; i++ {
		if !b.Allow() {
			t.Fatalf("token %d must be admitted", i)
		}
	}
	if b.Allow() {
		t.Fatal("the bucket must refuse once drained")
	}
}

func TestBucketRefillsOverTime(t *testing.T) {
	now := time.Now()
	b := New(6, 4, func() time.Time { return now })
	for i := 0; i < 6; i++ {
		b.Allow()
	}
	if b.Allow() {
		t.Fatal("drained")
	}
	// A tenth of a minute at 6/min refills exactly one token.
	now = now.Add(10 * time.Second)
	if !b.Allow() {
		t.Fatal("one token must have refilled")
	}
	if b.Allow() {
		t.Fatal("only one token was due")
	}
}

// TestBucketDoesNotOverfill pins that a long idle period cannot bank
// unlimited credit — otherwise the cap would be no cap at all after a
// quiet night.
func TestBucketDoesNotOverfill(t *testing.T) {
	now := time.Now()
	b := New(2, 4, func() time.Time { return now })
	now = now.Add(24 * time.Hour)
	if !b.Allow() || !b.Allow() {
		t.Fatal("capacity must be available")
	}
	if b.Allow() {
		t.Fatal("idle time must not bank credit beyond capacity")
	}
}

// TestNilBucketRefuses is the fail-closed property: every caller guards
// an off-by-default affordance, so "not configured" and "refuse" must
// be the same thing.
func TestNilBucketRefuses(t *testing.T) {
	var b *Bucket
	if b.Allow() {
		t.Fatal("a nil bucket must never admit anything")
	}
}

func TestNewAppliesFallbackAndDefaultClock(t *testing.T) {
	b := New(0, 5, nil)
	if b.capacity != 5 {
		t.Fatalf("fallback not applied: %v", b.capacity)
	}
	if !b.Allow() {
		t.Fatal("the default clock must work")
	}
}
