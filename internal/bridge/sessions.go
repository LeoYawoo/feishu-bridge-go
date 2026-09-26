package bridge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"feishubridge/internal/agent"
)

const sessionsFile = ".feishu-bridge-sessions.json"

// ---- idle reaping --------------------------------------------------------

// ReaperIntervalSec is how often the reaper sweeps.
const ReaperIntervalSec = 60

// reaper runs a collect/stop pair on a fixed interval. It holds no state of
// its own; the bridge supplies both callbacks, so the reaper never touches
// the maps it does not own. Session records are its only current caller, but
// the shape generalises: it is just a periodic two-step sweep.
type reaper struct {
	idle time.Duration
	logf func(format string, args ...any)
}

func newReaper(idleSec int, logf func(string, ...any)) *reaper {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &reaper{idle: time.Duration(idleSec) * time.Second, logf: logf}
}

// cutoff is the idle threshold, read by the collect callback.
func (r *reaper) cutoff() time.Duration { return r.idle }

// Run sweeps until ctx is cancelled.
func (r *reaper) Run(ctx context.Context, collect func() []string, stop func(context.Context, []string)) {
	r.logf("idle reaper: stopping records idle over %s, every %s",
		r.idle, time.Duration(ReaperIntervalSec)*time.Second)

	t := time.NewTicker(time.Duration(ReaperIntervalSec) * time.Second)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			keys := collect()
			if len(keys) == 0 {
				continue
			}
			r.logf("reaper: %d record(s) idle over %s, stopping", len(keys), r.idle)
			stop(ctx, keys)
		}
	}
}

// shortKey trims a key for log lines.
func shortKey(k string) string {
	if len(k) > 24 {
		return k[:24] + "…"
	}
	return k
}

// ---- session store -------------------------------------------------------

// sessionStore is the session table, its reverse index and persistence.
//
// It lives outside Bridge so the map operations can be tested without a
// websocket. Bridge owns a single instance; all access is serialised here.
type sessionStore struct {
	mu    sync.Mutex
	byKey map[string]*session        // key = sessionKey(bot, chat, thread)
	byID  map[agent.SessionID]string // session_id -> key
	byCwd map[string][]string        // cwd -> []key, most-recent-first
	path  string
	logf  func(format string, args ...any)
}

func newSessionStore(dir string, logf func(string, ...any)) *sessionStore {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	s := &sessionStore{
		byKey: make(map[string]*session),
		byID:  make(map[agent.SessionID]string),
		byCwd: make(map[string][]string),
		path:  filepath.Join(dir, sessionsFile),
		logf:  logf,
	}
	if err := s.load(); err != nil {
		// A corrupt file must not block startup. The mapping is lost but the
		// bridge still runs, and Claude's own session files remain intact.
		s.logf("session store: could not load %s: %v", s.path, err)
		_ = os.Remove(s.path)
	}
	return s
}

