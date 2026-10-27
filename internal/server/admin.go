package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"opendesign-balancer/internal/config"
	"opendesign-balancer/internal/logstore"
	"opendesign-balancer/internal/pool"
	"opendesign-balancer/internal/pricing"
	"opendesign-balancer/internal/quota"
	"opendesign-balancer/internal/rules"
	"opendesign-balancer/internal/stats"
)

//go:embed web
var webFS embed.FS

const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func (s *Server) registerAdmin(mux *http.ServeMux) {
	sub, _ := fs.Sub(webFS, "web")
	static := http.FileServer(http.FS(sub))
	// The assets carry a content hash in their URL so a proxy or browser that
	// cached an older app.js/app.css (the page is served behind a CDN) can't
	// pair it with a newer index.html.
	index, _ := fs.ReadFile(sub, "index.html")
	sum := sha256.New()
	for _, f := range []string{"app.css", "app.js"} {
		b, _ := fs.ReadFile(sub, f)
		sum.Write(b)
	}
	ver := "?v=" + hex.EncodeToString(sum.Sum(nil))[:10]
	index = bytes.ReplaceAll(index, []byte(`/assets/app.css"`), []byte(`/assets/app.css`+ver+`"`))
	index = bytes.ReplaceAll(index, []byte(`/assets/app.js"`), []byte(`/assets/app.js`+ver+`"`))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	})
	mux.HandleFunc("GET /assets/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-cache")
		http.StripPrefix("/assets", static).ServeHTTP(w, r)
	})

	mux.HandleFunc("GET /api/session", s.apiSession)
	mux.HandleFunc("POST /api/setup", s.apiSetup)
	mux.HandleFunc("POST /api/login", s.apiLogin)
	mux.HandleFunc("POST /api/logout", s.apiLogout)
	a := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, s.adminOnly(h)) }
	a("POST /api/password", s.apiPassword)
	a("GET /api/overview", s.apiOverview)
	a("GET /api/accounts", s.apiAccounts)
	a("POST /api/accounts", s.apiCreateAccount)
	a("POST /api/accounts/import", s.apiImportAccounts)
	a("PUT /api/accounts/{id}", s.apiUpdateAccount)
	a("DELETE /api/accounts/{id}", s.apiDeleteAccount)
	a("POST /api/accounts/{id}/recover", s.apiAccountAction)
	a("POST /api/accounts/{id}/reset-health", s.apiAccountAction)
	a("POST /api/accounts/{id}/refresh-quota", s.apiAccountAction)
	a("POST /api/accounts/{id}/test", s.apiTestAccount)
	a("GET /api/accounts/{id}/keys", s.apiAccountKeys)
	a("POST /api/device-auth", s.apiDeviceStart)
	a("GET /api/device-auth/{flow}", s.apiDevicePoll)
	a("DELETE /api/device-auth/{flow}", s.apiDeviceCancel)
	a("GET /api/client-keys", s.apiClientKeys)
	a("POST /api/client-keys", s.apiCreateClientKey)
	a("PUT /api/client-keys/{id}", s.apiUpdateClientKey)
	a("DELETE /api/client-keys/{id}", s.apiDeleteClientKey)
	a("GET /api/import-keys", s.apiImportKeys)
	a("POST /api/import-keys", s.apiCreateImportKey)
	a("PUT /api/import-keys/{id}", s.apiUpdateImportKey)
	a("DELETE /api/import-keys/{id}", s.apiDeleteImportKey)
	a("GET /api/staging", s.apiStaging)
	a("POST /api/staging/{id}/apply", s.apiStagingApply)
	a("DELETE /api/staging/{id}", s.apiStagingDiscard)
	a("GET /api/import-log", s.apiImportLog)
	a("GET /api/settings", s.apiSettings)
	a("PUT /api/settings", s.apiSaveSettings)
	a("GET /api/logs/{kind}", s.apiLogs)
	a("DELETE /api/logs/{kind}", s.apiClearLogs)
	a("POST /api/workflow/preview", s.apiPreview)
	a("POST /api/workflow/reset-counts", s.apiResetCounts)
	a("POST /api/config/reload", s.apiReload)
}

func readBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid JSON body: " + err.Error())
	}
	return nil
}

func apiError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (s *Server) configError(w http.ResponseWriter, err error) {
	if errors.Is(err, config.ErrConflict) {
		apiError(w, http.StatusConflict, errors.New("配置已被其他人修改，请刷新后重试"))
		return
	}
	apiError(w, http.StatusBadRequest, err)
}

