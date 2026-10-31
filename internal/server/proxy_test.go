package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"opendesign-balancer/internal/config"
	"opendesign-balancer/internal/quota"
	"opendesign-balancer/internal/rules"
)

type mockUpstream struct {
	mu     sync.Mutex
	calls  []string // runtime key of each call
	handle func(w http.ResponseWriter, r *http.Request, key string, body map[string]any)
}

func (m *mockUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	m.mu.Lock()
	m.calls = append(m.calls, key)
	h := m.handle
	m.mu.Unlock()
	h(w, r, key, body)
}

func (m *mockUpstream) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

type env struct {
	t      *testing.T
	srv    *Server
	front  *httptest.Server
	up     *mockUpstream
	key    string
	cancel context.CancelFunc
}

const secretMarker = "TOPSECRET-PROMPT-TEXT"

func newEnv(t *testing.T, accts []config.Account, tweak func(c *config.Config)) *env {
	t.Helper()
	up := &mockUpstream{handle: func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		writeOK(w, key)
	}}
	upSrv := httptest.NewServer(up)
	t.Cleanup(upSrv.Close)
	s, err := New(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	clientKey := "sk-odb-test-client-key-0001"
	_, err = s.cfg.Update(-1, func(c *config.Config) error {
		c.Upstream.RuntimeBase = upSrv.URL + "/v1"
		c.Upstream.ControlBase = upSrv.URL
		c.Quota.Enabled = false
		c.Routing.Strategy = "least-selections"
		c.Routing.ConcurrencyWaitMs = 0
		c.ClientKeys = []config.ClientKey{{ID: "ck1", Group: "default", KeyHash: config.HashKey(clientKey), KeyPrefix: "x", Enabled: true}}
		for i := range accts {
			accts[i].Enabled = true
			if accts[i].RuntimeKey == "" {
				accts[i].RuntimeKey = "sk-amr-" + accts[i].ID + "-runtime-secret"
			}
		}
		c.Accounts = accts
		if tweak != nil {
			tweak(c)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go s.quota.Run(ctx)
	front := httptest.NewServer(s.Handler())
	t.Cleanup(front.Close)
	t.Cleanup(cancel)
	for !s.logs.Ready() {
		time.Sleep(5 * time.Millisecond)
	}
	return &env{t: t, srv: s, front: front, up: up, key: clientKey, cancel: cancel}
}

func writeOK(w http.ResponseWriter, key string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi from %s"}}],"usage":{"prompt_tokens":11,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":5}}}`, key[len(key)-20:len(key)-15])
}

func (e *env) post(path string, body map[string]any, headers map[string]string) *http.Response {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, e.front.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func chat(stream bool) map[string]any {
	return map[string]any{"model": "m1", "stream": stream, "messages": []any{map[string]any{"role": "user", "content": secretMarker}}}
}

func (e *env) requestLogs() []map[string]any {
	e.t.Helper()
	page, err := e.srv.logs.Query("requests", nil, time.Time{}, time.Time{}, 200, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []map[string]any
	for _, raw := range page.Items {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		out = append(out, m)
	}
	return out
}

func (e *env) allLogText() string {
	var b strings.Builder
	for _, kind := range []string{"requests", "errors"} {
		page, _ := e.srv.logs.Query(kind, nil, time.Time{}, time.Time{}, 200, 0)
		for _, raw := range page.Items {
			b.Write(raw)
		}
	}
	return b.String()
}

func readAll(resp *http.Response) string {
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(b)
}

func TestNonStreamSuccessAndLogsClean(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}}, nil)
	resp := e.post("/v1/chat/completions", chat(false), map[string]string{"X-Claude-Code-Session-Id": "session-value-xyz"})
	body := readAll(resp)
	if resp.StatusCode != 200 || !strings.Contains(body, "hi from") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Fatal("missing X-Request-Id")
	}
	logs := e.requestLogs()
	if len(logs) != 1 || logs[0]["inputTokens"].(float64) != 11 || logs[0]["cacheHit"] != true || logs[0]["affinityConfidence"] != "explicit" {
		t.Fatalf("request log = %v", logs)
	}
	text := e.allLogText()
	for _, secret := range []string{"sk-amr-a-runtime-secret", "session-value-xyz", secretMarker, e.key} {
		if strings.Contains(text, secret) {
			t.Fatalf("log leaks %q", secret)
		}
	}
}

func TestBadClientKey(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}}, nil)
	e.key = "sk-wrong"
	resp := e.post("/v1/chat/completions", chat(false), nil)
	readAll(resp)
	if resp.StatusCode != 401 || e.up.callCount() != 0 {
		t.Fatalf("status %d calls %d", resp.StatusCode, e.up.callCount())
	}
}

func TestUpstream401SwitchesAndNeverReaches401(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}, {ID: "b"}}, nil)
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_api_key","message":"[code=invalid_api_key] api key not found"}}`))
	}
	resp := e.post("/v1/chat/completions", chat(false), nil)
	body := readAll(resp)
	if resp.StatusCode != 502 || !strings.Contains(body, "upstream_account_auth_failed") {
		t.Fatalf("want 502, got %d %s", resp.StatusCode, body)
	}
	if e.up.callCount() != 2 {
		t.Fatalf("want one switch (2 calls), got %d", e.up.callCount())
	}
	if e.up.calls[0] == e.up.calls[1] {
		t.Fatal("switch re-selected the same account")
	}
	logs := e.requestLogs()
	if logs[0]["upstreamStatus"].(float64) != 401 || logs[0]["switched"] != true {
		t.Fatalf("log = %v", logs[0])
	}
	views := e.srv.pool.Views(e.srv.cfg.Get())
	if views["a"].Status != "quarantined" || views["b"].Status != "quarantined" {
		t.Fatalf("invalid keys must be quarantined: %+v", views)
	}
}

func TestSwitchToHealthyAccount(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a", Priority: 10}, {ID: "b"}}, nil)
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		if strings.Contains(key, "-a-") {
			w.WriteHeader(402)
			_, _ = w.Write([]byte(`{"error":{"code":"insufficient_balance","message":"insufficient_balance"}}`))
			return
		}
		writeOK(w, key)
	}
	for range 3 {
		resp := e.post("/v1/chat/completions", chat(false), nil)
		readAll(resp)
		if resp.StatusCode != 200 {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	// a's model cooldown keeps it out after the first failure
	views := e.srv.pool.Views(e.srv.cfg.Get())
	if views["a"].State == nil || views["a"].State.Models["m1"] == nil {
		t.Fatalf("insufficient_balance must cool the model on a: %+v", views["a"])
	}
	if n := e.up.callCount(); n != 4 {
		t.Fatalf("calls = %d, want 4 (one failed attempt on a)", n)
	}
}

func TestWrappedBadRequestIsRestoredAndCoolsNothing(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}, {ID: "b"}}, nil)
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":{"message":"provider returned status code 400: messages must not be empty"}}`))
	}
	resp := e.post("/v1/chat/completions", chat(false), nil)
	readAll(resp)
	if resp.StatusCode != 400 {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
	if e.up.callCount() != 1 {
		t.Fatalf("a request error must not switch: %d calls", e.up.callCount())
	}
	for id, v := range e.srv.pool.Views(e.srv.cfg.Get()) {
		if v.Status != "ok" || v.Samples != 0 {
			t.Fatalf("%s affected by a request error: %+v", id, v)
		}
	}
}

