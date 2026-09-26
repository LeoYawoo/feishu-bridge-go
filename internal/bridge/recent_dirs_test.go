package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// newTestRecentDirsStore builds a store backed by t.TempDir() so each test
// gets an isolated on-disk state.
func newTestRecentDirsStore(t *testing.T) *recentDirsStore {
	t.Helper()
	return newRecentDirsStore(t.TempDir(), func(string, ...any) {})
}

func TestRecentDirsStore_Add_MovesToTop(t *testing.T) {
	s := newTestRecentDirsStore(t)
	s.Add("botA", "/x")
	s.Add("botA", "/y")
	s.Add("botA", "/x") // re-adding /x should promote it

	got := s.List("botA")
	want := []string{"/x", "/y"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
}

func TestRecentDirsStore_Add_MaxSize5(t *testing.T) {
	s := newTestRecentDirsStore(t)
	for i, p := range []string{"/a", "/b", "/c", "/d", "/e", "/f", "/g"} {
		s.Add("botA", p)
		if i < 5 {
			continue
		}
		if len(s.List("botA")) > recentDirsMax {
			t.Fatalf("Add #%d: list length %d exceeds max %d",
				i+1, len(s.List("botA")), recentDirsMax)
		}
	}
	// Oldest entries should have been dropped; most-recent survives.
	got := s.List("botA")
	if got[0] != "/g" {
		t.Errorf("front of LRU = %q, want /g", got[0])
	}
	for _, c := range got {
		if c == "/a" || c == "/b" {
			t.Errorf("oldest entries /a,/b should be dropped, got %v", got)
			break
		}
	}
}

func TestRecentDirsStore_SaveLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	logf := func(string, ...any) {}

	s1 := newRecentDirsStore(dir, logf)
	s1.Add("botA", "/api")
	s1.Add("botA", "/work")
	s1.Add("botB", "/solo")

	// Load a fresh store from the same directory; it should see the same
	// state. newRecentDirsStore loads on construction.
	s2 := newRecentDirsStore(dir, logf)
	if !reflect.DeepEqual(s2.List("botA"), []string{"/work", "/api"}) {
		t.Errorf("botA roundtrip = %v", s2.List("botA"))
	}
	if got := s2.List("botB"); !reflect.DeepEqual(got, []string{"/solo"}) {
		t.Errorf("botB roundtrip = %v", got)
	}
}

func TestRecentDirsStore_CorruptFile_Recovers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, recentDirsFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Corrupt file must not block startup; the store starts empty.
	s := newRecentDirsStore(dir, func(string, ...any) {})
	if got := s.List("botA"); len(got) != 0 {
		t.Errorf("List after corrupt load = %v, want empty", got)
	}
	// And the file has been removed.
	if _, err := os.Stat(filepath.Join(dir, recentDirsFile)); !os.IsNotExist(err) {
		t.Errorf("corrupt file should be removed, stat err=%v", err)
	}
}

func TestRecentDirsStore_Empty_MostRecentReturnsEmpty(t *testing.T) {
	s := newTestRecentDirsStore(t)
	if got := s.MostRecent("botA"); got != "" {
		t.Errorf("MostRecent = %q, want empty", got)
	}
	if got := s.List("botA"); got != nil {
		t.Errorf("List = %v, want nil", got)
	}
}

func TestRecentDirsStore_DifferentBotsIsolated(t *testing.T) {
	s := newTestRecentDirsStore(t)
	s.Add("botA", "/a")
	s.Add("botA", "/b")
	s.Add("botB", "/x")

	if !reflect.DeepEqual(s.List("botA"), []string{"/b", "/a"}) {
		t.Errorf("botA = %v", s.List("botA"))
	}
	if !reflect.DeepEqual(s.List("botB"), []string{"/x"}) {
		t.Errorf("botB = %v", s.List("botB"))
	}
	// Unknown bot returns nil, not a copy of somebody else's list.
	if got := s.List("botC"); got != nil {
		t.Errorf("botC = %v, want nil", got)
	}
}

func TestRecentDirsStore_List_ReturnsCopy(t *testing.T) {
	s := newTestRecentDirsStore(t)
	s.Add("botA", "/a")
	got := s.List("botA")
	got[0] = "mutated"
	if s.List("botA")[0] != "/a" {
		t.Error("List returned the internal slice; mutation leaked")
	}
}

func TestRecentDirsStore_Add_EmptyArgs_NoOp(t *testing.T) {
	s := newTestRecentDirsStore(t)
	s.Add("", "/a")   // no bot
	s.Add("botA", "") // no cwd
	if got := s.List("botA"); got != nil {
		t.Errorf("List after no-ops = %v, want nil", got)
	}
	// File should not have been written; store is still at initial state.
	// We don't assert "no file" directly because newRecentDirsStore may have
	// created an empty file on load/save; just assert the store's view.
	if _, ok := s.data[""]; ok {
		t.Error("empty botID was stored")
	}
	_ = json.Valid // keep json import used
}

func TestRecentDirsStore_Save_PersistsBetweenCalls(t *testing.T) {
	dir := t.TempDir()
	logf := func(string, ...any) {}

	s := newRecentDirsStore(dir, logf)
	s.Add("botA", "/x")
	// Direct check on the file so a no-op save cannot silently pass.
	b, err := os.ReadFile(filepath.Join(dir, recentDirsFile))
	if err != nil {
		t.Fatalf("expected file after Add, got %v", err)
	}
	var doc map[string][]string
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("on-disk JSON malformed: %v\n%s", err, b)
	}
	if !reflect.DeepEqual(doc["botA"], []string{"/x"}) {
		t.Errorf("on-disk botA = %v", doc["botA"])
	}
}