// load hydrates both maps from disk. A missing file is a clean start.
func (s *sessionStore) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var doc struct {
		Sessions []*session `json:"sessions"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	for _, se := range doc.Sessions {
		if se == nil || se.ID == "" {
			continue
		}
		key := sessionKey(se.BotID, se.ChatID, se.ThreadID)
		s.byKey[key] = se
		s.byID[se.ID] = key
		s.indexByCwd(key, se)
	}
	return nil
}

// Save writes the table. Called after every mutation; the in-memory state is
// authoritative so a failed write is a logged degradation, not a crash.
func (s *sessionStore) Save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*session, 0, len(s.byKey))
	for _, se := range s.byKey {
		out = append(out, se)
	}
	b, err := json.MarshalIndent(map[string]any{"sessions": out}, "", "  ")
	if err != nil {
		s.logf("session store: marshal: %v", err)
		return
	}
	if err := os.WriteFile(s.path, b, 0o600); err != nil {
		s.logf("session store: write %s: %v", s.path, err)
	}
}

// Get returns the session for a bot/chat/thread, or nil.
func (s *sessionStore) Get(botID, chatID, threadID string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byKey[sessionKey(botID, chatID, threadID)]
}

// Set upserts and maintains the reverse indices. The byCwd bucket is
// re-keyed off the incoming record's Cwd so a session whose cwd changes
// during an upsert lands in the right bucket.
func (s *sessionStore) Set(se *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sessionKey(se.BotID, se.ChatID, se.ThreadID)
	if old := s.byKey[key]; old != nil && old.ID != "" && old.ID != se.ID {
		delete(s.byID, old.ID)
	}
	if se.LastSeen.IsZero() {
		se.LastSeen = time.Now()
	}
	// Remove this key from any cwd bucket before re-adding under the new cwd.
	s.unindexByCwd(key)
	s.byKey[key] = se
	s.byID[se.ID] = key
	s.indexByCwd(key, se)
}

// Delete removes by chat coordinates.
func (s *sessionStore) Delete(botID, chatID, threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sessionKey(botID, chatID, threadID)
	if se := s.byKey[key]; se != nil && se.ID != "" {
		delete(s.byID, se.ID)
	}
	s.unindexByCwd(key)
	delete(s.byKey, key)
}

// Count returns the number of live sessions.
func (s *sessionStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byKey)
}

// Idle returns sessions not seen within cutoff.
func (s *sessionStore) Idle(cutoff time.Duration) []*session {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	var out []*session
	for _, se := range s.byKey {
		if now.Sub(se.LastSeen) > cutoff {
			out = append(out, se)
		}
	}
	return out
}

// ClearIdle drops every session older than cutoff and returns how many went.
func (s *sessionStore) ClearIdle(cutoff time.Duration) int {
	idle := s.Idle(cutoff)
	if len(idle) == 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, se := range idle {
		key := sessionKey(se.BotID, se.ChatID, se.ThreadID)
		if s.byKey[key] == se {
			delete(s.byID, se.ID)
			delete(s.byKey, key)
			s.unindexByCwd(key)
		}
	}
	return len(idle)
}

// SessionsForCwd returns the sessions whose Cwd matches, most-recent-first,
// capped at limit. Empty/nil is returned for unknown cwds or when there is
// nothing to list. The topic-root card uses this for its history-session
// buttons.
//
// Cwd is compared exactly as stored; the caller is responsible for
// normalising (filepath.Abs/Join) before and after writing. Sessions with an
// empty Cwd (legacy records from before the field was added) are never
// returned here — the caller's fallback is the bot's configured workspace.
func (s *sessionStore) SessionsForCwd(cwd string, limit int) []*session {
	if cwd == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := s.byCwd[cwd]
	if len(keys) == 0 {
		return nil
	}
	if limit > 0 && limit < len(keys) {
		keys = keys[:limit]
	}
	out := make([]*session, 0, len(keys))
	for _, k := range keys {
		if se := s.byKey[k]; se != nil {
			out = append(out, se)
		}
	}
	return out
}

// indexByCwd adds key to the head of the cwd bucket when se.Cwd is set.
// The caller must hold s.mu.
func (s *sessionStore) indexByCwd(key string, se *session) {
	if se == nil || se.Cwd == "" {
		return
	}
	bucket := s.byCwd[se.Cwd]
	// Promote to front, deduping (shouldn't happen post-unindex, but cheap).
	out := make([]string, 0, len(bucket)+1)
	out = append(out, key)
	for _, k := range bucket {
		if k != key {
			out = append(out, k)
		}
	}
	s.byCwd[se.Cwd] = out
}

// unindexByCwd removes key from whatever cwd bucket it currently lives in.
// The caller must hold s.mu.
func (s *sessionStore) unindexByCwd(key string) {
	for cwd, bucket := range s.byCwd {
		found := false
		for i, k := range bucket {
			if k == key {
				found = true
				out := make([]string, 0, len(bucket)-1)
				out = append(out, bucket[:i]...)
				out = append(out, bucket[i+1:]...)
				if len(out) == 0 {
					delete(s.byCwd, cwd)
				} else {
					s.byCwd[cwd] = out
				}
				return
			}
		}
		_ = found
	}
}