func TestStreamNoRetryAfterFirstByte(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}, {ID: "b"}}, nil)
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		// the connection breaks mid-stream
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}
	resp := e.post("/v1/chat/completions", chat(true), nil)
	body := readAll(resp)
	if resp.StatusCode != 200 || !strings.Contains(body, "he") {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	if e.up.callCount() != 1 {
		t.Fatalf("replayed after first byte: %d calls", e.up.callCount())
	}
	if l := e.requestLogs()[0]; l["result"] != "failed" || l["ttftMs"] == nil {
		t.Fatalf("log = %v", l)
	}
}

func TestStreamFirstEventErrorSwitches(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a", Priority: 10}, {ID: "b"}}, nil)
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(key, "-a-") {
			_, _ = w.Write([]byte("data: {\"error\":{\"code\":\"model_limit_exceeded\",\"message\":\"limit\"}}\n\n"))
			return
		}
		_, _ = w.Write([]byte(": keepalive\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"))
	}
	var got string
	for range 2 {
		resp := e.post("/v1/chat/completions", chat(true), nil)
		got = readAll(resp)
		if resp.StatusCode != 200 || strings.Contains(got, "model_limit_exceeded") || !strings.Contains(got, "[DONE]") {
			t.Fatalf("status %d body %q", resp.StatusCode, got)
		}
	}
	logs := e.requestLogs()
	found := false
	for _, l := range logs {
		if l["switched"] == true && l["outputTokens"].(float64) == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no switched stream with usage in %v", logs)
	}
}

func TestClientCancelIs499WithoutSamples(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}}, nil)
	release := make(chan struct{})
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	b, _ := json.Marshal(chat(false))
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.front.URL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+e.key)
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("expected client timeout")
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(e.requestLogs()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	logs := e.requestLogs()
	if len(logs) != 1 || logs[0]["status"].(float64) != 499 || logs[0]["result"] != "client_cancelled" {
		t.Fatalf("log = %v", logs)
	}
	if v := e.srv.pool.Views(e.srv.cfg.Get())["a"]; v.Samples != 0 || v.Active != 0 {
		t.Fatalf("cancel wrote samples or leaked a lease: %+v", v)
	}
	page, _ := e.srv.logs.Query("errors", nil, time.Time{}, time.Time{}, 10, 0)
	if len(page.Items) != 0 {
		t.Fatal("client cancel must not write an error log")
	}
}

func TestStickySessionAcrossRequests(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}, {ID: "b"}, {ID: "c"}}, nil)
	for range 6 {
		readAll(e.post("/v1/chat/completions", chat(false), map[string]string{"Session_id": "codex-thread-1"}))
	}
	first := e.up.calls[0]
	for _, k := range e.up.calls {
		if k != first {
			t.Fatalf("session moved between accounts: %v", e.up.calls)
		}
	}
	// without a session the strategy spreads
	e.srv.cfg.Update(-1, func(c *config.Config) error { c.Routing.FallbackHash = false; return nil })
	e.up.calls = nil
	for range 6 {
		readAll(e.post("/v1/chat/completions", chat(false), nil))
	}
	seen := map[string]bool{}
	for _, k := range e.up.calls {
		seen[k] = true
	}
	if len(seen) != 3 {
		t.Fatalf("least-selections did not spread: %v", e.up.calls)
	}
}

func TestCapacity429WithRetryAfter(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a", MaxConc: 1}}, nil)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		started <- struct{}{}
		<-release
		writeOK(w, key)
	}
	done := make(chan int)
	go func() {
		resp := e.post("/v1/chat/completions", chat(false), nil)
		readAll(resp)
		done <- resp.StatusCode
	}()
	<-started
	resp := e.post("/v1/chat/completions", chat(false), nil)
	readAll(resp)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("want local 429 with Retry-After, got %d", resp.StatusCode)
	}
	close(release)
	if st := <-done; st != 200 {
		t.Fatalf("first request %d", st)
	}
	if v := e.srv.pool.Views(e.srv.cfg.Get())["a"]; v.Samples != 1 {
		t.Fatalf("local capacity 429 must not be a health sample: %+v", v)
	}
}

func TestResponsesPreviousResponseAffinity(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}, {ID: "b"}}, func(c *config.Config) { c.Routing.FallbackHash = false })
	n := 0
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		n++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"resp_%d","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`, n)
	}
	readAll(e.post("/v1/responses", map[string]any{"model": "m1", "input": "x"}, nil))
	firstKey := e.up.calls[0]
	for i := range 4 {
		readAll(e.post("/v1/responses", map[string]any{"model": "m1", "input": "y", "previous_response_id": fmt.Sprintf("resp_%d", i+1)}, nil))
	}
	for _, k := range e.up.calls {
		if k != firstKey {
			t.Fatalf("previous_response_id chain left its account: %v", e.up.calls)
		}
	}
}

func TestAnthropicErrorShape(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}}, nil)
	e.srv.pool.Quarantine("a", 401, "x", "x")
	resp := e.post("/v1/messages", map[string]any{"model": "m1", "max_tokens": 5, "messages": []any{}}, nil)
	body := readAll(resp)
	if resp.StatusCode != 503 || !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestModelWindowLimitSwitchesThen429(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}, {ID: "b"}}, nil)
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		reset := time.Now().Add(90 * time.Minute).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `{"error":{"code":"model_limit_exceeded","message":"You have reached the 5-hour usage limit for m1. Try again after %s. This request was not charged to Wallet Credits."}}`, reset)
	}
	resp := e.post("/v1/chat/completions", chat(false), nil)
	body := readAll(resp)
	if e.up.callCount() != 2 {
		t.Fatalf("a 400 model window limit must switch despite the bad-request retry rule: %d calls", e.up.callCount())
	}
	ra, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
	if resp.StatusCode != 429 || ra < 85*60 || strings.Contains(body, "Wallet") {
		t.Fatalf("want 429 with ~90m Retry-After, got %d %q %s", resp.StatusCode, resp.Header.Get("Retry-After"), body)
	}
	// both cooled for m1 only: the next m1 request is a local 429, m2 still goes
	resp = e.post("/v1/chat/completions", chat(false), nil)
	readAll(resp)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" || e.up.callCount() != 2 {
		t.Fatalf("cooling pool must answer 429 locally: %d calls=%d", resp.StatusCode, e.up.callCount())
	}
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) { writeOK(w, key) }
	other := chat(false)
	other["model"] = "m2"
	resp = e.post("/v1/chat/completions", other, nil)
	readAll(resp)
	if resp.StatusCode != 200 {
		t.Fatalf("other model blocked: %d", resp.StatusCode)
	}
}

