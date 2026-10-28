package catchup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store is the persisted per-conversation checkpoint: for every channel
// or DM, the ts of the newest message the relay has processed there.
//
// Slack ts values are fixed-width "<10-digit seconds>.<6-digit>"
// decimals that increase in posting order within a conversation —
// thread replies included — so one ts per conversation covers its
// top-level posts and all of its threads, and plain string comparison
// orders them.
//
// Floor is the checkpoint of a conversation that has none of its own.
// It is set to "now" the first time the store is created, so a relay
// installed today never replays the history that predates it.
type Store struct {
	path string

	mu    sync.Mutex
	state storeState
}

type storeState struct {
	Floor    string            `json:"floor"`
	Channels map[string]string `json:"channels"`
}

// Open loads the store at path. A missing file is a first run: the
// floor is set to now and persisted at once, so a crash before the
// first message still leaves the relay with a "nothing before this"
// line rather than none.
func Open(path string, now time.Time) (*Store, error) {
	s := &Store{path: path}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s.state = storeState{Floor: TS(now), Channels: map[string]string{}}
		if err := s.save(); err != nil {
			return nil, err
		}
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("catchup: read checkpoint: %w", err)
	}
	if err := json.Unmarshal(b, &s.state); err != nil || s.state.Floor == "" {
		return nil, fmt.Errorf("catchup: checkpoint %s is malformed", path)
	}
	if s.state.Channels == nil {
		s.state.Channels = map[string]string{}
	}
	return s, nil
}

// TS renders t as a Slack ts.
func TS(t time.Time) string {
	return fmt.Sprintf("%d.%06d", t.Unix(), t.Nanosecond()/1000)
}

// Mark is the checkpoint of channel: its own, else the floor.
func (s *Store) Mark(channel string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ts, ok := s.state.Channels[channel]; ok {
		return ts
	}
	return s.state.Floor
}

// Has reports whether channel has a checkpoint of its own.
func (s *Store) Has(channel string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.state.Channels[channel]
	return ok
}

// Advance moves channel's checkpoint forward to ts and persists it. A
// ts at or below the current mark is a no-op: the checkpoint never
// moves back.
func (s *Store) Advance(channel, ts string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.state.Channels[channel]
	if !ok {
		cur = s.state.Floor
	}
	if ts <= cur {
		return nil
	}
	s.state.Channels[channel] = ts
	return s.save()
}

// save writes the state atomically. Called with mu held (or before the
// store is shared).
func (s *Store) save() error {
	// A struct of strings and a string map cannot fail to encode.
	b, _ := json.Marshal(s.state)
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("catchup: mkdir: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("catchup: write checkpoint: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("catchup: commit checkpoint: %w", err)
	}
	return nil
}
