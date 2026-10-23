package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"opendesign-balancer/internal/config"
	"opendesign-balancer/internal/logstore"
)

func TestHasUsage(t *testing.T) {
	for _, c := range []struct {
		data string
		want bool
	}{
		{`{"choices":[{"delta":{"content":"hi"}}],"usage":null}`, false},
		{`{"choices":[],"usage" : null}`, false},
		{`{"choices":[],"usage":{"prompt_tokens":3}}`, true},
		{`{"type":"message_delta","usage":{"output_tokens":9}}`, true},
		{`{"a":{"usage":null},"usage":{"prompt_tokens":1}}`, true},
		{`{"content":"the word \"usage\" in text"}`, false},
		{`{"choices":[{"delta":{"content":"hi"}}]}`, false},
	} {
		if got := hasUsage([]byte(c.data)); got != c.want {
			t.Errorf("hasUsage(%s) = %v, want %v", c.data, got, c.want)
		}
	}
}

func TestSSEErrorOnlyDecodesErrorEvents(t *testing.T) {
	for _, c := range []struct {
		event, data string
		want        bool
		status      int
	}{
		{"", `{"choices":[{"delta":{"content":"an \"error\" in text"}}]}`, false, 0},
		{"", `{"error":{"message":"boom","code":429}}`, true, 429},
		{"error", `{"message":"overloaded"}`, true, 502},
		{"", `{"type":"response.failed","response":{"status_code":500}}`, true, 500},
		{"", `{"type":"error","error":{"type":"overloaded_error"}}`, true, 502},
		{"", `{"choices":[],"error":null}`, false, 0},
	} {
		got, st, _ := sseError(c.event, []byte(c.data))
		if got != c.want || st != c.status {
			t.Errorf("sseError(%q, %s) = %v %d, want %v %d", c.event, c.data, got, st, c.want, c.status)
		}
	}
}

