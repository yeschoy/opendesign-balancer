package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"opendesign-balancer/internal/config"
)

const testImportKey = "sk-odbimp-test-import-key-00000001"

func newImportEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, []config.Account{{ID: "a", Name: "A", ControlKey: "vela_ctrl_a"}}, func(c *config.Config) {
		c.ImportKeys = []config.ImportKey{{ID: "ik1", Name: "signup-bot", Group: "pool2", KeyHash: config.HashKey(testImportKey), KeyPrefix: "x", Enabled: true}}
	})
	return e
}

func (e *env) openImport(key string, body any) (int, map[string]any) {
	e.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, e.front.URL+"/open/v1/accounts/import", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	return resp.StatusCode, out
}

func results(out map[string]any) []map[string]any {
	var rs []map[string]any
	for _, r := range out["results"].([]any) {
		rs = append(rs, r.(map[string]any))
	}
	return rs
}

func TestOpenImportAuth(t *testing.T) {
	e := newImportEnv(t)
	if st, _ := e.openImport("", map[string]any{"runtimeKey": "sk-amr-new"}); st != 401 {
		t.Fatalf("no key: %d", st)
	}
	if st, _ := e.openImport("sk-odbimp-wrong", map[string]any{"runtimeKey": "sk-amr-new"}); st != 401 {
		t.Fatalf("wrong key: %d", st)
	}
	if st, _ := e.openImport(e.key, map[string]any{"runtimeKey": "sk-amr-new"}); st != 401 {
		t.Fatalf("a downstream sk-odb key must not import accounts: %d", st)
	}
	if len(e.srv.cfg.Get().Accounts) != 1 {
		t.Fatal("an unauthenticated request changed the accounts")
	}
	// a disabled key is refused too
	_, _ = e.srv.cfg.Update(-1, func(c *config.Config) error { c.ImportKeys[0].Enabled = false; return nil })
	if st, _ := e.openImport(testImportKey, map[string]any{"runtimeKey": "sk-amr-new"}); st != 401 {
		t.Fatalf("disabled key: %d", st)
	}
}