// ---- session ----

func (s *Server) apiSession(w http.ResponseWriter, r *http.Request) {
	_, sess := s.admin.get(r)
	out := map[string]any{"initialized": s.cfg.Get().Admin.PasswordHash != "", "authenticated": sess != nil}
	if sess != nil {
		out["csrf"] = sess.csrf
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) apiSetup(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		apiError(w, http.StatusForbidden, errors.New("origin mismatch"))
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := readBody(r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	hash, err := hashPassword(in.Password)
	if err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	_, err = s.cfg.Update(-1, func(c *config.Config) error {
		if c.Admin.PasswordHash != "" {
			return errors.New("already initialized")
		}
		c.Admin.PasswordHash = hash
		return nil
	})
	if err != nil {
		apiError(w, http.StatusConflict, err)
		return
	}
	sess := s.admin.create(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"csrf": sess.csrf})
}

func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !sameOrigin(r) {
		apiError(w, http.StatusForbidden, errors.New("origin mismatch"))
		return
	}
	if s.admin.limited(ip) {
		w.Header().Set("Retry-After", "60")
		apiError(w, http.StatusTooManyRequests, errors.New("登录失败次数过多，请 60 秒后再试"))
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := readBody(r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	hash := s.cfg.Get().Admin.PasswordHash
	if hash == "" || !checkPassword(hash, in.Password) {
		s.admin.fail(ip)
		apiError(w, http.StatusUnauthorized, errors.New("密码错误"))
		return
	}
	sess := s.admin.create(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"csrf": sess.csrf})
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	if tok, _ := s.admin.get(r); tok != "" {
		s.admin.destroy(tok)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) apiPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := readBody(r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	if !checkPassword(s.cfg.Get().Admin.PasswordHash, in.Old) {
		apiError(w, http.StatusUnauthorized, errors.New("原密码错误"))
		return
	}
	hash, err := hashPassword(in.New)
	if err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := s.cfg.Update(-1, func(c *config.Config) error { c.Admin.PasswordHash = hash; return nil }); err != nil {
		s.configError(w, err)
		return
	}
	s.admin.destroyAll()
	sess := s.admin.create(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"csrf": sess.csrf})
}

// ---- accounts ----

type accountView struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Email         string          `json:"email"`
	UserID        string          `json:"userId"`
	Note          string          `json:"note"`
	Group         string          `json:"group"`
	Enabled       bool            `json:"enabled"`
	RuntimeKey    string          `json:"runtimeKey"`
	ControlKey    string          `json:"controlKey"`
	HasControlKey bool            `json:"hasControlKey"`
	MaxConcurrent int             `json:"maxConcurrent"`
	MaxRPM        int             `json:"maxRpm"`
	Weight        int             `json:"weight"`
	Priority      int             `json:"priority"`
	HasProxy      bool            `json:"hasProxy"`
	ProxyURL      string          `json:"proxyUrl"`
	CreatedAt     time.Time       `json:"createdAt"`
	Runtime       pool.View       `json:"runtime"`
	Quota         *quota.Snapshot `json:"quota"`
	QuotaFresh    bool            `json:"quotaFresh"`
	SharedWith    []string        `json:"sharedWorkspaceWith,omitempty"`
	// Usage24h is what this pool sent the account in the last 24h — the only
	// usage view of an account without a control key.
	Usage24h stats.Bucket `json:"usage24h"`
}

var proxyAuthRe = regexp.MustCompile(`//[^/@]*@`)

func maskProxy(u string) string { return proxyAuthRe.ReplaceAllString(u, "//***@") }