func TestMembershipConcurrencyIsLearned(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a", Priority: 10}, {ID: "b"}}, nil)
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		if strings.Contains(key, "-a-") {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"code":"tier_limit_exceeded","message":"membership concurrency limit exceeded 6/5"}}`))
			return
		}
		writeOK(w, key)
	}
	resp := e.post("/v1/chat/completions", chat(false), nil)
	readAll(resp)
	if resp.StatusCode != 200 || e.up.callCount() != 2 {
		t.Fatalf("status %d calls %d", resp.StatusCode, e.up.callCount())
	}
	st := e.srv.pool.Views(e.srv.cfg.Get())["a"].State
	if st == nil || st.LearnedConcurrency != 5 || st.CooldownUntil.IsZero() {
		t.Fatalf("limit not learned: %+v", st)
	}
}

func TestEntitlementAndPlatformFaultsNeverLookLikeAuth(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}}, nil)
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":{"code":"tier_model_not_entitled"}}`))
	}
	resp := e.post("/v1/chat/completions", chat(false), nil)
	body := readAll(resp)
	if resp.StatusCode != 503 || strings.Contains(body, "auth") {
		t.Fatalf("not entitled must be 503, got %d %s", resp.StatusCode, body)
	}
	if v := e.srv.pool.Views(e.srv.cfg.Get())["a"]; v.Status != "ok" || v.State.Models["m1"] == nil {
		t.Fatalf("must cool only the model: %+v", v)
	}

	e2 := newEnv(t, []config.Account{{ID: "a"}, {ID: "b"}}, nil)
	e2.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":{"code":"upstream_provider_unauthenticated","message":"Upstream provider credentials are missing or invalid."}}`))
	}
	resp = e2.post("/v1/chat/completions", chat(false), nil)
	readAll(resp)
	if resp.StatusCode != 502 || e2.up.callCount() != 1 {
		t.Fatalf("platform fault: status %d calls %d", resp.StatusCode, e2.up.callCount())
	}
	for id, v := range e2.srv.pool.Views(e2.srv.cfg.Get()) {
		if v.Samples != 0 || v.Status != "ok" {
			t.Fatalf("platform fault blamed account %s: %+v", id, v)
		}
	}
}

func TestCatalogGateAndFilteredModels(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}, {ID: "b"}}, nil)
	now := time.Now()
	e.srv.quota.Load(map[string]*quota.Snapshot{
		"a": {CatalogAt: now, Catalog: map[string]bool{"m1": true, "big": false}},
		"b": {CatalogAt: now, Catalog: map[string]bool{"m1": true, "big": true}},
	})
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"},{"id":"big"},{"id":"nobody"}]}`))
			return
		}
		writeOK(w, key)
	}
	big := chat(false)
	big["model"] = "big"
	for range 4 {
		readAll(e.post("/v1/chat/completions", big, nil))
	}
	for _, k := range e.up.calls {
		if strings.Contains(k, "-a-") {
			t.Fatalf("a's plan lacks big but it was called: %v", e.up.calls)
		}
	}
	nobody := chat(false)
	nobody["model"] = "nobody"
	resp := e.post("/v1/chat/completions", nobody, nil)
	body := readAll(resp)
	if resp.StatusCode != 503 || !strings.Contains(body, "model_not_entitled") {
		t.Fatalf("want 503 model_not_entitled, got %d %s", resp.StatusCode, body)
	}
	req, _ := http.NewRequest(http.MethodGet, e.front.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+e.key)
	mr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	list := readAll(mr)
	if !strings.Contains(list, `"m1"`) || !strings.Contains(list, `"big"`) || strings.Contains(list, "nobody") {
		t.Fatalf("models not narrowed to the pool's plans: %s", list)
	}
}

func TestInsufficientBalanceByFundingClass(t *testing.T) {
	// one conversation would stay on b once a refused it; these are separate ones
	e := newEnv(t, []config.Account{{ID: "a", Priority: 10}, {ID: "b"}}, func(c *config.Config) { c.Routing.FallbackHash = false })
	now := time.Now()
	cat := map[string]bool{"deepseek-v4.1-flash": true, "claude-sonnet-5.5": true, "gpt-6-sol": true}
	e.srv.quota.Load(map[string]*quota.Snapshot{"a": {CatalogAt: now, Catalog: cat}, "b": {CatalogAt: now, Catalog: cat}})
	broke := map[string]bool{} // models a can't pay for
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		if strings.Contains(key, "-a-") && broke[body["model"].(string)] {
			w.WriteHeader(402)
			_, _ = w.Write([]byte(`{"error":{"code":"insufficient_balance","message":"Insufficient wallet balance."}}`))
			return
		}
		writeOK(w, key)
	}
	ask := func(model string) {
		c := chat(false)
		c["model"] = model
		resp := e.post("/v1/chat/completions", c, nil)
		readAll(resp)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", model, resp.StatusCode)
		}
	}
	broke["claude-sonnet-5.5"] = true
	ask("claude-sonnet-5.5") // a refuses, b answers
	v := e.srv.pool.Views(e.srv.cfg.Get())["a"]
	if v.State == nil || v.State.Classes["package"] == nil || !v.State.CooldownUntil.IsZero() {
		t.Fatalf("a 套餐模型 refused for money must cool the package class only: %+v", v.State)
	}
	n := e.up.callCount()
	ask("gpt-6-sol") // another 套餐模型: a is skipped without a call
	if e.up.callCount() != n+1 {
		t.Fatalf("a was tried again for a 套餐模型")
	}
	ask("deepseek-v4.1-flash") // Design Plan model: a (priority) still serves
	if !strings.Contains(e.up.calls[len(e.up.calls)-1], "-a-") {
		t.Fatal("a Design Plan model must still go to a")
	}
	broke["deepseek-v4.1-flash"] = true
	e.srv.pool.Recover("a")
	ask("deepseek-v4.1-flash") // now a Design Plan model is refused: the whole account
	v = e.srv.pool.Views(e.srv.cfg.Get())["a"]
	if v.State == nil || v.State.CooldownUntil.IsZero() || v.State.Strikes["plan"] != 1 {
		t.Fatalf("a Design Plan model refused for money must cool the account: %+v", v.State)
	}
}

