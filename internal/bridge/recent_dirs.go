package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

const recentDirsFile = ".feishu-bridge-recent-dirs.json"

// recentDirsMax is the per-bot cap on the cwd LRU. The design doc fixes this
// at 5: the console card shows up to 5 recent-directory buttons, so storing
// more would only be dead weight.
const recentDirsMax = 5

// recentDirsStore is a per-bot LRU of recently used cwds, persisted next to
// the session table so the console card survives a restart.
//
// Unlike sessionStore, this is deliberately small: one slice per bot, an
// Add / List / MostRecent trio. It is deliberately not keyed by
// bot/chat/thread — the console is a per-bot surface, and the same cwd is
// meaningful for the user regardless of which chat they typed /new in.
type recentDirsStore struct {
	mu   sync.Mutex
	data map[string][]string // botID -> ordered cwd list, most-recent first
	path string
	logf func(format string, args ...any)
}

// newRecentDirsStore loads an existing file if present. A missing file is a
// clean start; a corrupt file is deleted and logged (matching sessionStore's
// policy that persistence must never block startup).
func newRecentDirsStore(dir string, logf func(string, ...any)) *recentDirsStore {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &recentDirsStore{
		data: make(map[string][]string),
		path: filepath.Join(dir, recentDirsFile),
		logf: logf,
	}
	if err := s.load(); err != nil {
		s.logf("recent dirs: could not load %s: %v", s.path, err)
		_ = os.Remove(s.path)
	}
	return s
}

func (s *recentDirsStore) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	// On-disk shape: {"<botID>": ["cwd1", "cwd2", ...], ...}
	var doc map[string][]string
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	for bot, cwds := range doc {
		if bot == "" {
			continue
		}
		s.data[bot] = cwds
	}
	return nil
}

// save writes the table. Called after every mutation; the in-memory state is
// authoritative so a failed write is a logged degradation, not a crash.
func (s *recentDirsStore) save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		s.logf("recent dirs: marshal: %v", err)
		return
	}
	if err := os.WriteFile(s.path, b, 0o600); err != nil {
		s.logf("recent dirs: write %s: %v", s.path, err)
	}
}

// Add moves cwd to the front of bot's list, dropping duplicates and
// truncating to recentDirsMax. Empty bot or cwd is a no-op: the console card
// will still render (possibly empty) and the LRU is only useful once the
// user has actually opened a topic.
func (s *recentDirsStore) Add(botID, cwd string) {
	if botID == "" || cwd == "" {
		return
	}
	s.mu.Lock()
	cur := s.data[botID]
	out := make([]string, 0, len(cur)+1)
	out = append(out, cwd)
	for _, c := range cur {
		if c == cwd {
			continue
		}
		out = append(out, c)
		if len(out) >= recentDirsMax {
			break
		}
	}
	s.data[botID] = out
	s.mu.Unlock()
	s.save()
}

// List returns bot's cwd LRU in most-recent-first order, capped at
// recentDirsMax. The returned slice is a copy — callers may mutate it
// without affecting store state.
func (s *recentDirsStore) List(botID string) []string {
	if botID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.data[botID]
	if len(cur) == 0 {
		return nil
	}
	out := make([]string, len(cur))
	copy(out, cur)
	return out
}

// MostRecent returns the front of bot's LRU, or "" if the list is empty.
// The no-arg /new command falls back to the bot's configured Workspace when
// this is empty; the caller decides whether "" is meaningful.
func (s *recentDirsStore) MostRecent(botID string) string {
	l := s.List(botID)
	if len(l) == 0 {
		return ""
	}
	return l[0]
}
