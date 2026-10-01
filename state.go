package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/pyed/rtapi"
)

const stateVersion = 1

// stateData is everything rtelegram remembers across restarts.
type stateData struct {
	Version int                     `json:"version"`
	Sorts   map[int64]rtapi.Sorting `json:"sorts,omitempty"`
	// Notify holds each chat's event subscriptions.
	Notify map[int64]notifySettings `json:"notify,omitempty"`
	// CompletedWatermark is the newest finish time already announced, and
	// CompletedAt the torrents announced at exactly that second. Zero means
	// the watcher has not looked yet, so existing torrents are not announced.
	CompletedWatermark uint64   `json:"completedWatermark,omitempty"`
	CompletedAt        []string `json:"completedAt,omitempty"`
	// Quiet holds the quiet hours schedule, if any.
	Quiet *quietHours `json:"quiet,omitempty"`
}

type notifySettings struct {
	Events []string `json:"events"`
	Thread int      `json:"thread,omitempty"` // the forum topic to post in
}

// state holds stateData and saves it as JSON after every change. A state
// with no path keeps changes in memory only.
type state struct {
	path string
	mu   sync.Mutex
	data stateData
}

// defaultStatePath is rtelegram/state.json in the user's config directory.
func defaultStatePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find a directory for the state file (set -state): %w", err)
	}
	return filepath.Join(dir, "rtelegram", "state.json"), nil
}

// loadState reads the state file at path, or starts empty when it does not
// exist yet. A file that cannot be read is an error rather than a fresh
// start, so a damaged file never silently loses subscriptions.
func loadState(path string) (*state, error) {
	s := &state{path: path, data: stateData{Version: stateVersion}}
	if path == "" {
		return s, nil
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}
	if err := json.Unmarshal(content, &s.data); err != nil {
		return nil, fmt.Errorf("state file %s is damaged (move it aside to start fresh): %w", path, err)
	}
	if s.data.Version > stateVersion {
		return nil, fmt.Errorf("state file %s is from a newer rtelegram (version %d)", path, s.data.Version)
	}
	s.data.Version = stateVersion
	return s, nil
}

// read calls fn with the current state. A nil state reads as empty.
func (s *state) read(fn func(*stateData)) {
	if s == nil {
		fn(&stateData{})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.data)
}

// update applies change and saves the result. If saving fails, the change is
// kept in memory and the error is returned so the caller can report it.
func (s *state) update(change func(*stateData)) error {
	if s == nil {
		return errors.New("no state store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	change(&s.data)
	return s.save()
}

// save writes the state atomically: to a private temporary file in the same
// directory, which then replaces the old file.
func (s *state) save() error {
	if s.path == "" {
		return nil
	}
	content, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	defer os.Remove(temp.Name()) // a no-op once renamed
	if _, err := temp.Write(append(content, '\n')); err != nil {
		temp.Close()
		return fmt.Errorf("save state: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("save state: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	if err := os.Rename(temp.Name(), s.path); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}
