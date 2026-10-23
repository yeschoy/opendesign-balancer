package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"opendesign-balancer/internal/config"
	"opendesign-balancer/internal/logstore"
)

func bigBody() []byte {
	msgs := []any{map[string]any{"role": "system", "content": "be brief"}}
	for i := 0; i < 300; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": strings.Repeat("x", 700)},
			map[string]any{"role": "assistant", "content": strings.Repeat("y", 700), "tool_calls": []any{map[string]any{"id": "c", "function": map[string]any{"name": "f", "arguments": "{}"}}}})
	}
	b, _ := json.Marshal(map[string]any{"model": "m", "stream": true, "messages": msgs, "tools": make([]any, 40)})
	return b
}

func BenchmarkParseBody(b *testing.B) {
	body := bigBody()
	s := &Server{secret: []byte("k")}
	cfg := config.Default()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		obj, err := parseRequestBody(body)
		if err != nil {
			b.Fatal(err)
		}
		_ = s.identity(cfg, "g", nil, obj)
	}
}

func BenchmarkAppend(b *testing.B) {
	st, _ := logstore.Open(b.TempDir(), func() logstore.Limits {
		return logstore.Limits{RetentionDays: 30, MaxTotalBytes: 1 << 30, MaxRecords: map[string]int{"requests": 50000, "errors": 10000, "details": 500}}
	})
	defer st.Close()
	rec := &requestLog{TS: time.Now(), RequestID: "req_1", Path: "/v1/chat/completions", Model: "m", Attempts: []attemptLog{{Account: "a"}}}
	for b.Loop() {
		st.Append("requests", rec.TS, rec)
	}
}

func BenchmarkQuery(b *testing.B) {
	st, _ := logstore.Open(b.TempDir(), func() logstore.Limits {
		return logstore.Limits{RetentionDays: 30, MaxTotalBytes: 1 << 30, MaxRecords: map[string]int{"requests": 50000, "errors": 10000, "details": 500}}
	})
	defer st.Close()
	rec := &requestLog{TS: time.Now(), RequestID: "req_1", Model: "m", Attempts: []attemptLog{}}
	for range 50000 {
		st.Append("requests", rec.TS, rec)
	}
	time.Sleep(200 * time.Millisecond)
	for b.Loop() {
		st.Query("requests", map[string]string{"model": "m"}, time.Time{}, time.Time{}, 50, 0)
	}
}

func BenchmarkRedact(b *testing.B) {
	cfg := config.Default()
	for i := 0; i < 20; i++ {
		cfg.Accounts = append(cfg.Accounts, config.Account{ID: "a", RuntimeKey: "sk-amr-aaaaaaaaaaaa" + string(rune('a'+i)), ControlKey: "vela_ctrl_bbbbbbbbbbbb"})
	}
	txt := strings.Repeat(`{"error":{"message":"rate limited","code":429}}`, 300)
	for b.Loop() {
		redactSecrets(cfg, txt)
	}
}