func TestOpenImportAddsUnchangedAndStages(t *testing.T) {
	e := newImportEnv(t)
	st, out := e.openImport(testImportKey, map[string]any{"runtimeKey": "sk-amr-new-1", "controlKey": "vela_ctrl_new1", "name": "新号", "note": "来自脚本"})
	if st != 200 || out["added"].(float64) != 1 {
		t.Fatalf("add: %d %v", st, out)
	}
	cfg := e.srv.cfg.Get()
	if len(cfg.Accounts) != 2 {
		t.Fatalf("accounts = %d", len(cfg.Accounts))
	}
	n := cfg.Accounts[1]
	if n.Name != "新号" || n.Group != "pool2" || n.RuntimeKey != "sk-amr-new-1" || n.ControlKey != "vela_ctrl_new1" || !n.Enabled {
		t.Fatalf("stored account: %+v", n)
	}
	if !strings.Contains(n.Note, "开放接口导入 20") || !strings.Contains(n.Note, "signup-bot") || !strings.Contains(n.Note, "来自脚本") {
		t.Fatalf("note lacks the import time/key/user note: %q", n.Note)
	}

	// the same account again is a no-op
	st, out = e.openImport(testImportKey, map[string]any{"runtimeKey": "sk-amr-new-1", "controlKey": "vela_ctrl_new1", "name": "新号"})
	if st != 200 || out["unchanged"].(float64) != 1 || len(e.srv.cfg.Get().Accounts) != 2 {
		t.Fatalf("repeat: %d %v", st, out)
	}

	// same runtime key with another name: staged, config untouched
	st, out = e.openImport(testImportKey, map[string]any{"runtimeKey": "sk-amr-a-runtime-secret", "name": "A-renamed"})
	if st != 202 || out["staged"].(float64) != 1 {
		t.Fatalf("stage: %d %v", st, out)
	}
	r := results(out)[0]
	if r["status"] != "staged" || r["stageId"] == "" || r["conflicts"].([]any)[0].(map[string]any)["reason"] != "runtimeKey" {
		t.Fatalf("staged result: %v", r)
	}
	if len(e.srv.cfg.Get().Accounts) != 2 {
		t.Fatal("a staged import changed config.json")
	}
	if _, err := os.Stat(stagingPath(e.srv.dataDir)); err != nil {
		t.Fatal("staging.json not written:", err)
	}
	// re-posting refreshes the staged entry instead of adding a second one
	st, out2 := e.openImport(testImportKey, map[string]any{"runtimeKey": "sk-amr-a-runtime-secret", "name": "A-renamed-2"})
	if st != 202 || results(out2)[0]["stageId"] != r["stageId"] || len(e.srv.staging.list()) != 1 {
		t.Fatalf("restage: %d %v", st, out2)
	}

	// same name only: staged with reason name and may be added as new
	st, out3 := e.openImport(testImportKey, map[string]any{"runtimeKey": "sk-amr-other", "name": "a"})
	if st != 202 {
		t.Fatalf("name conflict: %d %v", st, out3)
	}
	views := e.srv.stagedViews(e.srv.cfg.Get())
	if len(views) != 2 || views[0].CanAdd || !views[1].CanAdd || views[1].Conflicts[0].Reason != "name" {
		t.Fatalf("views: %+v", views)
	}
	if views[0].RuntimeKey == "sk-amr-a-runtime-secret" {
		t.Fatal("staged view leaks the runtime key")
	}

	// overwrite: account a takes the new name; staging shrinks
	if _, err := e.srv.applyStaged(views[0].ID, "overwrite", ""); err != nil {
		t.Fatal(err)
	}
	a, _ := e.srv.cfg.Get().Account("a")
	if a.Name != "A-renamed-2" || a.RuntimeKey != "sk-amr-a-runtime-secret" || a.ControlKey != "vela_ctrl_a" || !strings.Contains(a.Note, "覆盖确认") {
		t.Fatalf("after overwrite: %+v", a)
	}
	// add: a new account with the clashing name
	id, err := e.srv.applyStaged(views[1].ID, "add", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := e.srv.cfg.Get().Account(id); !ok || len(e.srv.cfg.Get().Accounts) != 3 || len(e.srv.staging.list()) != 0 {
		t.Fatalf("after add: %d accounts, %d staged", len(e.srv.cfg.Get().Accounts), len(e.srv.staging.list()))
	}
	if _, err := e.srv.applyStaged("stg_missing", "add", ""); err == nil {
		t.Fatal("missing staged id accepted")
	}
}

func TestOpenImportBatchAndValidation(t *testing.T) {
	e := newImportEnv(t)
	st, out := e.openImport(testImportKey, map[string]any{"accounts": []map[string]any{
		{"runtimeKey": "sk-amr-b1", "name": "b1", "group": "g1"},
		{"runtimeKey": "bad-key", "name": "b2"},
		{"controlKey": "vela_ctrl_x"},
		{"runtimeKey": "sk-amr-b3", "controlKey": "wrong"},
	}})
	if st != 200 || out["added"].(float64) != 1 || out["failed"].(float64) != 3 {
		t.Fatalf("batch: %d %v", st, out)
	}
	rs := results(out)
	for i, want := range []string{"added", "error", "error", "error"} {
		if rs[i]["status"] != want {
			t.Fatalf("result %d = %v, want %s", i, rs[i], want)
		}
	}
	b1 := e.srv.cfg.Get().Accounts[1]
	if b1.Group != "g1" || b1.Name != "b1" {
		t.Fatalf("group from request not kept: %+v", b1)
	}
	if st, _ := e.openImport(testImportKey, map[string]any{"name": "x"}); st != 400 {
		t.Fatalf("all failed must be 400, got %d", st)
	}
	if st, _ := e.openImport(testImportKey, map[string]any{"runtimeKey": "sk-amr-z", "bogus": 1}); st != 400 {
		t.Fatalf("unknown field must be rejected, got %d", st)
	}
}

func TestStagingSurvivesRestart(t *testing.T) {
	e := newImportEnv(t)
	if st, _ := e.openImport(testImportKey, map[string]any{"runtimeKey": "sk-amr-a-runtime-secret", "name": "dup"}); st != 202 {
		t.Fatalf("stage: %d", st)
	}
	again, err := loadStaging(stagingPath(e.srv.dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if l := again.list(); len(l) != 1 || l[0].Name != "dup" || l[0].ImportKey != "signup-bot" {
		t.Fatalf("reloaded staging: %+v", l)
	}
}

func TestImportLogRecordsCalls(t *testing.T) {
	e := newImportEnv(t)
	// a failed authentication is not recorded
	if st, _ := e.openImport("sk-odbimp-wrong", map[string]any{"runtimeKey": "sk-amr-new"}); st != 401 {
		t.Fatalf("wrong key: %d", st)
	}
	b, _ := json.Marshal(map[string]any{"accounts": []map[string]any{
		{"runtimeKey": "sk-amr-log-1", "name": "L1"},
		{"runtimeKey": "sk-amr-a-runtime-secret"}, // the existing account again
		{"runtimeKey": "bad"},
	}})
	req, _ := http.NewRequest(http.MethodPost, e.front.URL+"/open/v1/accounts/import", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+testImportKey)
	req.Header.Set("CF-Connecting-IP", "203.0.113.7")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if st, _ := e.openImport(testImportKey, "not an object"); st != 400 {
		t.Fatalf("bad body: %d", st)
	}
	recs := e.srv.imports.recent(10)
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2 (401 not recorded): %+v", len(recs), recs)
	}
	bad, batch := recs[0], recs[1]
	if bad.Status != 400 || bad.Error == "" || bad.Total != 0 {
		t.Fatalf("bad body record: %+v", bad)
	}
	if batch.Source != "open" || batch.ImportKey != "signup-bot" || batch.ImportKeyID != "ik1" || batch.Status != 200 ||
		batch.Total != 3 || batch.Added != 1 || batch.Unchanged != 1 || batch.Failed != 1 || batch.Staged != 0 {
		t.Fatalf("batch record: %+v", batch)
	}
	// the test server's peer is loopback, so the Cloudflare header names the caller
	if batch.IP != "203.0.113.7" || batch.Peer != "127.0.0.1" || bad.IP != "127.0.0.1" || bad.Peer != "" {
		t.Fatalf("ips: batch %s/%s bad %s/%s", batch.IP, batch.Peer, bad.IP, bad.Peer)
	}
	reloaded, err := loadImportLog(importLogPath(e.srv.dataDir))
	if err != nil || len(reloaded.recent(10)) != 2 || reloaded.recent(1)[0].ID != bad.ID {
		t.Fatalf("reload: %v %+v", err, reloaded)
	}
}

func TestImportLogCap(t *testing.T) {
	l := &importLog{path: t.TempDir() + "/import-log.json"}
	for i := 0; i < maxImportLog+5; i++ {
		l.add(importRecord{ID: strconv.Itoa(i)})
	}
	if got := l.recent(maxImportLog * 2); len(got) != maxImportLog || got[0].ID != strconv.Itoa(maxImportLog+4) || got[len(got)-1].ID != "5" {
		t.Fatalf("cap: len %d first %s last %s", len(got), got[0].ID, got[len(got)-1].ID)
	}
}

func TestRequestIP(t *testing.T) {
	cases := []struct {
		peer    string
		hdr     map[string]string
		ip, via string
	}{
		{"198.51.100.1:5000", map[string]string{"CF-Connecting-IP": "203.0.113.7"}, "198.51.100.1", ""}, // public peer: headers ignored
		{"172.18.0.1:5000", map[string]string{"CF-Connecting-IP": "203.0.113.7", "X-Real-IP": "10.0.0.9"}, "203.0.113.7", "172.18.0.1"},
		{"127.0.0.1:5000", map[string]string{"X-Forwarded-For": "1.1.1.1, 203.0.113.8"}, "203.0.113.8", "127.0.0.1"}, // last hop, not the spoofable first
		{"127.0.0.1:5000", map[string]string{"CF-Connecting-IP": "not-an-ip"}, "127.0.0.1", ""},
		{"10.1.2.3:5000", nil, "10.1.2.3", ""},
	}
	for i, c := range cases {
		r, _ := http.NewRequest(http.MethodGet, "http://x/", nil)
		r.RemoteAddr = c.peer
		for k, v := range c.hdr {
			r.Header.Set(k, v)
		}
		if ip, peer := requestIP(r); ip != c.ip || peer != c.via {
			t.Errorf("case %d: got %s/%s want %s/%s", i, ip, peer, c.ip, c.via)
		}
	}
}

func TestImportLogRecordsConsoleImport(t *testing.T) {
	e := newImportEnv(t)
	post := func(text string) int {
		b, _ := json.Marshal(map[string]string{"text": text, "group": "g2"})
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/import", bytes.NewReader(b))
		req.RemoteAddr = "192.0.2.5:4000"
		rec := httptest.NewRecorder()
		e.srv.apiImportAccounts(rec, req)
		return rec.Code
	}
	if st := post("sk-amr-c1,vela_ctrl_c1,C1\nsk-amr-c2"); st != 200 {
		t.Fatalf("import: %d", st)
	}
	if st := post("sk-amr-c3\nno-key-here"); st != 400 {
		t.Fatalf("bad line: %d", st)
	}
	recs := e.srv.imports.recent(10)
	if len(recs) != 2 {
		t.Fatalf("records = %d", len(recs))
	}
	if r := recs[1]; r.Source != "console" || r.Group != "g2" || r.IP != "192.0.2.5" || r.Total != 2 || r.Added != 2 || r.Status != 200 {
		t.Fatalf("ok record: %+v", r)
	}
	if r := recs[0]; r.Total != 2 || r.Added != 0 || r.Failed != 1 || r.Status != 400 || r.Error == "" {
		t.Fatalf("rejected record: %+v", r)
	}
}