func (s *Server) accountViews(cfg *config.Config) []accountView {
	views := s.pool.Views(cfg)
	snaps := s.quota.All()
	usage := s.stats.Summary().Accounts
	now := time.Now()
	byWS := map[string][]string{}
	for _, a := range cfg.Accounts {
		if q := snaps[a.ID]; q != nil && q.WorkspaceID != "" {
			byWS[q.WorkspaceID] = append(byWS[q.WorkspaceID], a.ID)
		}
	}
	out := make([]accountView, 0, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		v := accountView{ID: a.ID, Name: a.Name, Email: a.Email, UserID: a.UserID, Note: a.Note, Group: a.Group, Enabled: a.Enabled,
			RuntimeKey: config.Mask(a.RuntimeKey), ControlKey: config.Mask(a.ControlKey), HasControlKey: a.ControlKey != "",
			MaxConcurrent: a.MaxConc, MaxRPM: a.MaxRPM, Weight: a.Weight, Priority: a.Priority,
			HasProxy: a.ProxyURL != "", ProxyURL: maskProxy(a.ProxyURL), CreatedAt: a.CreatedAt, Runtime: views[a.ID], Usage24h: usage[a.ID]}
		if q := snaps[a.ID]; q != nil && (q.WorkspaceID != "" || !q.PlanAt.IsZero() || q.LastError != "" || !q.CatalogAt.IsZero()) {
			v.Quota = q
			v.QuotaFresh = q.Fresh(now, time.Duration(cfg.Quota.StaleSec)*time.Second)
			for _, other := range byWS[q.WorkspaceID] {
				if other != a.ID {
					v.SharedWith = append(v.SharedWith, other)
				}
			}
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) apiAccounts(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	writeJSON(w, http.StatusOK, map[string]any{"revision": cfg.Revision, "accounts": s.accountViews(cfg)})
}

type accountInput struct {
	ExpectedRevision *int64  `json:"expectedRevision"`
	Name             *string `json:"name"`
	Email            *string `json:"email"`
	Note             *string `json:"note"`
	Group            *string `json:"group"`
	Enabled          *bool   `json:"enabled"`
	RuntimeKey       *string `json:"runtimeKey"`
	ControlKey       *string `json:"controlKey"`
	ClearControlKey  bool    `json:"clearControlKey"`
	MaxConcurrent    *int    `json:"maxConcurrent"`
	MaxRPM           *int    `json:"maxRpm"`
	Weight           *int    `json:"weight"`
	Priority         *int    `json:"priority"`
	ProxyURL         *string `json:"proxyUrl"`
	ClearProxy       bool    `json:"clearProxy"`
}

// apply sets the given fields; an empty key or proxy keeps the stored one.
func (in accountInput) apply(a *config.Account) {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = strings.TrimSpace(*v)
		}
	}
	set(&a.Name, in.Name)
	set(&a.Email, in.Email)
	set(&a.Note, in.Note)
	set(&a.Group, in.Group)
	if in.Enabled != nil {
		a.Enabled = *in.Enabled
	}
	if in.RuntimeKey != nil && strings.TrimSpace(*in.RuntimeKey) != "" {
		a.RuntimeKey = strings.TrimSpace(*in.RuntimeKey)
	}
	if in.ControlKey != nil && strings.TrimSpace(*in.ControlKey) != "" {
		a.ControlKey = strings.TrimSpace(*in.ControlKey)
	}
	if in.ClearControlKey {
		a.ControlKey = ""
	}
	if in.ProxyURL != nil && strings.TrimSpace(*in.ProxyURL) != "" {
		a.ProxyURL = strings.TrimSpace(*in.ProxyURL)
	}
	if in.ClearProxy {
		a.ProxyURL = ""
	}
	for _, f := range []struct {
		dst *int
		v   *int
	}{{&a.MaxConc, in.MaxConcurrent}, {&a.MaxRPM, in.MaxRPM}, {&a.Weight, in.Weight}, {&a.Priority, in.Priority}} {
		if f.v != nil {
			*f.dst = *f.v
		}
	}
}

