package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opendesign-balancer/internal/config"
)

// Rolling counters wait for sampleSaveEvery; routing state is saved at once.
func TestMetadataSavesCountersLazilyAndStateAtOnce(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}}, nil)
	s := e.srv
	path := filepath.Join(s.dataDir, "metadata.json")
	read := func() []byte {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if err := s.saveMetadata(true); err != nil {
		t.Fatal(err)
	}
	before := read()
	if bytes.Contains(before, []byte("\n  ")) {
		t.Fatal("metadata.json is still indented")
	}

	readAll(e.post("/v1/chat/completions", chat(false), nil)) // stats + a health sample
	if err := s.saveMetadata(false); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read(), before) {
		t.Fatal("counters alone rewrote metadata.json before sampleSaveEvery")
	}
	s.metaSavedAt = time.Now().Add(-sampleSaveEvery)
	if err := s.saveMetadata(false); err != nil {
		t.Fatal(err)
	}
	saved := read()
	if bytes.Equal(saved, before) {
		t.Fatal("counters never saved after sampleSaveEvery")
	}

	s.pool.Cooldown("a", "", time.Now().Add(time.Hour), 429, "test", "test")
	if err := s.saveMetadata(false); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(read(), saved) {
		t.Fatal("a cooldown waited for sampleSaveEvery")
	}
}

func TestAdminSessionsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	a := newAdminAuth(path)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/login", nil)
	sess := a.create(rec, req)
	cookie := rec.Result().Cookies()[0]
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), cookie.Value) {
		t.Fatal("the sessions file holds a usable token")
	}
	next := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	next.AddCookie(cookie)
	if _, got := newAdminAuth(path).get(next); got == nil || got.csrf != sess.csrf {
		t.Fatalf("session lost across restart: %+v", got)
	}
	a.destroyAll()
	if _, got := newAdminAuth(path).get(next); got != nil {
		t.Fatal("a destroyed session came back")
	}
}