func TestDetailLogOffByDefaultAndMasksWhenOn(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}}, nil)
	readAll(e.post("/v1/chat/completions", chat(false), nil))
	if page, _ := e.srv.logs.Query("details", nil, time.Time{}, time.Time{}, 10, 0); len(page.Items) != 0 {
		t.Fatalf("detail log written while off: %v", page.Items)
	}
	if logs := e.requestLogs(); logs[0]["protocol"] != "openai-chat" {
		t.Fatalf("protocol = %v", logs[0]["protocol"])
	}

	if _, err := e.srv.cfg.Update(-1, func(c *config.Config) error { c.Logs.Detail = true; return nil }); err != nil {
		t.Fatal(err)
	}
	readAll(e.post("/v1/chat/completions", chat(true), map[string]string{"X-Custom": "v1"}))
	page, _ := e.srv.logs.Query("details", nil, time.Time{}, time.Time{}, 10, 0)
	if len(page.Items) != 1 {
		t.Fatalf("details = %d", len(page.Items))
	}
	var d detailLog
	_ = json.Unmarshal(page.Items[0], &d)
	if d.Protocol != "openai-chat" || d.Status != 200 || !strings.Contains(d.RequestBody, secretMarker) || d.RequestHeaders["X-Custom"][0] != "v1" || d.ResponseBodyBytes == 0 || d.ResponseBody == "" {
		t.Fatalf("detail = %+v", d)
	}
	if d.RequestHeaders["Authorization"][0] != "[REDACTED]" || strings.Contains(string(page.Items[0]), e.key) {
		t.Fatalf("credential not masked: %v", d.RequestHeaders)
	}
}

func TestClientClosingAfterDoneIsNotCancelled(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}}, nil)
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // upstream lingers: no EOF until the proxy lets go
	}
	resp := e.post("/v1/chat/completions", chat(true), nil)
	rd := bufio.NewReader(resp.Body)
	for {
		line, err := rd.ReadString('\n')
		if err != nil || strings.HasPrefix(line, "data: [DONE]") {
			break
		}
	}
	resp.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(e.requestLogs()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	logs := e.requestLogs()
	if len(logs) != 1 || logs[0]["status"].(float64) != 200 || logs[0]["result"] != "success" {
		t.Fatalf("log = %v", logs)
	}
}

func TestRequestLogCarriesCacheRateAndCost(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "a"}}, nil)
	// writeOK reports 11 prompt tokens (5 cached) and 3 completion tokens
	readAll(e.post("/v1/chat/completions", map[string]any{"model": "claude-opus-5", "messages": []any{map[string]any{"role": "user", "content": "x"}}}, nil))
	readAll(e.post("/v1/chat/completions", map[string]any{"model": "unpriced-model", "messages": []any{map[string]any{"role": "user", "content": "y"}}}, nil))
	byModel := map[string]map[string]any{}
	for _, l := range e.requestLogs() {
		byModel[l["model"].(string)] = l
	}
	l := byModel["claude-opus-5"]
	want := (6*5 + 5*0.5 + 3*25) / 1e6
	if l["currency"] != "USD" || l["cacheRate"].(float64) != 5.0/11 || l["cost"].(float64)-want > 1e-12 || want-l["cost"].(float64) > 1e-12 {
		t.Fatalf("priced log = %v", l)
	}
	if u := byModel["unpriced-model"]; u["cost"] != nil || u["cacheRate"] == nil {
		t.Fatalf("unpriced log = %v", u)
	}
}

func TestDetailCaptureKeepsHeadAndTail(t *testing.T) {
	cw := &captureWriter{ResponseWriter: httptest.NewRecorder(), max: 20}
	for _, part := range []string{"0123456789", "abcdefghij", "ABCDEFGHIJ", "klmnopqrst"} {
		cw.Write([]byte(part))
	}
	b, cut := cw.body()
	s := string(b)
	if !cut || !strings.HasPrefix(s, "0123456789") || !strings.HasSuffix(s, "klmnopqrst") || !strings.Contains(s, "省略 20 字节") {
		t.Fatalf("body = %q cut=%v", s, cut)
	}
	short := &captureWriter{ResponseWriter: httptest.NewRecorder(), max: 20}
	short.Write([]byte("hello"))
	if b, cut := short.body(); cut || string(b) != "hello" {
		t.Fatalf("short body = %q cut=%v", b, cut)
	}
	if b, cut := clipMiddle([]byte("0123456789abcdefghij0123"), 10); !cut || !strings.HasPrefix(string(b), "01234") || !strings.HasSuffix(string(b), "ij0123"[1:]) {
		t.Fatalf("clipMiddle = %q", b)
	}
}

func TestStripUnsupportedTool(t *testing.T) {
	errBody := []byte(`{"error":{"message":"[code=unsupported_tool] unsupported Responses tool type \"web_search\" at tools[1] on Chat route for model \"x\"","code":"unsupported_tool"}}`)
	body := []byte(`{"model":"x","tools":[{"type":"function","function":{"name":"a"}},{"type":"web_search"}],"tool_choice":"auto"}`)
	nb, ok := stripUnsupportedTool(body, errBody)
	if !ok || strings.Contains(string(nb), "web_search") || !strings.Contains(string(nb), `"name":"a"`) || !strings.Contains(string(nb), "tool_choice") {
		t.Fatalf("got %v %s", ok, nb)
	}
	nb, ok = stripUnsupportedTool([]byte(`{"model":"x","tools":[{"type":"web_search"}],"tool_choice":"auto"}`), errBody)
	if !ok || strings.Contains(string(nb), "tools") || strings.Contains(string(nb), "tool_choice") {
		t.Fatalf("got %v %s", ok, nb)
	}
	if _, ok := stripUnsupportedTool(body, []byte(`{"error":"bad"}`)); ok {
		t.Fatal("unexpected strip")
	}
}

// River offers only OpenAI Chat Completions and Responses: other endpoints
// (Anthropic Messages, embeddings…) are refused locally, without a call upstream.
func TestRiverPoolOnlyOpenAIEndpoints(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "r1", Group: "default", RuntimeKey: "rv_aaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, func(c *config.Config) {
		c.Upstream.RiverBase = c.Upstream.RuntimeBase
	})
	for _, path := range []string{"/v1/messages", "/v1/embeddings"} {
		resp := e.post(path, chat(false), nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", path, resp.StatusCode)
		}
	}
	if n := e.up.callCount(); n != 0 {
		t.Fatalf("%d upstream calls for unsupported endpoints", n)
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		resp := e.post(path, chat(false), nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", path, resp.StatusCode)
		}
	}
}