func rev(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func (s *Server) apiCreateAccount(w http.ResponseWriter, r *http.Request) {
	var in accountInput
	if err := readBody(r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	if in.RuntimeKey == nil || strings.TrimSpace(*in.RuntimeKey) == "" {
		apiError(w, http.StatusBadRequest, errors.New("runtimeKey is required"))
		return
	}
	a := config.Account{ID: config.NewID("acc_"), Enabled: true, Group: config.DefaultGroup}
	in.apply(&a)
	cfg, err := s.cfg.Update(rev(in.ExpectedRevision), func(c *config.Config) error {
		c.Accounts = append(c.Accounts, a)
		return nil
	})
	if err != nil {
		s.configError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": cfg.Revision, "id": a.ID})
}

func (s *Server) apiUpdateAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in accountInput
	if err := readBody(r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	cfg, err := s.cfg.Update(rev(in.ExpectedRevision), func(c *config.Config) error {
		for i := range c.Accounts {
			if c.Accounts[i].ID == id {
				in.apply(&c.Accounts[i])
				return nil
			}
		}
		return errors.New("account not found")
	})
	if err != nil {
		s.configError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": cfg.Revision})
}

func (s *Server) apiDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cfg, err := s.cfg.Update(-1, func(c *config.Config) error {
		for i := range c.Accounts {
			if c.Accounts[i].ID == id {
				c.Accounts = append(c.Accounts[:i], c.Accounts[i+1:]...)
				return nil
			}
		}
		return errors.New("account not found")
	})
	if err != nil {
		s.configError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": cfg.Revision})
}

// apiImportAccounts takes one account per line:
// runtimeKey[,controlKey[,name[,proxyUrl]]] (commas, tabs or spaces).
func (s *Server) apiImportAccounts(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text  string `json:"text"`
		Group string `json:"group"`
	}
	if err := readBody(r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	rec := importRecord{Source: "console", Group: strings.TrimSpace(in.Group)}
	var added []config.Account
	var errs []string
	for n, line := range strings.Split(in.Text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == '\t' || r == ' ' })
		a := config.Account{ID: config.NewID("acc_"), Enabled: true, Group: strings.TrimSpace(in.Group)}
		for _, field := range f {
			switch {
			case strings.HasPrefix(field, "sk-") && a.RuntimeKey == "":
				a.RuntimeKey = field
			case strings.HasPrefix(field, "vela_ctrl_") && a.ControlKey == "":
				a.ControlKey = field
			case strings.Contains(field, "://"):
				a.ProxyURL = field
			case a.Name == "":
				a.Name = field
			}
		}
		if a.RuntimeKey == "" {
			errs = append(errs, "第 "+strconv.Itoa(n+1)+" 行缺少 sk-amr- 运行密钥")
			continue
		}
		added = append(added, a)
	}
	// all or nothing: one bad line rejects the batch
	rec.Total, rec.Status = len(added)+len(errs), http.StatusBadRequest
	if len(errs) > 0 {
		rec.Failed, rec.Error = len(errs), clipRunes(strings.Join(errs, "；"), 300)
		s.recordImport(r, rec)
		apiError(w, http.StatusBadRequest, errors.New(strings.Join(errs, "；")))
		return
	}
	if len(added) == 0 {
		apiError(w, http.StatusBadRequest, errors.New("没有可导入的账号"))
		return
	}
	cfg, err := s.cfg.Update(-1, func(c *config.Config) error {
		c.Accounts = append(c.Accounts, added...)
		return nil
	})
	if err != nil {
		rec.Failed, rec.Error = len(added), err.Error()
		s.recordImport(r, rec)
		s.configError(w, err)
		return
	}
	rec.Added, rec.Status = len(added), http.StatusOK
	s.recordImport(r, rec)
	writeJSON(w, http.StatusOK, map[string]any{"revision": cfg.Revision, "added": len(added)})
}

func (s *Server) apiAccountAction(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cfg := s.cfg.Get()
	a, ok := cfg.Account(id)
	if !ok {
		apiError(w, http.StatusNotFound, errors.New("account not found"))
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/recover"):
		s.pool.Recover(id)
	case strings.HasSuffix(r.URL.Path, "/reset-health"):
		s.pool.ResetHealth(id)
	case strings.HasSuffix(r.URL.Path, "/refresh-quota"):
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		if err := s.quota.RefreshNow(ctx, a); err != nil {
			apiError(w, http.StatusBadGateway, errors.New("额度读取失败："+err.Error()))
			return
		}
	}
	s.metaDirty.Store(true)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// apiTestAccount checks the runtime key with the free /models listing and
// the control key with the workspace read. With inference set it also
// sends one tiny real request, which costs credits.
// apiAccountKeys returns the unmasked keys so the admin can copy them from the edit dialog.
func (s *Server) apiAccountKeys(w http.ResponseWriter, r *http.Request) {
	a, ok := s.cfg.Get().Account(r.PathValue("id"))
	if !ok {
		apiError(w, http.StatusNotFound, errors.New("account not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"runtimeKey": a.RuntimeKey, "controlKey": a.ControlKey})
}

func (s *Server) apiTestAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cfg := s.cfg.Get()
	a, ok := cfg.Account(id)
	if !ok {
		apiError(w, http.StatusNotFound, errors.New("account not found"))
		return
	}
	var in struct {
		Inference bool   `json:"inference"`
		Model     string `json:"model"`
	}
	_ = readBody(r, &in)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	client := s.httpClient(a.ProxyURL)
	out := map[string]any{}

	t0 := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, cfg.Upstream.RuntimeBase+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+a.RuntimeKey)
	if resp, err := client.Do(req); err != nil {
		out["runtime"] = map[string]any{"ok": false, "error": redactSecrets(cfg, err.Error())}
	} else {
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body)
		resp.Body.Close()
		out["runtime"] = map[string]any{"ok": resp.StatusCode == 200, "status": resp.StatusCode, "models": len(body.Data), "ms": time.Since(t0).Milliseconds()}
	}
	if a.ControlKey != "" {
		qc := &quota.Client{Base: cfg.Upstream.ControlBase, HTTP: client}
		if ws, err := qc.Workspace(ctx, a.ControlKey); err != nil {
			out["control"] = map[string]any{"ok": false, "error": err.Error()}
		} else {
			out["control"] = map[string]any{"ok": true, "workspaceId": ws.WorkspaceID(), "workspaceName": ws.Name, "tier": ws.PersonalMembershipTier}
		}
	}
	if in.Inference {
		model := in.Model
		if model == "" {
			model = "deepseek-v4.1-flash"
		}
		payload, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 1, "messages": []map[string]string{{"role": "user", "content": "ping"}}})
		t1 := time.Now()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Upstream.RuntimeBase+"/chat/completions", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+a.RuntimeKey)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := client.Do(req); err != nil {
			out["inference"] = map[string]any{"ok": false, "error": redactSecrets(cfg, err.Error())}
		} else {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
			resp.Body.Close()
			res := map[string]any{"ok": resp.StatusCode == 200, "status": resp.StatusCode, "ms": time.Since(t1).Milliseconds(), "model": model}
			if resp.StatusCode != 200 {
				res["error"], _ = errorReason(cfg, b, resp.Header.Get("Content-Type"))
			}
			out["inference"] = res
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- device authorization: add an account by approving in its browser session ----

type deviceFlow struct {
	ID            string    `json:"id"`
	UserCode      string    `json:"userCode"`
	ActivationURL string    `json:"activationUrl"`
	ExpiresAt     time.Time `json:"expiresAt"`
	Status        string    `json:"status"` // pending | approved | expired | failed | cancelled
	Error         string    `json:"error,omitempty"`
	AccountID     string    `json:"accountId,omitempty"`
	Email         string    `json:"email,omitempty"`
	Updated       bool      `json:"updated,omitempty"`
	group         string
	accountID     string // re-authorize this account
	deviceID      string
	secret        string
	cancel        context.CancelFunc
}

type deviceFlows struct {
	mu    sync.Mutex
	flows map[string]*deviceFlow
}

func newDeviceFlows() *deviceFlows { return &deviceFlows{flows: map[string]*deviceFlow{}} }

func (d *deviceFlows) view(id string) (deviceFlow, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f := d.flows[id]
	if f == nil {
		return deviceFlow{}, false
	}
	return *f, true
}

func (s *Server) apiDeviceStart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Group     string `json:"group"`
		AccountID string `json:"accountId"`
		ProxyURL  string `json:"proxyUrl"`
	}
	if err := readBody(r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	cfg := s.cfg.Get()
	proxy := in.ProxyURL
	if in.AccountID != "" {
		a, ok := cfg.Account(in.AccountID)
		if !ok {
			apiError(w, http.StatusNotFound, errors.New("account not found"))
			return
		}
		proxy = a.ProxyURL
	}
	qc := &quota.Client{Base: cfg.Upstream.ControlBase, HTTP: s.httpClient(proxy)}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	start, err := qc.StartDevice(ctx, "account-pool")
	if err != nil {
		apiError(w, http.StatusBadGateway, errors.New("申请设备授权失败："+err.Error()))
		return
	}
	bg, bgCancel := context.WithDeadline(context.Background(), start.ExpiresAt.Add(30*time.Second))
	f := &deviceFlow{ID: randHex(8), UserCode: start.UserCode, ActivationURL: start.ActivationURL, ExpiresAt: start.ExpiresAt,
		Status: "pending", group: in.Group, accountID: in.AccountID, deviceID: start.DeviceID, secret: start.DeviceSecret, cancel: bgCancel}
	s.devices.mu.Lock()
	for id, old := range s.devices.flows {
		if old.Status != "pending" && time.Since(old.ExpiresAt) > time.Hour {
			delete(s.devices.flows, id)
		}
	}
	s.devices.flows[f.ID] = f
	s.devices.mu.Unlock()
	go s.pollDevice(bg, f, qc, max(start.PollIntervalSeconds, 2), proxy)
	v, _ := s.devices.view(f.ID)
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) pollDevice(ctx context.Context, f *deviceFlow, qc *quota.Client, interval int, proxy string) {
	defer f.cancel()
	setStatus := func(st, msg string) {
		s.devices.mu.Lock()
		f.Status, f.Error, f.secret = st, msg, ""
		s.devices.mu.Unlock()
	}
	t := time.NewTicker(time.Duration(interval) * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.devices.mu.Lock()
			if f.Status == "pending" {
				f.Status, f.secret = "expired", ""
			}
			s.devices.mu.Unlock()
			return
		case <-t.C:
		}
		s.devices.mu.Lock()
		secret, status := f.secret, f.Status
		s.devices.mu.Unlock()
		if status != "pending" {
			return
		}
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		tok, err := qc.PollDevice(c, f.deviceID, secret)
		cancel()
		if err != nil {
			var he *quota.HTTPError
			if errors.As(err, &he) && he.Status >= 400 && he.Status < 500 && he.Status != 429 {
				setStatus("failed", err.Error())
				return
			}
			continue
		}
		switch tok.Status {
		case "pending", "":
			continue
		case "approved":
		default:
			setStatus(tok.Status, "")
			return
		}
		if tok.RuntimeKey == "" || tok.ControlKey == "" {
			setStatus("failed", "approved but keys are missing")
			return
		}
		email, userID := "", ""
		if tok.User != nil {
			email, userID = tok.User.Email, tok.User.ID
		}
		accountID, updated, err := s.storeDeviceKeys(f, email, userID, tok.RuntimeKey, tok.ControlKey, proxy)
		s.devices.mu.Lock()
		f.secret = ""
		if err != nil {
			f.Status, f.Error = "failed", err.Error()
		} else {
			f.Status, f.AccountID, f.Email, f.Updated = "approved", accountID, email, updated
		}
		s.devices.mu.Unlock()
		return
	}
}

// storeDeviceKeys adds the account, or replaces the keys of the account
// being re-authorized or already holding that email.
func (s *Server) storeDeviceKeys(f *deviceFlow, email, userID, runtimeKey, controlKey, proxy string) (string, bool, error) {
	id, updated := "", false
	_, err := s.cfg.Update(-1, func(c *config.Config) error {
		for i := range c.Accounts {
			a := &c.Accounts[i]
			if (f.accountID != "" && a.ID == f.accountID) || (f.accountID == "" && email != "" && strings.EqualFold(a.Email, email)) {
				if f.accountID != "" && a.Email != "" && email != "" && !strings.EqualFold(a.Email, email) {
					return errors.New("批准的账号 " + email + " 与要重新授权的账号 " + a.Email + " 不一致")
				}
				a.RuntimeKey, a.ControlKey, a.UserID = runtimeKey, controlKey, userID
				if email != "" {
					a.Email = email
				}
				id, updated = a.ID, true
				return nil
			}
		}
		a := config.Account{ID: config.NewID("acc_"), Name: email, Email: email, UserID: userID, Group: f.group,
			Enabled: true, RuntimeKey: runtimeKey, ControlKey: controlKey, ProxyURL: proxy}
		c.Accounts = append(c.Accounts, a)
		id = a.ID
		return nil
	})
	if err == nil {
		s.pool.Recover(id)
	}
	return id, updated, err
}

func (s *Server) apiDevicePoll(w http.ResponseWriter, r *http.Request) {
	v, ok := s.devices.view(r.PathValue("flow"))
	if !ok {
		apiError(w, http.StatusNotFound, errors.New("flow not found"))
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) apiDeviceCancel(w http.ResponseWriter, r *http.Request) {
	s.devices.mu.Lock()
	if f := s.devices.flows[r.PathValue("flow")]; f != nil && f.Status == "pending" {
		f.Status, f.secret = "cancelled", ""
		f.cancel()
	}
	s.devices.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- client keys ----

func (s *Server) apiClientKeys(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	type view struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		Group     string    `json:"group"`
		KeyPrefix string    `json:"keyPrefix"`
		Key       string    `json:"key,omitempty"`
		Enabled   bool      `json:"enabled"`
		CreatedAt time.Time `json:"createdAt"`
	}
	out := []view{}
	for _, k := range cfg.ClientKeys {
		out = append(out, view{k.ID, k.Name, k.Group, k.KeyPrefix, k.Key, k.Enabled, k.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": cfg.Revision, "keys": out})
}

func (s *Server) apiCreateClientKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name  string `json:"name"`
		Group string `json:"group"`
	}
	if err := readBody(r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	key := "sk-odb-" + randHex(24)
	k := config.ClientKey{ID: config.NewID("ck_"), Name: strings.TrimSpace(in.Name), Group: strings.TrimSpace(in.Group),
		KeyHash: config.HashKey(key), KeyPrefix: key[:11] + "…" + key[len(key)-4:], Key: key, Enabled: true, CreatedAt: time.Now().UTC()}
	if _, err := s.cfg.Update(-1, func(c *config.Config) error { c.ClientKeys = append(c.ClientKeys, k); return nil }); err != nil {
		s.configError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": k.ID, "key": key})
}

func (s *Server) apiUpdateClientKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in struct {
		Name    *string `json:"name"`
		Group   *string `json:"group"`
		Enabled *bool   `json:"enabled"`
	}
	if err := readBody(r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	_, err := s.cfg.Update(-1, func(c *config.Config) error {
		for i := range c.ClientKeys {
			k := &c.ClientKeys[i]
			if k.ID != id {
				continue
			}
			if in.Name != nil {
				k.Name = strings.TrimSpace(*in.Name)
			}
			if in.Group != nil {
				k.Group = strings.TrimSpace(*in.Group)
			}
			if in.Enabled != nil {
				k.Enabled = *in.Enabled
			}
			return nil
		}
		return errors.New("client key not found")
	})
	if err != nil {
		s.configError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) apiDeleteClientKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := s.cfg.Update(-1, func(c *config.Config) error {
		for i := range c.ClientKeys {
			if c.ClientKeys[i].ID == id {
				c.ClientKeys = append(c.ClientKeys[:i], c.ClientKeys[i+1:]...)
				return nil
			}
		}
		return errors.New("client key not found")
	})
	if err != nil {
		s.configError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- settings ----

type settingsDoc struct {
	Revision   int64             `json:"revision"`
	Upstream   config.Upstream   `json:"upstream"`
	Routing    config.Routing    `json:"routing"`
	Quota      config.Quota      `json:"quota"`
	Logs       config.Logs       `json:"logs"`
	Pricing    pricing.Config    `json:"pricing"`
	ErrorRules []rules.ErrorRule `json:"errorRules"`
	RetryRules []rules.RetryRule `json:"retryRules"`
}

func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request) {
	c := s.cfg.Get()
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":   settingsDoc{c.Revision, c.Upstream, c.Routing, c.Quota, c.Logs, c.Pricing, c.ErrorRules, c.RetryRules},
		"strategies": config.Strategies,
		"defaults":   map[string]any{"errorRules": rules.DefaultErrorRules(), "retryRules": rules.DefaultRetryRules(), "pricing": pricing.Defaults()},
	})
}

func (s *Server) apiSaveSettings(w http.ResponseWriter, r *http.Request) {
	var in settingsDoc
	if err := readBody(r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	cfg, err := s.cfg.Update(in.Revision, func(c *config.Config) error {
		c.Upstream, c.Routing, c.Quota, c.Logs = in.Upstream, in.Routing, in.Quota, in.Logs
		c.Pricing = in.Pricing
		c.ErrorRules, c.RetryRules = in.ErrorRules, in.RetryRules
		return nil
	})
	if err != nil {
		s.configError(w, err)
		return
	}
	s.resetModels()
	writeJSON(w, http.StatusOK, map[string]any{"revision": cfg.Revision})
}

func (s *Server) apiReload(w http.ResponseWriter, r *http.Request) {
	if err := s.Reload(); err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": s.cfg.Get().Revision})
}

// ---- logs ----

func parseTime(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.UnixMilli(ms), nil
	}
	return time.Parse(time.RFC3339, v)
}

func (s *Server) apiLogs(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	allowed, ok := logstore.Filters[kind]
	if !ok {
		apiError(w, http.StatusNotFound, errors.New("unknown log kind"))
		return
	}
	q := r.URL.Query()
	filters := map[string]string{}
	limit, cursor := 50, int64(0)
	var since, until time.Time
	for k, vs := range q {
		v := vs[0]
		var err error
		switch {
		case k == "limit":
			limit, err = strconv.Atoi(v)
			if err == nil && (limit < 1 || limit > 200) {
				err = errors.New("limit must be 1..200")
			}
		case k == "cursor":
			cursor, err = strconv.ParseInt(v, 10, 64)
		case k == "since":
			since, err = parseTime(v)
		case k == "until":
			until, err = parseTime(v)
		default:
			known := false
			for _, f := range allowed {
				known = known || f == k
			}
			if !known {
				err = errors.New("unknown query parameter " + k)
			} else if v != "" {
				filters[k] = v
			}
		}
		if err != nil {
			apiError(w, http.StatusBadRequest, err)
			return
		}
	}
	page, err := s.logs.Query(kind, filters, since, until, limit, cursor)
	if errors.Is(err, logstore.ErrNotReady) {
		w.Header().Set("Retry-After", "2")
		apiError(w, http.StatusServiceUnavailable, err)
		return
	}
	if err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": page.Items, "nextCursor": page.NextCursor, "dropped": s.logs.Dropped()})
}

func (s *Server) apiClearLogs(w http.ResponseWriter, r *http.Request) {
	if err := s.logs.Clear(r.PathValue("kind")); err != nil {
		apiError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- workflow ----

func (s *Server) apiPreview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Group string `json:"group"`
		Model string `json:"model"`
	}
	if err := readBody(r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	if in.Group == "" {
		in.Group = config.DefaultGroup
	}
	order, excluded := s.pool.Preview(s.cfg.Get(), in.Group, in.Model)
	if order == nil {
		order = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"order": order, "excluded": excluded})
}

func (s *Server) apiResetCounts(w http.ResponseWriter, r *http.Request) {
	s.pool.ResetCounts()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- overview ----

type windowTotal struct {
	DurationSeconds int64 `json:"durationSeconds"`
	Used            int64 `json:"used"`
	Limit           int64 `json:"limit"`
	Remaining       int64 `json:"remaining"`
	Workspaces      int   `json:"workspaces"`
}

func (s *Server) apiOverview(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	views := s.accountViews(cfg)
	now := time.Now()
	statusCount := map[string]int{}
	groups := map[string]int{}
	seenWS := map[string]bool{}
	windows := map[int64]*windowTotal{}
	wallet, walletKnown := 0.0, 0
	authFailed := 0
	for _, v := range views {
		statusCount[v.Runtime.Status]++
		groups[v.Group]++
		q := v.Quota
		if q == nil {
			continue
		}
		if q.AuthFailed {
			authFailed++
		}
		// the same workspace is counted once; failed or stale reads count nothing
		if !v.QuotaFresh || q.WorkspaceID == "" || seenWS[q.WorkspaceID] {
			continue
		}
		seenWS[q.WorkspaceID] = true
		for _, win := range q.Windows {
			t := windows[win.DurationSeconds]
			if t == nil {
				t = &windowTotal{DurationSeconds: win.DurationSeconds}
				windows[win.DurationSeconds] = t
			}
			rem := win.EffectiveRemaining(now)
			t.Limit += win.Limit
			t.Remaining += rem
			t.Used += win.Limit - rem
			t.Workspaces++
		}
		if usd, ok := quota.ParseUSD(q.WalletUSD); ok {
			wallet += usd
			walletKnown++
		}
	}
	wl := []windowTotal{}
	for _, t := range windows {
		wl = append(wl, *t)
	}
	sort.Slice(wl, func(i, j int) bool { return wl[i].DurationSeconds > wl[j].DurationSeconds })
	groupList := []string{}
	for g := range groups {
		groupList = append(groupList, g)
	}
	for _, k := range cfg.ClientKeys {
		if _, ok := groups[k.Group]; !ok {
			groups[k.Group] = 0
			groupList = append(groupList, k.Group)
		}
	}
	sort.Strings(groupList)
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":       len(views),
		"status":         statusCount,
		"groups":         groupList,
		"windows":        wl,
		"walletUsd":      wallet,
		"walletAccounts": walletKnown,
		"authFailed":     authFailed,
		"workspaces":     len(seenWS),
		"bindings":       s.pool.BindingCount(),
		"stats":          s.stats.Summary(),
		"logsDropped":    s.logs.Dropped(),
		"clientKeys":     len(cfg.ClientKeys),
		"uptimeSec":      int64(time.Since(s.started).Seconds()),
		"strategy":       cfg.Routing.Strategy,
	})
}
