package server

// The open account-import endpoint: an automation (a sign-up script, another
// pool) posts runtime key, control key and name with an import key and the
// account joins the pool at once. Anything that collides with an existing
// account — same runtime key, same control key or same name — is not
// written to config.json but parked in DATA_DIR/staging.json until an admin
// decides in the console to overwrite the existing account, add it as a new
// one, or discard it. The note of every imported account records when and
// through which import key it arrived.

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"opendesign-balancer/internal/config"
	"opendesign-balancer/internal/fsutil"
)

const (
	importKeyPrefix = "sk-odbimp-"
	maxStaged       = 500
	maxImportBatch  = 200
	noteTimeLayout  = "2006-01-02 15:04:05 MST"
)

// stagedAccount is an import held back because it collides with an account.
type stagedAccount struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Group       string    `json:"group"`
	Note        string    `json:"note"`
	RuntimeKey  string    `json:"runtimeKey"`
	ControlKey  string    `json:"controlKey,omitempty"`
	ProxyURL    string    `json:"proxyUrl,omitempty"`
	ImportedAt  time.Time `json:"importedAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	ImportKey   string    `json:"importKey"` // name (or id) of the import key used
	ImportKeyID string    `json:"importKeyId"`
}

type stagingFile struct {
	Version int             `json:"version"`
	Items   []stagedAccount `json:"items"`
}

type staging struct {
	mu    sync.Mutex
	path  string
	items []stagedAccount
}

func loadStaging(path string) (*staging, error) {
	st := &staging{path: path}
	var f stagingFile
	switch err := fsutil.ReadJSON(path, &f); {
	case errors.Is(err, fsutil.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		st.items = f.Items
	}
	return st, nil
}

// save must be called with mu held.
func (st *staging) save() error {
	items := st.items
	if items == nil {
		items = []stagedAccount{}
	}
	return fsutil.WriteJSONAtomic(st.path, stagingFile{Version: 1, Items: items})
}

func (st *staging) list() []stagedAccount {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]stagedAccount, len(st.items))
	copy(out, st.items)
	return out
}

func (st *staging) get(id string) (stagedAccount, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, it := range st.items {
		if it.ID == id {
			return it, true
		}
	}
	return stagedAccount{}, false
}

// put adds a, or refreshes the staged entry holding the same runtime key.
// It reports the stored id and whether an entry was replaced.
func (st *staging) put(a stagedAccount) (string, bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := range st.items {
		if st.items[i].RuntimeKey == a.RuntimeKey {
			a.ID, a.ImportedAt = st.items[i].ID, st.items[i].ImportedAt
			st.items[i] = a
			return a.ID, true, st.save()
		}
	}
	if len(st.items) >= maxStaged {
		return "", false, fmt.Errorf("暂存区已满（%d 条），请先在管理台处理", maxStaged)
	}
	st.items = append(st.items, a)
	return a.ID, false, st.save()
}

func (st *staging) remove(id string) (bool, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for i := range st.items {
		if st.items[i].ID == id {
			st.items = append(st.items[:i], st.items[i+1:]...)
			return true, st.save()
		}
	}
	return false, nil
}

// conflict names an existing account an import collides with.
type conflict struct {
	AccountID string `json:"accountId"`
	Name      string `json:"name"`
	Email     string `json:"email,omitempty"`
	Group     string `json:"group"`
	Reason    string `json:"reason"` // runtimeKey | controlKey | name
}

// findConflicts lists the accounts in cfg that share a key or the name
// with the import, strongest reason first (runtime key, control key, name).
func findConflicts(cfg *config.Config, runtimeKey, controlKey, name string) []conflict {
	var out []conflict
	seen := map[string]bool{}
	add := func(a config.Account, reason string) {
		if seen[a.ID] {
			return
		}
		seen[a.ID] = true
		out = append(out, conflict{AccountID: a.ID, Name: a.Name, Email: a.Email, Group: a.Group, Reason: reason})
	}
	for _, a := range cfg.Accounts {
		if a.RuntimeKey == runtimeKey {
			add(a, "runtimeKey")
		}
	}
	if controlKey != "" {
		for _, a := range cfg.Accounts {
			if a.ControlKey == controlKey {
				add(a, "controlKey")
			}
		}
	}
	if name != "" {
		for _, a := range cfg.Accounts {
			if strings.EqualFold(strings.TrimSpace(a.Name), name) {
				add(a, "name")
			}
		}
	}
	return out
}

func hasReason(cs []conflict, reason string) bool {
	for _, c := range cs {
		if c.Reason == reason {
			return true
		}
	}
	return false
}

// importNote is the note an imported account carries: when it arrived and
// through which import key, followed by whatever the caller wrote.
func importNote(at time.Time, keyName, extra string) string {
	n := "开放接口导入 " + at.Local().Format(noteTimeLayout) + "（密钥 " + keyName + "）"
	if extra = strings.TrimSpace(extra); extra != "" {
		n += "\n" + extra
	}
	return clipRunes(n, 500)
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ---- the open endpoint ----

type importItem struct {
	RuntimeKey string `json:"runtimeKey"`
	ControlKey string `json:"controlKey"`
	Name       string `json:"name"`
	Group      string `json:"group"`
	Note       string `json:"note"`
	ProxyURL   string `json:"proxyUrl"`
	// Also accepted, as some clients name them this way.
	APIKey string `json:"apiKey"`
	Key    string `json:"key"`
}

type importRequest struct {
	importItem
	Accounts []importItem `json:"accounts"`
}

type importResult struct {
	Index     int        `json:"index"`
	Status    string     `json:"status"` // added | unchanged | staged | error
	AccountID string     `json:"accountId,omitempty"`
	StageID   string     `json:"stageId,omitempty"`
	Name      string     `json:"name,omitempty"`
	Conflicts []conflict `json:"conflicts,omitempty"`
	Error     string     `json:"error,omitempty"`
}

// importKeyFromRequest authenticates the open endpoint: Authorization:
// Bearer sk-odbimp-… or X-Import-Key.
func (s *Server) importKeyFromRequest(r *http.Request) (config.ImportKey, bool) {
	k := strings.TrimSpace(r.Header.Get("X-Import-Key"))
	if h := r.Header.Get("Authorization"); k == "" && len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		k = strings.TrimSpace(h[7:])
	}
	if k == "" || !strings.HasPrefix(k, importKeyPrefix) {
		return config.ImportKey{}, false
	}
	h := config.HashKey(k)
	for _, ik := range s.cfg.Get().ImportKeys {
		if ik.Enabled && subtle.ConstantTimeCompare([]byte(ik.KeyHash), []byte(h)) == 1 {
			return ik, true
		}
	}
	return config.ImportKey{}, false
}

func (s *Server) handleOpenImport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ip := clientIP(r)
	if s.openAuth.limited(ip) {
		w.Header().Set("Retry-After", "60")
		apiError(w, http.StatusTooManyRequests, errors.New("too many failed authentications, retry in 60s"))
		return
	}
	ik, ok := s.importKeyFromRequest(r)
	if !ok {
		s.openAuth.fail(ip)
		w.Header().Set("WWW-Authenticate", `Bearer realm="opendesign-balancer import"`)
		apiError(w, http.StatusUnauthorized, errors.New("invalid or missing import key"))
		return
	}
	var in importRequest
	if err := readBody(r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	items := in.Accounts
	if len(items) == 0 {
		items = []importItem{in.importItem}
	}
	if len(items) > maxImportBatch {
		apiError(w, http.StatusBadRequest, fmt.Errorf("at most %d accounts per request", maxImportBatch))
		return
	}
	now := time.Now().UTC()
	results := make([]importResult, 0, len(items))
	added, staged, unchanged, failed := 0, 0, 0, 0
	for i, it := range items {
		res := s.importOne(ik, it, now)
		res.Index = i
		switch res.Status {
		case "added":
			added++
		case "staged":
			staged++
		case "unchanged":
			unchanged++
		default:
			failed++
		}
		results = append(results, res)
	}
	status := http.StatusOK
	switch {
	case added+staged+unchanged == 0:
		status = http.StatusBadRequest
	case added+unchanged == 0:
		status = http.StatusAccepted
	}
	writeJSON(w, status, map[string]any{"added": added, "staged": staged, "unchanged": unchanged, "failed": failed, "results": results})
}

// importOne adds one account, stages it when it collides, or rejects it.
func (s *Server) importOne(ik config.ImportKey, it importItem, now time.Time) importResult {
	runtimeKey := strings.TrimSpace(it.RuntimeKey)
	if runtimeKey == "" {
		runtimeKey = strings.TrimSpace(it.APIKey)
	}
	if runtimeKey == "" {
		runtimeKey = strings.TrimSpace(it.Key)
	}
	controlKey := strings.TrimSpace(it.ControlKey)
	name := strings.TrimSpace(it.Name)
	group := strings.TrimSpace(it.Group)
	if group == "" {
		group = ik.Group
	}
	fail := func(msg string) importResult { return importResult{Status: "error", Name: name, Error: msg} }
	switch {
	case runtimeKey == "":
		return fail("runtimeKey is required")
	case !strings.HasPrefix(runtimeKey, "sk-") || len(runtimeKey) > 512:
		return fail("runtimeKey must start with sk-amr-")
	case controlKey != "" && (!strings.HasPrefix(controlKey, "vela_ctrl_") || len(controlKey) > 512):
		return fail("controlKey must start with vela_ctrl_")
	case len([]rune(name)) > 50:
		return fail("name longer than 50")
	case len([]rune(it.Note)) > 400:
		return fail("note longer than 400")
	}
	if err := config.ValidateProxyURL(strings.TrimSpace(it.ProxyURL)); err != nil {
		return fail(err.Error())
	}
	keyName := ik.Name
	if keyName == "" {
		keyName = ik.ID
	}
	for attempt := 0; ; attempt++ {
		res, retry := s.importDecide(ik, it, now, keyName, runtimeKey, controlKey, name, group)
		if !retry || attempt >= 3 {
			return res
		}
	}
}

// importDecide is one attempt of importOne; retry asks for another look
// because a conflict appeared between the read and the write.
func (s *Server) importDecide(ik config.ImportKey, it importItem, now time.Time, keyName, runtimeKey, controlKey, name, group string) (res importResult, retry bool) {
	fail := func(msg string) importResult { return importResult{Status: "error", Name: name, Error: msg} }
	cfg := s.cfg.Get()
	conflicts := findConflicts(cfg, runtimeKey, controlKey, name)
	if len(conflicts) > 0 {
		// The very same account again (same keys, same or no name): nothing to do.
		if c := conflicts[0]; c.Reason == "runtimeKey" {
			if a, ok := cfg.Account(c.AccountID); ok && (controlKey == "" || a.ControlKey == controlKey) && (name == "" || strings.EqualFold(a.Name, name)) && len(conflicts) == 1 {
				return importResult{Status: "unchanged", AccountID: a.ID, Name: a.Name, Conflicts: conflicts}, false
			}
		}
		st := stagedAccount{ID: config.NewID("stg_"), Name: name, Group: group, Note: strings.TrimSpace(it.Note), RuntimeKey: runtimeKey,
			ControlKey: controlKey, ProxyURL: strings.TrimSpace(it.ProxyURL), ImportedAt: now, UpdatedAt: now, ImportKey: keyName, ImportKeyID: ik.ID}
		id, replaced, err := s.staging.put(st)
		if err != nil {
			return fail(err.Error()), false
		}
		log.Printf("open import: staged %s (%s) via import key %s, %d conflict(s), replaced=%v", id, name, ik.ID, len(conflicts), replaced)
		return importResult{Status: "staged", StageID: id, Name: name, Conflicts: conflicts}, false
	}
	a := config.Account{ID: config.NewID("acc_"), Name: name, Group: group, Enabled: true, RuntimeKey: runtimeKey, ControlKey: controlKey,
		ProxyURL: strings.TrimSpace(it.ProxyURL), Note: importNote(now, keyName, it.Note), CreatedAt: now}
	_, err := s.cfg.Update(-1, func(c *config.Config) error {
		// re-check inside the writer: two imports of one key may race
		if cs := findConflicts(c, runtimeKey, controlKey, name); len(cs) > 0 {
			return errConflictRace
		}
		c.Accounts = append(c.Accounts, a)
		return nil
	})
	if errors.Is(err, errConflictRace) {
		return importResult{}, true
	}
	if err != nil {
		return fail(err.Error()), false
	}
	log.Printf("open import: added account %s (%s) to group %s via import key %s", a.ID, a.Name, a.Group, ik.ID)
	return importResult{Status: "added", AccountID: a.ID, Name: a.Name}, false
}

var errConflictRace = errors.New("conflict appeared while writing")

// ---- admin: staging area ----

type stagedView struct {
	stagedAccount
	Conflicts []conflict `json:"conflicts"`
	// CanAdd: no account holds this runtime key, so it may join as a new account.
	CanAdd bool `json:"canAdd"`
}

func (s *Server) stagedViews(cfg *config.Config) []stagedView {
	items := s.staging.list()
	out := make([]stagedView, 0, len(items))
	for _, it := range items {
		cs := findConflicts(cfg, it.RuntimeKey, it.ControlKey, it.Name)
		v := stagedView{stagedAccount: it, Conflicts: cs, CanAdd: !hasReason(cs, "runtimeKey")}
		v.RuntimeKey, v.ControlKey = config.Mask(it.RuntimeKey), config.Mask(it.ControlKey)
		v.ProxyURL = maskProxy(it.ProxyURL)
		if v.Conflicts == nil {
			v.Conflicts = []conflict{}
		}
		out = append(out, v)
	}
	return out
}

func (s *Server) apiStaging(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	writeJSON(w, http.StatusOK, map[string]any{"revision": cfg.Revision, "items": s.stagedViews(cfg)})
}

// applyStaged resolves a staged import. action "overwrite" replaces the keys
// and name of target (default: the strongest conflict); "add" appends it as
// a new account, allowed only while no account holds its runtime key.
func (s *Server) applyStaged(id, action, target string) (string, error) {
	st, ok := s.staging.get(id)
	if !ok {
		return "", errors.New("staged account not found")
	}
	now := time.Now().UTC()
	accountID := ""
	_, err := s.cfg.Update(-1, func(c *config.Config) error {
		cs := findConflicts(c, st.RuntimeKey, st.ControlKey, st.Name)
		switch action {
		case "add", "":
			if action == "" && len(cs) > 0 {
				action = "overwrite"
				break
			}
			if hasReason(cs, "runtimeKey") {
				return errors.New("另一个账号已持有这把运行密钥，只能覆盖它或丢弃")
			}
			a := config.Account{ID: config.NewID("acc_"), Name: st.Name, Group: st.Group, Enabled: true, RuntimeKey: st.RuntimeKey, ControlKey: st.ControlKey,
				ProxyURL: st.ProxyURL, Note: importNote(st.ImportedAt, st.ImportKey, st.Note), CreatedAt: now}
			c.Accounts = append(c.Accounts, a)
			accountID = a.ID
			return nil
		}
		if action != "overwrite" {
			return errors.New("action must be overwrite or add")
		}
		if target == "" {
			if len(cs) == 0 {
				return errors.New("没有可覆盖的账号，请改为添加")
			}
			target = cs[0].AccountID
		}
		for i := range c.Accounts {
			a := &c.Accounts[i]
			if a.ID != target {
				continue
			}
			// the runtime key moves here, so no other account may keep it
			for j := range c.Accounts {
				if j != i && c.Accounts[j].RuntimeKey == st.RuntimeKey {
					return errors.New("运行密钥已被账号 " + c.Accounts[j].Name + " 持有，请覆盖该账号")
				}
			}
			a.RuntimeKey = st.RuntimeKey
			if st.ControlKey != "" {
				a.ControlKey = st.ControlKey
			}
			if st.Name != "" {
				a.Name = st.Name
			}
			if st.ProxyURL != "" {
				a.ProxyURL = st.ProxyURL
			}
			a.Note = clipRunes(importNote(st.ImportedAt, st.ImportKey, st.Note)+"\n覆盖确认 "+now.Local().Format(noteTimeLayout), 500)
			accountID = a.ID
			return nil
		}
		return errors.New("target account not found")
	})
	if err != nil {
		return "", err
	}
	if action == "overwrite" {
		s.pool.Recover(accountID) // new keys: clear a quarantine from the old ones
	}
	if _, err := s.staging.remove(id); err != nil {
		log.Printf("staging: remove %s after apply: %v", id, err)
	}
	log.Printf("staging: %s %s -> account %s", action, id, accountID)
	return accountID, nil
}

func (s *Server) apiStagingApply(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action string `json:"action"`
		Target string `json:"target"`
	}
	if r.ContentLength != 0 {
		if err := readBody(r, &in); err != nil {
			apiError(w, http.StatusBadRequest, err)
			return
		}
	}
	id, err := s.applyStaged(r.PathValue("id"), in.Action, in.Target)
	if err != nil {
		s.configError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accountId": id, "revision": s.cfg.Get().Revision})
}

func (s *Server) apiStagingDiscard(w http.ResponseWriter, r *http.Request) {
	ok, err := s.staging.remove(r.PathValue("id"))
	if err != nil {
		apiError(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		apiError(w, http.StatusNotFound, errors.New("staged account not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- admin: import keys ----

func (s *Server) apiImportKeys(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	type view struct {
		ID        string    `json:"id"`
		Name      string    `json:"name"`
		Group     string    `json:"group"`
		KeyPrefix string    `json:"keyPrefix"`
		Enabled   bool      `json:"enabled"`
		CreatedAt time.Time `json:"createdAt"`
	}
	out := []view{}
	for _, k := range cfg.ImportKeys {
		out = append(out, view{k.ID, k.Name, k.Group, k.KeyPrefix, k.Enabled, k.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"revision": cfg.Revision, "keys": out})
}

func (s *Server) apiCreateImportKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name  string `json:"name"`
		Group string `json:"group"`
	}
	if err := readBody(r, &in); err != nil {
		apiError(w, http.StatusBadRequest, err)
		return
	}
	key := importKeyPrefix + randHex(24)
	k := config.ImportKey{ID: config.NewID("ik_"), Name: strings.TrimSpace(in.Name), Group: strings.TrimSpace(in.Group),
		KeyHash: config.HashKey(key), KeyPrefix: key[:len(importKeyPrefix)+4] + "…" + key[len(key)-4:], Enabled: true, CreatedAt: time.Now().UTC()}
	if _, err := s.cfg.Update(-1, func(c *config.Config) error { c.ImportKeys = append(c.ImportKeys, k); return nil }); err != nil {
		s.configError(w, err)
		return
	}
	// the only time the key is shown
	writeJSON(w, http.StatusOK, map[string]any{"id": k.ID, "key": key})
}

func (s *Server) apiUpdateImportKey(w http.ResponseWriter, r *http.Request) {
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
		for i := range c.ImportKeys {
			k := &c.ImportKeys[i]
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
		return errors.New("import key not found")
	})
	if err != nil {
		s.configError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) apiDeleteImportKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := s.cfg.Update(-1, func(c *config.Config) error {
		for i := range c.ImportKeys {
			if c.ImportKeys[i].ID == id {
				c.ImportKeys = append(c.ImportKeys[:i], c.ImportKeys[i+1:]...)
				return nil
			}
		}
		return errors.New("import key not found")
	})
	if err != nil {
		s.configError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func stagingPath(dataDir string) string { return filepath.Join(dataDir, "staging.json") }