// Each pool judges failures by its own rules: a 402 cools a River account but
// means nothing special to an OpenDesign one.
func TestRulesAreSeparatePerPool(t *testing.T) {
	c := config.Default()
	od, _ := c.RulesFor(config.ProviderOpenDesign)
	rv, _ := c.RulesFor(config.ProviderRiver)
	has := func(rs []rules.ErrorRule, id string) bool {
		for _, r := range rs {
			if r.ID == id {
				return true
			}
		}
		return false
	}
	if !has(rv, "river-insufficient-balance") || has(od, "river-insufficient-balance") {
		t.Fatal("river rules missing or leaked into opendesign")
	}
	if !has(od, "model-window-limit") || has(rv, "model-window-limit") {
		t.Fatal("opendesign rules missing or leaked into river")
	}
	c.RiverErrorRules = nil
	c.Accounts = nil
	if err := c.Normalize(); err != nil || len(c.RiverErrorRules) == 0 {
		t.Fatalf("river rules not defaulted: %v", err)
	}
}

// An Ollama pool serves all three protocols this proxy speaks: OpenAI Chat
// Completions, OpenAI Responses and Anthropic Messages, all under /v1 of the
// same base, with the key in Authorization only.
func TestOllamaPoolServesThreeProtocols(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]http.Header{}
	e := newEnv(t, []config.Account{{ID: "o1", Provider: config.ProviderOllama, Group: "ollama", RuntimeKey: "opaque-ollama-key-should-reach-upstream"}}, func(c *config.Config) {
		c.Upstream.OllamaBase = c.Upstream.RuntimeBase
		c.ClientKeys = []config.ClientKey{{ID: "ck2", Group: "ollama", KeyHash: config.HashKey("sk-odb-ollama"), KeyPrefix: "x", Enabled: true}}
	})
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Clone()
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":7,"output_tokens":2}}`)
		default:
			writeOK(w, key)
		}
	}
	e.key = "sk-odb-ollama"

	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		hdr := map[string]string{}
		if path == "/v1/messages" {
			hdr["anthropic-version"] = "2023-06-01"
		}
		resp := e.post(path, chat(false), hdr)
		body := readAll(resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d body %s", path, resp.StatusCode, body)
		}
		mu.Lock()
		h := seen[path]
		mu.Unlock()
		if h == nil {
			t.Fatalf("%s never reached the upstream", path)
		}
		if got := h.Get("Authorization"); got != "Bearer opaque-ollama-key-should-reach-upstream" {
			t.Fatalf("%s: Authorization %q", path, got)
		}
		// ollama.com refuses x-api-key on its own, so it must not be sent
		if got := h.Get("X-Api-Key"); got != "" {
			t.Fatalf("%s: unexpected x-api-key %q", path, got)
		}
		if path == "/v1/messages" && h.Get("Anthropic-Version") == "" {
			t.Fatalf("%s: missing anthropic-version", path)
		}
	}
	// embeddings are served too, but a path Ollama doesn't have is refused locally
	if resp := e.post("/v1/embeddings", chat(false), nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("embeddings: status %d", resp.StatusCode)
	}
	for _, path := range []string{"/v1/batches", "/v1/fine_tuning/jobs"} {
		before := e.up.callCount()
		resp := e.post(path, chat(false), nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", path, resp.StatusCode)
		}
		if e.up.callCount() != before {
			t.Fatalf("%s reached the upstream", path)
		}
	}
}

// Ollama reports a refused key as {"error":"invalid credentials"}; that has to
// be read as a 401 so the stock rule quarantines the account instead of
// cooling it down for five minutes: only the next request differs.
func TestOllamaRefusedKeyQuarantines(t *testing.T) {
	e := newEnv(t, []config.Account{
		{ID: "o1", Provider: config.ProviderOllama, Group: "ollama", RuntimeKey: "dead-ollama-key-aaaaaaaaaaaaaaaa"},
	}, func(c *config.Config) {
		c.Upstream.OllamaBase = c.Upstream.RuntimeBase
		c.ClientKeys = []config.ClientKey{{ID: "ck2", Group: "ollama", KeyHash: config.HashKey("sk-odb-ollama"), KeyPrefix: "x", Enabled: true}}
	})
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid credentials"}`))
	}
	e.key = "sk-odb-ollama"
	resp := e.post("/v1/chat/completions", chat(false), nil)
	body := readAll(resp)
	// an account problem never reaches the client as a 401: a relay such as
	// new-api would disable the whole channel
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	st := e.srv.pool.Views(e.srv.cfg.Get())["o1"].State
	if st == nil || !st.HardQuarantined || st.RuleID != "ollama-key-invalid" {
		t.Fatalf("dead key not quarantined: %+v", st)
	}
	page, err := e.srv.logs.Query("errors", nil, time.Time{}, time.Time{}, 50, 0)
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("error log: %v %d", err, len(page.Items))
	}
	var el map[string]any
	_ = json.Unmarshal(page.Items[0], &el)
	if el["ruleId"] != "ollama-key-invalid" || el["provider"] != config.ProviderOllama || el["status"].(float64) != 401 {
		t.Fatalf("error log = %v", el)
	}
	// the quarantined account takes no further requests
	before := e.up.callCount()
	resp2 := e.post("/v1/chat/completions", chat(false), nil)
	resp2.Body.Close()
	if e.up.callCount() != before {
		t.Fatal("quarantined account was called again")
	}
}

// The Ollama pool's rules are its own: a 402 with a balance message cools the
// account rather than quarantining it, and a 429 backs off by Retry-After.
func TestOllamaErrorRules(t *testing.T) {
	c := config.Default()
	rs := config.DefaultErrorRulesFor(config.ProviderOllama)
	h := http.Header{}
	h.Set("Retry-After", "45")
	limited := rules.MatchError(rs, 429, `{"error":"rate limit exceeded"}`, h)
	if limited == nil || limited.ID != "ollama-rate-limited" {
		t.Fatalf("429 rule = %+v", limited)
	}
	if d := rules.Cooldown(limited, h, "", time.Now()); d != 45*time.Second {
		t.Fatalf("cooldown = %v", d)
	}
	if r := rules.MatchError(rs, 401, `{"error":"invalid credentials"}`, http.Header{}); r == nil || r.ID != "ollama-key-invalid" {
		t.Fatalf("401 rule = %+v", r)
	}
	if r := rules.MatchError(rs, 502, `{"error":"cloud model unavailable"}`, http.Header{}); r == nil || r.ID != "ollama-unavailable" {
		t.Fatalf("502 rule = %+v", r)
	}
	// the OpenDesign rules know nothing of these ids
	od, _ := c.RulesFor(config.ProviderOpenDesign)
	if r := rules.MatchError(od, 429, `{"error":"rate limit exceeded"}`, h); r != nil && r.ID == "ollama-rate-limited" {
		t.Fatal("ollama rule leaked into opendesign")
	}
}

