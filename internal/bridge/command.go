package bridge

import (
	"path/filepath"
	"strings"

	"feishubridge/internal/card"
	"feishubridge/internal/config"
)

// isBridgeCommand reports whether text starts with a known bridge command.
// Only exact first-token matches are treated as commands, so that a user
// typed message like "/tmp/foo" is never swallowed.
//
// The redesign (docs/design-session-and-test.md §3.3) collapses the command
// surface to a single entry point: /new. Main chat is a strict console,
// topics are pure claude conversations. Anything else the user types falls
// through to the agent.
func isBridgeCommand(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	first := strings.Fields(t)
	if len(first) == 0 {
		return false
	}
	return first[0] == "/new"
}

// parseNewArgs splits "/new <cwd?>" into the raw cwd argument. Empty string
// when no argument was supplied. Not normalised here; the caller resolves
// relative paths and validates existence.
func parseNewArgs(text string) string {
	fields := strings.Fields(text)
	if len(fields) < 2 {
		return ""
	}
	// Rejoin in case the cwd contains spaces; the cwd arg is everything
	// after the first token.
	rest := strings.TrimSpace(text[len(fields[0]):])
	return rest
}

// recentDirButtons renders the console card's "recent directories" button
// set. Each button's Value stamps action="new_topic" and the full cwd, so
// clicking it is equivalent to typing "/new <cwd>" in the main chat.
//
// Basename is used for display; the full path is preserved in Value so the
// callback can resolve to the exact directory without ambiguity (two cwds
// can share a basename).
//
// Up to recentDirsMax buttons. The "(最近)" suffix on the first entry is a
// UX affordance: when the user just wants to continue where they left off,
// the most-recent row is what they'll click.
func recentDirButtons(bot *config.BotConfig, cwds []string) []card.Button {
	if len(cwds) == 0 {
		return nil
	}
	// Defensive: Feishu cards cap visible buttons around 5-6. Even though
	// recentDirsStore enforces recentDirsMax, don't rely on the caller
	// honouring it — the constructor is the last gate before pixels.
	if len(cwds) > recentDirsMax {
		cwds = cwds[:recentDirsMax]
	}
	out := make([]card.Button, 0, len(cwds))
	for i, cwd := range cwds {
		label := filepath.Base(cwd)
		if label == "." || label == "" {
			label = cwd
		}
		if i == 0 {
			label += " (最近)"
		}
		out = append(out, card.Button{
			Text: "🆕 " + label,
			Value: map[string]string{
				"action": "new_topic",
				"cwd":    cwd,
			},
		})
	}
	return out
}

// topicRootButtons renders the topic-root card's session-picker buttons.
// Up to 5 history sessions (most-recent-first) plus a "🆕 新" button.
//
// History session buttons stamp action="resume_session" plus the topic's
// thread and root message ids (which must survive the callback round-trip;
// the callback's Context only carries the clicked card's own id). The
// pendingSessions map in Bridge is keyed on threadID, so the same thread
// root can be offered on any of its history sessions.
//
// The "🆕 新" button stamps action="new_session"; clicking it clears any
// pending resume for this thread, so the next plain message starts a fresh
// session.
func topicRootButtons(bot *config.BotConfig, thread, root string, sessions []*session) []card.Button {
	maxHistory := 5
	if len(sessions) > maxHistory {
		sessions = sessions[:maxHistory]
	}
	out := make([]card.Button, 0, len(sessions)+1)
	for _, se := range sessions {
		if se == nil || se.ID == "" {
			continue
		}
		out = append(out, card.Button{
			Text: "▶ " + shortID(string(se.ID)),
			Value: map[string]string{
				"action":  "resume_session",
				"thread":  thread,
				"root":    root,
				"session": string(se.ID),
			},
		})
	}
	out = append(out, card.Button{
		Text: "🆕 新",
		Value: map[string]string{
			"action": "new_session",
			"thread": thread,
			"root":   root,
		},
	})
	return out
}