func TestIdentityFields(t *testing.T) {
	body := func(s string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	b := body(`{"model":"m1","Model":"other",
		"metadata":{"session_id":"s1"},
		"prompt_cache_key":7,
		"messages":[42,{"role":"system","content":[{"type":"text","text":"sys"},{"text":null},"x"]},
			{"role":"user","content":"hello"},{"role":"user","content":"later"}]}`)
	if v := lookup(b, "prompt_cache_key"); v != "" {
		t.Errorf("a number read as string: %q", v)
	}
	if v := lookup(b, "metadata.session_id"); v != "s1" {
		t.Errorf("metadata.session_id = %q", v)
	}
	if v := lookup(b, "model.x"); v != "" {
		t.Errorf("path through a string = %q", v)
	}
	if got, want := stableConversation(b), `["sys\n","hello"]`; got != want {
		t.Errorf("stableConversation = %s, want %s", got, want)
	}
	resp := body(`{"instructions":"be brief","input":[{"role":"user","content":[{"type":"input_text","text":"q"}]}]}`)
	if got, want := stableConversation(resp), `["be brief","q\n"]`; got != want {
		t.Errorf("responses stableConversation = %s, want %s", got, want)
	}
	if got, want := stableConversation(body(`{"system":"s","input":"just text"}`)), `["s","just text"]`; got != want {
		t.Errorf("string input stableConversation = %s, want %s", got, want)
	}
}

// LogKeys must equal what the store reads back from a record's JSON.
func TestLogKeysMatchTheirJSON(t *testing.T) {
	us := 503
	recs := map[string]logstore.Keyed{
		"requests": &requestLog{RequestID: "req_1", AccountID: "a", Status: 200, Result: "success", Model: "m1",
			BindingResult: "hit", ClientKeyID: "ck1", Stream: true, UpstreamStatus: &us},
		"errors": errorLog{RequestID: "req_2", AccountID: "b", Status: 429, Category: "upstream", RuleID: "r1", Model: "m2"},
	}
	recs["requests-empty"] = &requestLog{}
	for name, rec := range recs {
		kind, _, _ := strings.Cut(name, "-")
		data, _ := json.Marshal(rec)
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		keys := rec.LogKeys()
		for _, f := range logstore.Filters[kind] {
			want := ""
			switch v := m[f].(type) {
			case string:
				want = v
			case float64:
				want = strconv.FormatFloat(v, 'f', -1, 64)
			case bool:
				want = strconv.FormatBool(v)
			}
			if keys[f] != want {
				t.Errorf("%s %s: LogKeys %q, JSON %q", name, f, keys[f], want)
			}
		}
	}
}

func TestModelsCachedPerGroupAndShared(t *testing.T) {
	g2Key := "sk-odb-test-client-key-0002"
	e := newEnv(t, []config.Account{{ID: "a", Group: "default"}, {ID: "b", Group: "g2"}}, func(c *config.Config) {
		c.ClientKeys = append(c.ClientKeys, config.ClientKey{ID: "ck2", Group: "g2", KeyHash: config.HashKey(g2Key), KeyPrefix: "y", Enabled: true})
	})
	var reads atomic.Int32
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		reads.Add(1)
		time.Sleep(100 * time.Millisecond) // concurrent misses overlap
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"object":"list","data":[{"id":"only-%s"}]}`, key)
	}
	list := func(clientKey string) string {
		req, _ := http.NewRequest(http.MethodGet, e.front.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+clientKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return ""
		}
		return readAll(resp)
	}
	var wg sync.WaitGroup
	got := make([]string, 5)
	for i := range got {
		wg.Go(func() { got[i] = list(e.key) })
	}
	wg.Wait()
	for _, g := range got {
		if !strings.Contains(g, "only-sk-amr-a-") {
			t.Fatalf("default group got %s", g)
		}
	}
	if n := reads.Load(); n != 1 {
		t.Fatalf("5 concurrent misses made %d upstream reads, want 1", n)
	}
	if g := list(g2Key); !strings.Contains(g, "only-sk-amr-b-") {
		t.Fatalf("g2 got another group's catalogue: %s", g)
	}
	if n := reads.Load(); n != 2 {
		t.Fatalf("upstream reads = %d, want 2", n)
	}
	list(e.key)
	if n := reads.Load(); n != 2 {
		t.Fatalf("a cached group was read again: %d", n)
	}
}

func TestStreamUsageAfterNullUsageChunks(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}}, nil)
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		for range 3 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}],\"usage\":null}\n\n")
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
	}
	resp := e.post("/v1/chat/completions", chat(true), nil)
	if body := readAll(resp); resp.StatusCode != 200 || strings.Count(body, `"usage":null`) != 3 {
		t.Fatalf("stream not relayed: %d %s", resp.StatusCode, body)
	}
	logs := e.requestLogs()
	if len(logs) != 1 || logs[0]["inputTokens"] != float64(11) || logs[0]["outputTokens"] != float64(3) {
		t.Fatalf("usage not read from the last chunk: %v", logs)
	}
}

func TestAgentSessionHeadersAreExplicit(t *testing.T) {
	s := &Server{secret: []byte("k")}
	cfg := config.Default()
	for _, name := range []string{"x-deepseek-harness-session-id", "X-Opencode-Session", "Agent-Session-Id", "x-litellm-session-id", "X-Task-ID", "X-KILOCODE-TASKID", "x-grok-conv-id", "X-Session-Affinity"} {
		h := http.Header{}
		h.Set(name, "abc-123")
		id := s.identity(cfg, "g", h, map[string]any{})
		if id.Confidence != "explicit" || id.Source != "header" {
			t.Errorf("%s: %+v", name, id)
		}
	}
	body := map[string]any{"metadata": map[string]any{"sessionId": "qwen-1"}}
	if id := s.identity(cfg, "g", http.Header{}, body); id.Confidence != "explicit" || id.KeyType != "session_id" {
		t.Errorf("metadata.sessionId: %+v", id)
	}
}
