package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"feishubridge/internal/agent"
)

const sessionsFile = ".feishu-bridge-sessions.json"

// sessionStore is the session table, its reverse index and persistence.
//
// It lives outside Bridge so the map operations can be tested without a
// websocket. Bridge owns a single instance; all access is serialised here.
type sessionStore struct {
	mu    sync.Mutex
	byKey map[string]*session        // key = sessionKey(bot, chat, thread)
	byID  map[agent.SessionID]string // session_id -> key
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

// Set upserts and maintains the reverse index.
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
	s.byKey[key] = se
	s.byID[se.ID] = key
}

// Delete removes by chat coordinates.
func (s *sessionStore) Delete(botID, chatID, threadID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := sessionKey(botID, chatID, threadID)
	if se := s.byKey[key]; se != nil && se.ID != "" {
		delete(s.byID, se.ID)
	}
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
		}
	}
	return len(idle)
}