// A pool is its own pool even before its first account arrives: its key
// keeps its pool, and its key list and rules stay on it.
func TestPoolsSeparateBeforeFirstAccount(t *testing.T) {
	e := newEnv(t, []config.Account{{ID: "d1"}}, func(c *config.Config) {
		// an Ollama key whose group holds no account yet
		c.ClientKeys = append(c.ClientKeys, config.ClientKey{ID: "ck-ollama", Name: "ol", Group: "ollama", Provider: config.ProviderOllama,
			KeyHash: config.HashKey("sk-odb-ollama-only"), KeyPrefix: "x", Enabled: true})
	})
	cfg := e.srv.cfg.Get()
	var key config.ClientKey
	for _, k := range cfg.ClientKeys {
		if k.ID == "ck-ollama" {
			key = k
		}
	}
	if key.Provider != config.ProviderOllama {
		t.Fatalf("key provider = %q", key.Provider)
	}
	// no account holds the group, yet the key and the group stay Ollama's
	if p := cfg.GroupProvider("ollama"); p != "" {
		t.Fatalf("group already holds %q", p)
	}
	if got := keyPool(cfg, key); got != config.ProviderOllama {
		t.Fatalf("keyPool = %q", got)
	}
	// so its requests are judged, logged and measured as the Ollama pool's
	rec := &requestLog{}
	rec.Provider = keyPool(cfg, key)
	if rec.Provider != config.ProviderOllama {
		t.Fatalf("logged provider = %q", rec.Provider)
	}
	errR, retR := cfg.RulesFor(rec.Provider)
	if len(errR) == 0 || errR[0].ID != "ollama-key-invalid" || len(retR) == 0 || retR[0].ID != "ollama-bad-request" {
		t.Fatalf("rules = %v %v", errR, retR)
	}
	// and an OpenDesign key stays OpenDesign's, even with no account of its own
	od := config.ClientKey{ID: "ck-od", Group: "default", Provider: config.ProviderOpenDesign}
	if got := keyPool(cfg, od); got != config.ProviderOpenDesign {
		t.Fatalf("opendesign keyPool = %q", got)
	}
}

