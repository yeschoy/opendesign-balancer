package server

import (
	"bytes"
	"os"
	"path/filepath"
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
