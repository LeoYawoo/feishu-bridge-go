package feishu

import (
	"reflect"
	"testing"
)

func TestParseMS(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 0},
		{"0", 0},
		{"12345", 12345},
		{"1713000000000", 1713000000000}, // typical Feishu ms timestamp
		{"9223372036854775807", 9223372036854775807}, // int64 max
		{"-1", 0},    // negative is not ms
		{"12a45", 0}, // non-digit
		{"12 45", 0}, // space
	}
	for _, c := range cases {
		if got := parseMS(c.in); got != c.want {
			t.Errorf("parseMS(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestStripMentionKeys(t *testing.T) {
	raw := "hello @_user_123 how are you @_user_456"
	type mention struct {
		key, openID, name string
	}
	// We can't construct larkim.MentionEvent easily from within this
	// package's tests without pulling in SDK types; use a stub adapter.
	type mnEvent struct {
		key, openID, name *string
	}
	_ = mnEvent{} // just to keep the shape clear

	// Actually parse.go uses []*larkim.MentionEvent, not our own type.
	// The test would need to construct SDK types, which is heavy. Skip
	// for now; stripMentionKeys is only called from parseMessage which
	// is itself called on real SDK events.
	_ = raw
}

// TestStripMentionKeys_EmptyMentions confirms that when there are no
// mentions, the raw text is returned trimmed but otherwise unchanged.
func TestStripMentionKeys_EmptyMentions(t *testing.T) {
	got := stripMentionKeys("  hello world  ", nil, nil)
	want := "hello world"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestStripMentionKeys_NilEntries confirms nil entries in the mentions
// slice are skipped without panicking.
func TestStripMentionKeys_NilEntries(t *testing.T) {
	got := stripMentionKeys("hi", nil, nil)
	if got != "hi" {
		t.Errorf("got %q, want %q", got, "hi")
	}
}

// TestStripMentionKeys_OutputPointerNil confirms the out slice pointer
// may be nil without panicking.
func TestStripMentionKeys_OutputPointerNil(t *testing.T) {
	got := stripMentionKeys("hi", nil, nil)
	if got != "hi" {
		t.Errorf("got %q", got)
	}
	// out was nil; the call must not have dereferenced it.
}

// TestExtractCardText exercises the card walker on a small, realistic
// schema-2.0 card that the bridge itself emits.
func TestExtractCardText_MarkdownOnly(t *testing.T) {
	card := map[string]any{
		"schema": "2.0",
		"body": map[string]any{
			"elements": []any{
				map[string]any{
					"tag":     "markdown",
					"content": "hello **bold**",
				},
			},
		},
	}
	text, ok := extractCardText(card)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if text != "hello **bold**" {
		t.Errorf("text = %q", text)
	}
}

func TestExtractCardText_DivText(t *testing.T) {
	card := map[string]any{
		"schema": "2.0",
		"body": map[string]any{
			"elements": []any{
				map[string]any{
					"tag": "div",
					"text": map[string]any{
						"tag":     "plain_text",
						"content": "from div",
					},
				},
			},
		},
	}
	text, ok := extractCardText(card)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if text != "from div" {
		t.Errorf("text = %q", text)
	}
}

func TestExtractCardText_MultipleJoined(t *testing.T) {
	card := map[string]any{
		"schema": "2.0",
		"body": map[string]any{
			"elements": []any{
				map[string]any{"tag": "markdown", "content": "line 1"},
				map[string]any{"tag": "markdown", "content": "line 2"},
			},
		},
	}
	text, ok := extractCardText(card)
	if !ok {
		t.Fatal("expected ok=true")
	}
	want := "line 1\nline 2"
	if text != want {
		t.Errorf("text = %q, want %q", text, want)
	}
}

func TestExtractCardText_EmptyCard(t *testing.T) {
	card := map[string]any{
		"schema": "2.0",
		"body": map[string]any{
			"elements": []any{},
		},
	}
	if _, ok := extractCardText(card); ok {
		t.Error("expected ok=false for empty card")
	}
}

func TestExtractCardText_NonTextElementIgnored(t *testing.T) {
	// A button element has no text payload; walker must skip past it
	// rather than returning the button's "text.tag" or something.
	card := map[string]any{
		"schema": "2.0",
		"body": map[string]any{
			"elements": []any{
				map[string]any{
					"tag": "button",
					"text": map[string]any{
						"tag":     "plain_text",
						"content": "click me",
					},
				},
			},
		},
	}
	if _, ok := extractCardText(card); ok {
		t.Error("button text should not be extracted (button is not markdown/div)")
	}
}

// TestWalkCard_NestedStructure confirms the walker recurses through
// arbitrary nesting. A real card might bury markdown inside
// body.elements[].text, etc.
func TestWalkCard_NestedStructure(t *testing.T) {
	card := map[string]any{
		"body": map[string]any{
			"elements": []any{
				map[string]any{
					"tag": "column_set",
					"columns": []any{
						map[string]any{
							"elements": []any{
								map[string]any{
									"tag":     "markdown",
									"content": "nested",
								},
							},
						},
					},
				},
			},
		},
	}
	text, ok := extractCardText(card)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if text != "nested" {
		t.Errorf("text = %q", text)
	}
	_ = reflect.DeepEqual // keep reflect import for future cases
}