// Saving the settings of one pool rewrites only that pool's rules: the
// console's "restore defaults" on the Ollama tab must leave the other two
// pools' rules untouched.
func TestSettingsSaveScopedToPool(t *testing.T) {
	s, err := New(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.cfg.Get()
	odBefore, _ := cfg.RulesFor(config.ProviderOpenDesign)
	rvBefore, _ := cfg.RulesFor(config.ProviderRiver)

	in := settingsDoc{Revision: cfg.Revision, Upstream: cfg.Upstream, Routing: cfg.Routing, Quota: cfg.Quota, Logs: cfg.Logs,
		Pricing: cfg.Pricing, ErrorRules: []rules.ErrorRule{{ID: "only-ollama", Scope: rules.ScopeAccount,
			Action: rules.ActionIgnore, When: rules.Match{Statuses: []int{418}}}},
		RetryRules: []rules.RetryRule{{ID: "only-ollama-retry", Decision: rules.DecisionStop, When: rules.Match{Statuses: []int{418}}}}}
	body, _ := json.Marshal(in)
	req := httptest.NewRequest(http.MethodPut, "/api/settings?provider=ollama", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.apiSaveSettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	cfg = s.cfg.Get()
	if ol, olr := mustRules(t, cfg, config.ProviderOllama); len(ol) != 1 || ol[0].ID != "only-ollama" || len(olr) != 1 || olr[0].ID != "only-ollama-retry" {
		t.Fatalf("ollama rules = %v %v", ol, olr)
	}
	if od, _ := cfg.RulesFor(config.ProviderOpenDesign); len(od) != len(odBefore) || od[0].ID != odBefore[0].ID {
		t.Fatalf("opendesign rules changed: %v", od)
	}
	if rv, _ := cfg.RulesFor(config.ProviderRiver); len(rv) != len(rvBefore) || rv[0].ID != rvBefore[0].ID {
		t.Fatalf("river rules changed: %v", rv)
	}
	// and the Modal tab, added after that test was written, is its own too
	mdBefore, _ := cfg.RulesFor(config.ProviderModal)
	in = settingsDoc{Revision: cfg.Revision, Upstream: cfg.Upstream, Routing: cfg.Routing, Quota: cfg.Quota, Logs: cfg.Logs,
		Pricing: cfg.Pricing, ErrorRules: []rules.ErrorRule{{ID: "only-modal", Scope: rules.ScopeAccount,
			Action: rules.ActionIgnore, When: rules.Match{Statuses: []int{418}}}},
		RetryRules: []rules.RetryRule{{ID: "only-modal-retry", Decision: rules.DecisionStop, When: rules.Match{Statuses: []int{418}}}}}
	body, _ = json.Marshal(in)
	rec = httptest.NewRecorder()
	s.apiSaveSettings(rec, httptest.NewRequest(http.MethodPut, "/api/settings?provider=modal", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("modal save: %d %s", rec.Code, rec.Body.String())
	}
	cfg = s.cfg.Get()
	if md, mdr := mustRules(t, cfg, config.ProviderModal); len(md) != 1 || md[0].ID != "only-modal" || len(mdr) != 1 || mdr[0].ID != "only-modal-retry" {
		t.Fatalf("modal rules = %v %v", md, mdr)
	}
	if od, _ := cfg.RulesFor(config.ProviderOpenDesign); len(od) != len(odBefore) || od[0].ID != odBefore[0].ID {
		t.Fatalf("opendesign rules changed by the modal save: %v", od)
	}
	if ol, _ := cfg.RulesFor(config.ProviderOllama); len(ol) != 1 || ol[0].ID != "only-ollama" {
		t.Fatalf("ollama rules changed by the modal save: %v", ol)
	}
	if len(mdBefore) != 5 {
		t.Fatalf("modal defaults = %d rules", len(mdBefore))
	}

	// the Modal tab reads its own rules back, with its own defaults
	rec = httptest.NewRecorder()
	s.apiSettings(rec, httptest.NewRequest(http.MethodGet, "/api/settings?provider=modal", nil))
	var got struct {
		Provider string         `json:"provider"`
		Settings settingsDoc    `json:"settings"`
		Defaults map[string]any `json:"defaults"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Provider != config.ProviderModal || len(got.Settings.ErrorRules) != 1 || got.Settings.ErrorRules[0].ID != "only-modal" {
		t.Fatalf("modal settings = %+v", got.Settings)
	}
	defs, _ := got.Defaults["errorRules"].([]any)
	if len(defs) != 5 {
		t.Fatalf("modal defaults served = %v", got.Defaults["errorRules"])
	}
	if first, _ := defs[0].(map[string]any); first["id"] != "modal-token-invalid" {
		t.Fatalf("modal default rule = %v", first["id"])
	}
}

func mustRules(t *testing.T, c *config.Config, p string) ([]rules.ErrorRule, []rules.RetryRule) {
	t.Helper()
	e, r := c.RulesFor(p)
	return e, r
}

// A Modal pool sends all three protocols to one host: on Modal the request's
// model field IS the Endpoint's hostname, so the account never changes base
// per model. The proxy token goes in Authorization only, and the session is
// passed on as Modal's affinity key so the prompt prefix stays on one
// container.
func TestModalPoolServesThreeProtocols(t *testing.T) {
	const token = "wk-example0000000000.ws-secret"
	var mu sync.Mutex
	seen := map[string]http.Header{}
	bodies := map[string]map[string]any{}
	e := newEnv(t, []config.Account{{ID: "m1", Provider: config.ProviderModal, Group: "modal", RuntimeKey: token}}, func(c *config.Config) {
		c.Upstream.ModalBase = c.Upstream.RuntimeBase
		c.ClientKeys = []config.ClientKey{{ID: "ck2", Group: "modal", KeyHash: config.HashKey("sk-odb-modal"), KeyPrefix: "x", Enabled: true}}
	})
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		mu.Lock()
		seen[r.URL.Path] = r.Header.Clone()
		bodies[r.URL.Path] = body
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":7,"output_tokens":2}}`)
		default:
			writeOK(w, key)
		}
	}
	e.key = "sk-odb-modal"

	for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
		hdr := map[string]string{}
		if path == "/v1/messages" {
			hdr["anthropic-version"] = "2023-06-01"
		}
		body := chat(false)
		body["model"] = "my-endpoint.us-west.modal.direct"
		resp := e.post(path, body, hdr)
		out := readAll(resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d body %s", path, resp.StatusCode, out)
		}
		mu.Lock()
		h, got := seen[path], bodies[path]
		mu.Unlock()
		if h == nil {
			t.Fatalf("%s never reached the upstream", path)
		}
		if got := h.Get("Authorization"); got != "Bearer "+token {
			t.Fatalf("%s: Authorization %q", path, got)
		}
		// Modal authenticates a proxy token in Authorization only
		if got := h.Get("X-Api-Key"); got != "" {
			t.Fatalf("%s: unexpected x-api-key %q", path, got)
		}
		if path == "/v1/messages" && h.Get("Anthropic-Version") == "" {
			t.Fatalf("%s: missing anthropic-version", path)
		}
		// the model name is forwarded untouched: it names the Endpoint
		if m, _ := got["model"].(string); m != "my-endpoint.us-west.modal.direct" {
			t.Fatalf("%s: model rewritten to %q", path, m)
		}
		// the conversation is passed on as Modal's routing key, as our own
		// fingerprint — never the client's raw session value
		ak := h.Get("Modal-Routing-Affinity-Key")
		if ak == "" {
			t.Fatalf("%s: missing Modal-Routing-Affinity-Key", path)
		}
		if strings.Contains(ak, secretMarker) {
			t.Fatal("affinity key leaked the prompt")
		}
	}

	// every path keeps its /v1: Modal's inference host has no bare /chat or
	// /models and answers 404 without the prefix
	for path, want := range map[string]string{
		"/v1/chat/completions": "/v1/chat/completions",
		"/v1/responses":        "/v1/responses",
		"/v1/messages":         "/v1/messages",
	} {
		mu.Lock()
		h := seen[path]
		mu.Unlock()
		if h == nil {
			t.Fatalf("%s never reached the upstream", path)
		}
		if path != want {
			t.Fatalf("%s/%s mismatch", path, want)
		}
	}

	// embeddings are not a Shared Endpoint path: refused locally, like River
	before := e.up.callCount()
	resp := e.post("/v1/embeddings", chat(false), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("embeddings: status %d, want 404", resp.StatusCode)
	}
	if e.up.callCount() != before {
		t.Fatal("embeddings reached the upstream")
	}
}

// Modal refuses a token with 401 {"error":"invalid proxy auth credentials"};
// the stock rule quarantines it, and a relay never sees the 401.
func TestModalRefusedTokenQuarantines(t *testing.T) {
	e := newEnv(t, []config.Account{
		{ID: "m1", Provider: config.ProviderModal, Group: "modal", RuntimeKey: "wk-deadkey0000000000.ws-deadsecret"},
	}, func(c *config.Config) {
		c.Upstream.ModalBase = c.Upstream.RuntimeBase
		c.ClientKeys = []config.ClientKey{{ID: "ck2", Group: "modal", KeyHash: config.HashKey("sk-odb-modal"), KeyPrefix: "x", Enabled: true}}
	})
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid proxy auth credentials"}`))
	}
	e.key = "sk-odb-modal"
	resp := e.post("/v1/chat/completions", chat(false), nil)
	body := readAll(resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	st := e.srv.pool.Views(e.srv.cfg.Get())["m1"].State
	if st == nil || !st.HardQuarantined || st.RuleID != "modal-token-invalid" {
		t.Fatalf("dead token not quarantined: %+v", st)
	}
}

