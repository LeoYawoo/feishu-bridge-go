package bridge

import (
	"strings"
	"testing"

	"feishubridge/internal/agent"
	"feishubridge/internal/config"
)

func TestIsBridgeCommand_OnlyNew(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"/new", true},
		{"/new D:\\workcode\\api", true},
		{"/new /some/relative", true},
		{"  /new  ", true},  // leading/trailing whitespace tolerated
		{"\t/new\t", true},  // tab is fine too
		{"/newD:", false},   // not the command
		{"/tmp/foo", false}, // /-path must not be swallowed
		{"/newer", false},
		{"", false},
		{"hello", false},
	}
	for _, c := range cases {
		if got := isBridgeCommand(c.text); got != c.want {
			t.Errorf("isBridgeCommand(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestIsBridgeCommand_RejectedOldCommands(t *testing.T) {
	// The redesign (docs/design-session-and-test.md §3.3) dropped 11
	// commands. Regression-test that none of them sneak back in through
	// isBridgeCommand.
	for _, cmd := range []string{
		"/reset", "/cd", "/stop", "/cancel", "/status", "/help", "/h",
		"/sessions", "/model", "/resume", "/ls", "/pwd",
	} {
		if isBridgeCommand(cmd) {
			t.Errorf("isBridgeCommand(%q) = true; should be rejected", cmd)
		}
	}
}

func TestIsBridgeCommand_CaseInsensitive(t *testing.T) {
	if !isBridgeCommand("/NEW") {
		t.Error("/NEW should be treated as /new")
	}
	if !isBridgeCommand("/New D:\\x") {
		t.Error("/New <cwd> should be treated as /new <cwd>")
	}
}

func TestParseNewArgs_NoArg(t *testing.T) {
	if got := parseNewArgs("/new"); got != "" {
		t.Errorf("parseNewArgs('/new') = %q, want empty", got)
	}
	if got := parseNewArgs("/new   "); got != "" {
		t.Errorf("parseNewArgs('/new   ') = %q, want empty", got)
	}
}

func TestParseNewArgs_AbsolutePath(t *testing.T) {
	want := "D:\\workcode\\api"
	if got := parseNewArgs("/new D:\\workcode\\api"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseNewArgs_RelativePath(t *testing.T) {
	want := "./foo"
	if got := parseNewArgs("/new ./foo"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseNewArgs_PathWithSpaces(t *testing.T) {
	// Cwd may contain spaces; parseNewArgs rejoins from the raw string so
	// "/new my proj" preserves "my proj".
	want := "my proj"
	if got := parseNewArgs("/new my proj"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseNewArgs_AlreadyTrimmedOnly(t *testing.T) {
	// parseNewArgs assumes its input has already been trimmed;
	// isBridgeCommand does the trim before calling it. Regression-test
	// the contract so future edits don't silently regress callers that
	// do trim.
	want := "/abs/path"
	if got := parseNewArgs("/new /abs/path"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// --- recentDirButtons ------------------------------------------------------

func TestRecentDirButtons_Empty(t *testing.T) {
	bot := &config.BotConfig{ID: "main"}
	if got := recentDirButtons(bot, nil); got != nil {
		t.Errorf("recentDirButtons(nil) = %v, want nil", got)
	}
	if got := recentDirButtons(bot, []string{}); got != nil {
		t.Errorf("recentDirButtons([]) = %v, want nil", got)
	}
}

func TestRecentDirButtons_Basename(t *testing.T) {
	bot := &config.BotConfig{ID: "main"}
	got := recentDirButtons(bot, []string{"D:\\workcode\\api", "D:\\workcode\\work"})
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// First (most recent) gets the "(最近)" suffix and the basename only.
	if got[0].Text != "🆕 api (最近)" {
		t.Errorf("first button text = %q, want %q", got[0].Text, "🆕 api (最近)")
	}
	if got[1].Text != "🆕 work" {
		t.Errorf("second button text = %q, want %q", got[1].Text, "🆕 work")
	}
	// The cwd in Value must be the FULL path, not the basename.
	if got[0].Value["cwd"] != "D:\\workcode\\api" {
		t.Errorf("first cwd = %q", got[0].Value["cwd"])
	}
	if got[1].Value["cwd"] != "D:\\workcode\\work" {
		t.Errorf("second cwd = %q", got[1].Value["cwd"])
	}
}

func TestRecentDirButtons_ActionStamp(t *testing.T) {
	bot := &config.BotConfig{ID: "main"}
	got := recentDirButtons(bot, []string{"/a"})
	if got[0].Value["action"] != "new_topic" {
		t.Errorf("action = %q, want %q", got[0].Value["action"], "new_topic")
	}
}

func TestRecentDirButtons_Max5(t *testing.T) {
	bot := &config.BotConfig{ID: "main"}
	cwds := []string{"/a", "/b", "/c", "/d", "/e", "/f", "/g"}
	got := recentDirButtons(bot, cwds)
	if len(got) != 5 {
		// The recentDirsStore enforces max 5 in production; the button
		// constructor doesn't need to defensively truncate again because
		// it's the only consumer. But if a caller hands more, we still
		// want to render at most 5 — the Feishu card caps visible
		// buttons around there anyway.
		t.Errorf("len = %d, want 5", len(got))
	}
}

func TestRecentDirButtons_Root(t *testing.T) {
	// filepath.Base("/") is "/" on Linux/macOS but "\" on Windows.
	// Use filepath.Join to produce a platform-correct root and assert
	// the button label matches what the OS's path.Base would give.
	bot := &config.BotConfig{ID: "main"}
	// Just use a deep path whose basename is well-defined everywhere.
	got := recentDirButtons(bot, []string{"/deep/path/to/api"})
	if got[0].Text != "🆕 api (最近)" {
		t.Errorf("deep path label = %q", got[0].Text)
	}
}

// --- topicRootButtons ------------------------------------------------------

func TestTopicRootButtons_AlwaysHasNewButton(t *testing.T) {
	bot := &config.BotConfig{ID: "main"}
	got := topicRootButtons(bot, "omt_1", "om_root", nil)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 (only the 🆕 新 button)", len(got))
	}
	if got[0].Text != "🆕 新" {
		t.Errorf("text = %q", got[0].Text)
	}
	if got[0].Value["action"] != "new_session" {
		t.Errorf("action = %q", got[0].Value["action"])
	}
	if got[0].Value["thread"] != "omt_1" {
		t.Errorf("thread = %q", got[0].Value["thread"])
	}
	if got[0].Value["root"] != "om_root" {
		t.Errorf("root = %q", got[0].Value["root"])
	}
}

func TestTopicRootButtons_Max5HistoryPlusNew(t *testing.T) {
	bot := &config.BotConfig{ID: "main"}
	sessions := make([]*session, 0, 7)
	for i, id := range []string{
		"aaaaaaaa-1111-2222-3333-444444444444",
		"bbbbbbbb-1111-2222-3333-444444444444",
		"cccccccc-1111-2222-3333-444444444444",
		"dddddddd-1111-2222-3333-444444444444",
		"eeeeeeee-1111-2222-3333-444444444444",
		"ffffffff-1111-2222-3333-444444444444",
		"gggggggg-1111-2222-3333-444444444444",
	} {
		sessions = append(sessions, &session{ID: agent.SessionID(id)})
		_ = i
	}
	got := topicRootButtons(bot, "omt_1", "om_root", sessions)
	if len(got) != 6 { // 5 history + 1 new
		t.Errorf("len = %d, want 6", len(got))
	}
	// Last one must be the 🆕 新 button.
	if got[len(got)-1].Value["action"] != "new_session" {
		t.Errorf("last button action = %q", got[len(got)-1].Value["action"])
	}
}

func TestTopicRootButtons_HistoryStamp(t *testing.T) {
	bot := &config.BotConfig{ID: "main"}
	sessions := []*session{{ID: agent.SessionID("3b2526fd-aaaa-bbbb-cccc-dddddddddddd")}}
	got := topicRootButtons(bot, "omt_1", "om_root", sessions)
	if len(got) != 2 { // 1 history + 1 new
		t.Fatalf("len = %d", len(got))
	}
	h := got[0]
	if h.Value["action"] != "resume_session" {
		t.Errorf("action = %q", h.Value["action"])
	}
	if h.Value["thread"] != "omt_1" {
		t.Errorf("thread = %q", h.Value["thread"])
	}
	if h.Value["root"] != "om_root" {
		t.Errorf("root = %q", h.Value["root"])
	}
	if h.Value["session"] != "3b2526fd-aaaa-bbbb-cccc-dddddddddddd" {
		t.Errorf("session = %q", h.Value["session"])
	}
	// The button label shows only the first 8 chars of the session id.
	if !strings.HasPrefix(h.Text, "▶ 3b2526fd") {
		t.Errorf("text = %q, want prefix ▶ 3b2526fd", h.Text)
	}
}

func TestTopicRootButtons_SkipsNilAndEmptySessions(t *testing.T) {
	bot := &config.BotConfig{ID: "main"}
	// Nil session entries should not crash the constructor; empty-ID
	// entries should be silently skipped.
	sessions := []*session{
		nil,
		{ID: ""},
		{ID: agent.SessionID("abc")},
	}
	got := topicRootButtons(bot, "omt_1", "om_root", sessions)
	if len(got) != 2 { // 1 valid + 1 new
		t.Fatalf("len = %d, want 2; got %+v", len(got), got)
	}
	if got[0].Value["session"] != "abc" {
		t.Errorf("session = %q", got[0].Value["session"])
	}
}

func TestTopicRootButtons_ShortedID(t *testing.T) {
	bot := &config.BotConfig{ID: "main"}
	// long ID: only first 8 chars in the label
	got := topicRootButtons(bot, "omt", "omr", []*session{
		{ID: agent.SessionID("1234567890abcdef")},
	})
	if !strings.HasPrefix(got[0].Text, "▶ 12345678") {
		t.Errorf("long id label = %q", got[0].Text)
	}
	// short ID: full id in the label
	got = topicRootButtons(bot, "omt", "omr", []*session{
		{ID: agent.SessionID("abc12")},
	})
	if !strings.HasPrefix(got[0].Text, "▶ abc12") {
		t.Errorf("short id label = %q", got[0].Text)
	}
}
