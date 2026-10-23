package logstore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type keyedRec struct {
	TS        time.Time `json:"ts"`
	RequestID string    `json:"requestId"`
	Status    int       `json:"status"`
}

func (r keyedRec) LogKeys() map[string]string {
	return map[string]string{"requestId": r.RequestID, "status": "200"}
}

func limits() Limits {
	return Limits{RetentionDays: 1, MaxTotalBytes: 1 << 20, MaxRecords: map[string]int{"requests": 100, "errors": 100}}
}

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, limits)
	if err != nil {
		t.Fatal(err)
	}
	for !s.Ready() {
		time.Sleep(time.Millisecond)
	}
	return s
}

// Buffered lines reach the segment on Close and are recovered on reopen;
// a Keyed record's own keys drive the filters.
func TestBufferedWritesFlushOnCloseAndRecover(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	now := time.Now().UTC()
	s.Append("requests", now, keyedRec{TS: now, RequestID: "req_a", Status: 200})
	s.Append("requests", now, map[string]any{"ts": now, "requestId": "req_b", "status": 500})
	page, err := s.Query("requests", map[string]string{"status": "200"}, time.Time{}, time.Time{}, 10, 0)
	if err != nil || len(page.Items) != 1 || !strings.Contains(string(page.Items[0]), "req_a") {
		t.Fatalf("filter by Keyed keys: %v %v", page.Items, err)
	}
	s.Close()

	files, _ := filepath.Glob(filepath.Join(dir, "requests-*.jsonl"))
	var text string
	for _, f := range files {
		b, _ := os.ReadFile(f)
		text += string(b)
	}
	if !strings.Contains(text, "req_a") || !strings.Contains(text, "req_b") {
		t.Fatalf("buffered records not flushed on Close: %q", text)
	}

	r := open(t, dir)
	defer r.Close()
	page, _ = r.Query("requests", map[string]string{"status": "500"}, time.Time{}, time.Time{}, 10, 0)
	if len(page.Items) != 1 || !strings.Contains(string(page.Items[0]), "req_b") {
		t.Fatalf("not recovered from disk: %v", page.Items)
	}
}