// A model that names no Endpoint of this token is the caller's mistake: the
// 404 goes back as a request error and the account is not cooled down.
func TestModalUnknownEndpointIsClientError(t *testing.T) {
	e := newEnv(t, []config.Account{
		{ID: "m1", Provider: config.ProviderModal, Group: "modal", RuntimeKey: "wk-abcdefghijkl0000.ws-secretvalue"},
	}, func(c *config.Config) {
		c.Upstream.ModalBase = c.Upstream.RuntimeBase
		c.ClientKeys = []config.ClientKey{{ID: "ck2", Group: "modal", KeyHash: config.HashKey("sk-odb-modal"), KeyPrefix: "x", Enabled: true}}
	})
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"unknown inference model"}`))
	}
	e.key = "sk-odb-modal"
	resp := e.post("/v1/chat/completions", chat(false), nil)
	body := readAll(resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	st := e.srv.pool.Views(e.srv.cfg.Get())["m1"].State
	if st != nil && (st.HardQuarantined || !st.CooldownUntil.IsZero()) {
		t.Fatalf("a wrong model name cooled the account down: %+v", st)
	}
}

// A 429 from Modal is the model-wide concurrency limit: only that model cools
// down, so the same token keeps serving other Endpoints.
func TestModalConcurrencyLimitCoolsOneModel(t *testing.T) {
	e := newEnv(t, []config.Account{
		{ID: "m1", Provider: config.ProviderModal, Group: "modal", RuntimeKey: "wk-abcdefghijkl0000.ws-secretvalue"},
	}, func(c *config.Config) {
		c.Upstream.ModalBase = c.Upstream.RuntimeBase
		c.ClientKeys = []config.ClientKey{{ID: "ck2", Group: "modal", KeyHash: config.HashKey("sk-odb-modal"), KeyPrefix: "x", Enabled: true}}
	})
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		if m, _ := body["model"].(string); m == "busy.modal.direct" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "9")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"too many concurrent requests"}`))
			return
		}
		writeOK(w, key)
	}
	e.key = "sk-odb-modal"
	busy := chat(false)
	busy["model"] = "busy.modal.direct"
	if resp := e.post("/v1/chat/completions", busy, nil); resp.StatusCode != http.StatusTooManyRequests {
		resp.Body.Close()
		t.Fatalf("status %d, want the 429 passed on with its Retry-After", resp.StatusCode)
	}
	st := e.srv.pool.Views(e.srv.cfg.Get())["m1"].State
	if st == nil {
		t.Fatal("no account state")
	}
	if !st.CooldownUntil.IsZero() || st.HardQuarantined {
		t.Fatalf("the account itself must stay usable: %+v", st)
	}
	ms := st.Models["busy.modal.direct"]
	if ms == nil || ms.CooldownUntil.IsZero() {
		t.Fatalf("model not cooled down: %+v", st.Models)
	}
	// another Endpoint of the same token still works
	other := chat(false)
	other["model"] = "idle.modal.direct"
	if resp := e.post("/v1/chat/completions", other, nil); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("other model: status %d", resp.StatusCode)
	}
}

// The pool's base carries the /v1 the request paths hang off: a Modal request
// to /v1/chat/completions must reach {modalBase}/chat/completions, never the
// bare host Modal's proxy answers 404 on.
func TestModalTargetKeepsVersionPrefix(t *testing.T) {
	var got string
	e := newEnv(t, []config.Account{
		{ID: "m1", Provider: config.ProviderModal, Group: "modal", RuntimeKey: "wk-abcdefghijkl0000.ws-secretvalue"},
	}, func(c *config.Config) {
		c.Upstream.ModalBase = c.Upstream.RuntimeBase // the mock already ends in /v1
		c.ClientKeys = []config.ClientKey{{ID: "ck2", Group: "modal", KeyHash: config.HashKey("sk-odb-modal"), KeyPrefix: "x", Enabled: true}}
	})
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		got = r.URL.Path
		writeOK(w, key)
	}
	e.key = "sk-odb-modal"
	resp := e.post("/v1/chat/completions", chat(false), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if got != "/v1/chat/completions" {
		t.Fatalf("upstream path %q: the /v1 was dropped from the base", got)
	}
}

// A Modal token that is only its Token ID is named as such in the console,
// and the free test says why instead of relaying a 401 that reads like an
// outage: Modal wants wk-<id>.ws-<secret>.
func TestModalIncompleteTokenFlagged(t *testing.T) {
	e := newEnv(t, []config.Account{
		{ID: "m1", Provider: config.ProviderModal, Group: "modal", RuntimeKey: "wk-example0000000000"},
		{ID: "m2", Provider: config.ProviderModal, Group: "modal", RuntimeKey: "wk-abcdefghijkl0000.ws-secretvalue"},
	}, nil)
	views := e.srv.accountViews(e.srv.cfg.Get())
	got := map[string]bool{}
	for _, v := range views {
		got[v.ID] = v.TokenIncomplete
	}
	if !got["m1"] || got["m2"] {
		t.Fatalf("tokenIncomplete = %v", got)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/accounts/m1/test", strings.NewReader(`{"inference":false}`))
	req.SetPathValue("id", "m1")
	rec := httptest.NewRecorder()
	e.srv.apiTestAccount(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	rt, _ := out["runtime"].(map[string]any)
	msg, _ := rt["error"].(string)
	if rt["ok"] != false || !strings.Contains(msg, "ws-") {
		t.Fatalf("test result = %v", out)
	}
}

// A Modal real-request test has no model name to guess: it uses the first
// Endpoint of the account's own catalogue, and only says it can't when that
// catalogue is unknown.
func TestModalTestUsesCatalogueModel(t *testing.T) {
	e := newEnv(t, []config.Account{
		{ID: "m1", Provider: config.ProviderModal, Group: "modal", RuntimeKey: "wk-abcdefghijkl0000.ws-secretvalue"},
	}, func(c *config.Config) {
		c.Upstream.ModalBase = c.Upstream.RuntimeBase
	})
	var asked string
	e.up.handle = func(w http.ResponseWriter, r *http.Request, key string, body map[string]any) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":[{"id":"zzz.modal.direct"},{"id":"aaa.modal.direct"}]}`))
			return
		}
		asked, _ = body["model"].(string)
		writeOK(w, key)
	}
	// learn the catalogue the way the poller does
	if err := e.srv.quota.RefreshNow(context.Background(), e.srv.cfg.Get().Accounts[0]); err != nil {
		t.Fatal(err)
	}

	run := func() map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/m1/test", strings.NewReader(`{"inference":true}`))
		req.SetPathValue("id", "m1")
		rec := httptest.NewRecorder()
		e.srv.apiTestAccount(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := run()
	if rt, _ := out["inference"].(map[string]any); rt["ok"] != true {
		t.Fatalf("inference = %v", out["inference"])
	}
	// the catalogue's first entry, sorted: never a hard-coded model name
	if asked != "aaa.modal.direct" {
		t.Fatalf("model asked = %q", asked)
	}

	// with no catalogue read at all it refuses to guess instead of calling a
	// model that does not exist
	e.srv.quota.Forget("m1")
	out = run()
	rt, _ := out["inference"].(map[string]any)
	if rt["ok"] != false || !strings.Contains(rt["error"].(string), "hostname") {
		t.Fatalf("without a catalogue: %v", out["inference"])
	}
}
