// Package config is DATA_DIR/config.json: accounts with their keys, the
// downstream client keys, routing and quota settings, and the error rules.
// A loaded Config is immutable: every change goes through Store.Update,
// which clones, validates, writes to disk and only then swaps the pointer,
// so one request keeps one consistent snapshot from start to end.
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"opendesign-balancer/internal/fsutil"
	"opendesign-balancer/internal/rules"
)

const DefaultGroup = "default"

var (
	idRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)
	groupRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,50}$`)
	headerRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)
)

type Upstream struct {
	RuntimeBase string `json:"runtimeBase"`
	ControlBase string `json:"controlBase"`
}

type Admin struct {
	// PasswordHash is "pbkdf2-sha256$<iter>$<salt hex>$<hash hex>".
	PasswordHash string `json:"passwordHash,omitempty"`
}

type ClientKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Group     string    `json:"group"`
	KeyHash   string    `json:"keyHash"`
	KeyPrefix string    `json:"keyPrefix"`
	Key       string    `json:"key,omitempty"` // plaintext so the admin can copy it again; empty for keys created before this field existed
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

// ImportKey authorizes the open account-import endpoint
// (POST /open/v1/accounts/import). It is stored hashed like a ClientKey.
type ImportKey struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Group     string    `json:"group"` // the group imported accounts join when the request names none
	KeyHash   string    `json:"keyHash"`
	KeyPrefix string    `json:"keyPrefix"`
	Key       string    `json:"key,omitempty"` // plaintext so the admin can copy it again; empty for keys created before this field existed
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

type Account struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Email       string    `json:"email,omitempty"`
	UserID      string    `json:"userId,omitempty"`
	Note        string    `json:"note,omitempty"`
	Group       string    `json:"group"`
	Enabled     bool      `json:"enabled"`
	RuntimeKey  string    `json:"runtimeKey"`
	ControlKey  string    `json:"controlKey,omitempty"`
	WorkspaceID string    `json:"workspaceId,omitempty"`
	MaxConc     int       `json:"maxConcurrent"`
	MaxRPM      int       `json:"maxRpm"`
	Weight      int       `json:"weight"`
	Priority    int       `json:"priority"`
	ProxyURL    string    `json:"proxyUrl,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

type Routing struct {
	// Strategy orders the candidates of a conversation nobody is bound to;
	// see internal/routes.
	Strategy string `json:"strategy"`
	// Sticky binds a conversation to the account that first answered it.
	Sticky bool `json:"sticky"`
	// OnBindingBusy: overflow | wait-overflow | reject
	OnBindingBusy  string `json:"onBindingBusy"`
	ExplicitTTLSec int    `json:"explicitTtlSec"`
	FallbackTTLSec int    `json:"fallbackTtlSec"`
	FallbackHash   bool   `json:"fallbackHash"`
	// SessionHeaders are extra request headers (as new-api passes them
	// through) read as an explicit session, ahead of the built-in ones.
	SessionHeaders    []string `json:"sessionHeaders"`
	ConcurrencyWaitMs int      `json:"concurrencyWaitMs"`
	MaxAccounts       int      `json:"maxAccountsPerRequest"`
	HealthFilter      bool     `json:"healthFilter"`
	MinimumHealth     float64  `json:"minimumHealth"`
	// WalletFallback lets an account whose plan windows are used up still
	// serve while its wallet is above $0 (wallet-funded calls cost money).
	WalletFallback bool `json:"walletFallback"`
	// IgnoreCatalog turns off the plan-entitlement gate (each key's own
	// /v1/models); a model it lacks is then only found out by the upstream.
	IgnoreCatalog  bool    `json:"ignoreCatalog"`
	LowShare       float64 `json:"lowShare"`
	SpentShare     float64 `json:"spentShare"`
	FirstEventSec  int     `json:"firstEventTimeoutSec"`
	IdleSec        int     `json:"idleTimeoutSec"`
	HeartbeatSec   int     `json:"heartbeatSec"`
	RequestSec     int     `json:"requestTimeoutSec"`
	MaxBodyMB      int     `json:"maxBodyMB"`
	ModelsCacheSec int     `json:"modelsCacheSec"`
	DirectMaxConns int     `json:"directMaxConns"`
	ProxyMaxConns  int     `json:"proxyMaxConns"`
}

type Quota struct {
	Enabled     bool `json:"enabled"`
	IntervalSec int  `json:"intervalSec"`
	Concurrency int  `json:"concurrency"`
	StaleSec    int  `json:"staleSec"`
}

type Logs struct {
	RetentionDays int `json:"retentionDays"`
	MaxRequests   int `json:"maxRequests"`
	MaxErrors     int `json:"maxErrors"`
	MaxTotalMB    int `json:"maxTotalMB"`
	// Detail records every request's headers and bodies (client side); off by default.
	Detail       bool `json:"detail"`
	MaxDetails   int  `json:"maxDetails"`
	DetailBodyKB int  `json:"detailBodyKB"` // each body is cut to this size
}

type Config struct {
	Version    int               `json:"version"`
	Revision   int64             `json:"revision"`
	Upstream   Upstream          `json:"upstream"`
	Admin      Admin             `json:"admin"`
	ClientKeys []ClientKey       `json:"clientKeys"`
	ImportKeys []ImportKey       `json:"importKeys"`
	Accounts   []Account         `json:"accounts"`
	Routing    Routing           `json:"routing"`
	Quota      Quota             `json:"quota"`
	ErrorRules []rules.ErrorRule `json:"errorRules"`
	RetryRules []rules.RetryRule `json:"retryRules"`
	Logs       Logs              `json:"logs"`
}

func Default() *Config {
	return &Config{
		Version: 1,
		Upstream: Upstream{
			RuntimeBase: "https://amr-link.open-design.ai/v1",
			ControlBase: "https://amr-api.open-design.ai",
		},
		ClientKeys: []ClientKey{},
		ImportKeys: []ImportKey{},
		Accounts:   []Account{},
		Routing: Routing{
			Strategy: "smart", Sticky: true, OnBindingBusy: "overflow",
			ExplicitTTLSec: 7200, FallbackTTLSec: 900, FallbackHash: true,
			SessionHeaders:    []string{},
			ConcurrencyWaitMs: 2000, MaxAccounts: 2,
			HealthFilter: false, MinimumHealth: 0.2,
			WalletFallback: true, LowShare: 90, SpentShare: 98,
			FirstEventSec: 120, IdleSec: 360, HeartbeatSec: 25, RequestSec: 900,
			MaxBodyMB: 64, ModelsCacheSec: 600, DirectMaxConns: 512, ProxyMaxConns: 32,
		},
		Quota:      Quota{Enabled: true, IntervalSec: 60, Concurrency: 2, StaleSec: 900},
		ErrorRules: rules.DefaultErrorRules(),
		RetryRules: rules.DefaultRetryRules(),
		Logs:       Logs{RetentionDays: 30, MaxRequests: 50000, MaxErrors: 10000, MaxTotalMB: 100, MaxDetails: 500, DetailBodyKB: 64},
	}
}

// Strategies are the routing strategies Validate accepts.
var Strategies = []string{"smart", "pace", "least-used", "weighted", "roundrobin", "least-selections", "least-connections", "ordered"}

func NewID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

// HashKey is how client keys are stored and looked up.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func clampInt(v *int, lo, hi int, name string) error {
	if *v < lo || *v > hi {
		return fmt.Errorf("%s must be %d..%d", name, lo, hi)
	}
	return nil
}

var forbiddenProxyChars = regexp.MustCompile(`[\s]`)

func ValidateProxyURL(raw string) error {
	if raw == "" {
		return nil
	}
	if forbiddenProxyChars.MatchString(raw) {
		return errors.New("proxyUrl contains whitespace")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("proxyUrl is not a URL")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return errors.New("proxyUrl scheme must be http, https, socks5 or socks5h")
	}
	if u.Host == "" {
		return errors.New("proxyUrl needs a host")
	}
	return nil
}

// Normalize fills defaults on zero values and validates c in place.
func (c *Config) Normalize() error {
	d := Default()
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	if c.Upstream.RuntimeBase == "" {
		c.Upstream.RuntimeBase = d.Upstream.RuntimeBase
	}
	if c.Upstream.ControlBase == "" {
		c.Upstream.ControlBase = d.Upstream.ControlBase
	}
	c.Upstream.RuntimeBase = strings.TrimRight(c.Upstream.RuntimeBase, "/")
	c.Upstream.ControlBase = strings.TrimRight(c.Upstream.ControlBase, "/")
	for _, u := range []string{c.Upstream.RuntimeBase, c.Upstream.ControlBase} {
		if p, err := url.Parse(u); err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			return fmt.Errorf("invalid upstream URL %q", u)
		}
	}
	if c.ClientKeys == nil {
		c.ClientKeys = []ClientKey{}
	}
	if c.ImportKeys == nil {
		c.ImportKeys = []ImportKey{}
	}
	if c.Accounts == nil {
		c.Accounts = []Account{}
	}
	if c.ErrorRules == nil {
		c.ErrorRules = []rules.ErrorRule{}
	}
	if c.RetryRules == nil {
		c.RetryRules = []rules.RetryRule{}
	}
	if err := rules.ValidateErrorRules(c.ErrorRules); err != nil {
		return err
	}
	if err := rules.ValidateRetryRules(c.RetryRules); err != nil {
		return err
	}

	keyIDs, keyHashes := map[string]bool{}, map[string]bool{}
	for i := range c.ClientKeys {
		k := &c.ClientKeys[i]
		if !idRe.MatchString(k.ID) || keyIDs[k.ID] {
			return fmt.Errorf("clientKeys[%d]: invalid or duplicate id", i)
		}
		keyIDs[k.ID] = true
		if k.Group == "" {
			k.Group = DefaultGroup
		}
		if !groupRe.MatchString(k.Group) {
			return fmt.Errorf("clientKeys[%s]: invalid group", k.ID)
		}
		if len(k.KeyHash) != 64 || keyHashes[k.KeyHash] {
			return fmt.Errorf("clientKeys[%s]: invalid or duplicate key", k.ID)
		}
		keyHashes[k.KeyHash] = true
		if len([]rune(k.Name)) > 50 {
			return fmt.Errorf("clientKeys[%s]: name too long", k.ID)
		}
	}

	impIDs, impHashes := map[string]bool{}, map[string]bool{}
	for i := range c.ImportKeys {
		k := &c.ImportKeys[i]
		if !idRe.MatchString(k.ID) || impIDs[k.ID] {
			return fmt.Errorf("importKeys[%d]: invalid or duplicate id", i)
		}
		impIDs[k.ID] = true
		if k.Group == "" {
			k.Group = DefaultGroup
		}
		if !groupRe.MatchString(k.Group) {
			return fmt.Errorf("importKeys[%s]: invalid group", k.ID)
		}
		if len(k.KeyHash) != 64 || impHashes[k.KeyHash] {
			return fmt.Errorf("importKeys[%s]: invalid or duplicate key", k.ID)
		}
		impHashes[k.KeyHash] = true
		if len([]rune(k.Name)) > 50 {
			return fmt.Errorf("importKeys[%s]: name too long", k.ID)
		}
	}

	accIDs, runtimeKeys := map[string]bool{}, map[string]bool{}
	kept := c.Accounts[:0]
	for i := range c.Accounts {
		a := c.Accounts[i]
		a.RuntimeKey = strings.TrimSpace(a.RuntimeKey)
		a.ControlKey = strings.TrimSpace(a.ControlKey)
		if a.RuntimeKey == "" {
			continue // an account without a runtime key is dropped
		}
		if a.ID == "" {
			a.ID = NewID("acc_")
		}
		if !idRe.MatchString(a.ID) || accIDs[a.ID] {
			return fmt.Errorf("accounts[%d]: invalid or duplicate id", i)
		}
		accIDs[a.ID] = true
		if runtimeKeys[a.RuntimeKey] {
			return fmt.Errorf("accounts[%s]: runtime key already used by another account", a.ID)
		}
		runtimeKeys[a.RuntimeKey] = true
		if !strings.HasPrefix(a.RuntimeKey, "sk-") {
			return fmt.Errorf("accounts[%s]: runtime key must start with sk-amr-", a.ID)
		}
		if a.ControlKey != "" && !strings.HasPrefix(a.ControlKey, "vela_ctrl_") {
			return fmt.Errorf("accounts[%s]: control key must start with vela_ctrl_", a.ID)
		}
		if a.Group == "" {
			a.Group = DefaultGroup
		}
		if !groupRe.MatchString(a.Group) {
			return fmt.Errorf("accounts[%s]: invalid group", a.ID)
		}
		if a.Name == "" {
			a.Name = a.Email
		}
		if a.Name == "" {
			a.Name = a.ID
		}
		if len([]rune(a.Name)) > 50 {
			return fmt.Errorf("accounts[%s]: name longer than 50", a.ID)
		}
		if len([]rune(a.Note)) > 500 {
			return fmt.Errorf("accounts[%s]: note longer than 500", a.ID)
		}
		if a.Weight == 0 {
			a.Weight = 1
		}
		if a.Priority == 0 {
			a.Priority = 50
		}
		if err := clampInt(&a.Weight, 1, 100, "weight"); err != nil {
			return fmt.Errorf("accounts[%s]: %v", a.ID, err)
		}
		if err := clampInt(&a.Priority, 1, 100, "priority"); err != nil {
			return fmt.Errorf("accounts[%s]: %v", a.ID, err)
		}
		if err := clampInt(&a.MaxConc, 0, 10000, "maxConcurrent"); err != nil {
			return fmt.Errorf("accounts[%s]: %v", a.ID, err)
		}
		if err := clampInt(&a.MaxRPM, 0, 100000, "maxRpm"); err != nil {
			return fmt.Errorf("accounts[%s]: %v", a.ID, err)
		}
		if err := ValidateProxyURL(a.ProxyURL); err != nil {
			return fmt.Errorf("accounts[%s]: %v", a.ID, err)
		}
		if a.CreatedAt.IsZero() {
			a.CreatedAt = time.Now().UTC()
		}
		kept = append(kept, a)
	}
	c.Accounts = kept

	r := &c.Routing
	if r.Strategy == "" {
		r.Strategy = d.Routing.Strategy
	}
	okStrategy := false
	for _, s := range Strategies {
		okStrategy = okStrategy || s == r.Strategy
	}
	if !okStrategy {
		return fmt.Errorf("routing.strategy must be one of %v", Strategies)
	}
	if r.OnBindingBusy == "" {
		r.OnBindingBusy = d.Routing.OnBindingBusy
	}
	if r.OnBindingBusy != "overflow" && r.OnBindingBusy != "wait-overflow" && r.OnBindingBusy != "reject" {
		return errors.New("routing.onBindingBusy must be overflow, wait-overflow or reject")
	}
	if r.SessionHeaders == nil {
		r.SessionHeaders = []string{}
	}
	if len(r.SessionHeaders) > 20 {
		return errors.New("routing.sessionHeaders: at most 20")
	}
	for _, h := range r.SessionHeaders {
		if !headerRe.MatchString(h) {
			return fmt.Errorf("routing.sessionHeaders: invalid header %q", h)
		}
	}
	defInt := func(v *int, def int) {
		if *v == 0 {
			*v = def
		}
	}
	defInt(&r.ExplicitTTLSec, d.Routing.ExplicitTTLSec)
	defInt(&r.FallbackTTLSec, d.Routing.FallbackTTLSec)
	defInt(&r.MaxAccounts, d.Routing.MaxAccounts)
	defInt(&r.FirstEventSec, d.Routing.FirstEventSec)
	defInt(&r.IdleSec, d.Routing.IdleSec)
	defInt(&r.HeartbeatSec, d.Routing.HeartbeatSec)
	defInt(&r.RequestSec, d.Routing.RequestSec)
	defInt(&r.MaxBodyMB, d.Routing.MaxBodyMB)
	defInt(&r.ModelsCacheSec, d.Routing.ModelsCacheSec)
	defInt(&r.DirectMaxConns, d.Routing.DirectMaxConns)
	defInt(&r.ProxyMaxConns, d.Routing.ProxyMaxConns)
	if r.LowShare == 0 {
		r.LowShare = d.Routing.LowShare
	}
	if r.SpentShare == 0 {
		r.SpentShare = d.Routing.SpentShare
	}
	checks := []struct {
		v      *int
		lo, hi int
		name   string
	}{
		{&r.ExplicitTTLSec, 60, 7 * 86400, "routing.explicitTtlSec"},
		{&r.FallbackTTLSec, 60, 86400, "routing.fallbackTtlSec"},
		{&r.ConcurrencyWaitMs, 0, 30000, "routing.concurrencyWaitMs"},
		{&r.MaxAccounts, 1, 5, "routing.maxAccountsPerRequest"},
		{&r.FirstEventSec, 5, 600, "routing.firstEventTimeoutSec"},
		{&r.IdleSec, 10, 3600, "routing.idleTimeoutSec"},
		{&r.HeartbeatSec, 5, 300, "routing.heartbeatSec"},
		{&r.RequestSec, 10, 7200, "routing.requestTimeoutSec"},
		{&r.MaxBodyMB, 1, 512, "routing.maxBodyMB"},
		{&r.ModelsCacheSec, 10, 86400, "routing.modelsCacheSec"},
		{&r.DirectMaxConns, 1, 100000, "routing.directMaxConns"},
		{&r.ProxyMaxConns, 1, 10000, "routing.proxyMaxConns"},
	}
	for _, ch := range checks {
		if err := clampInt(ch.v, ch.lo, ch.hi, ch.name); err != nil {
			return err
		}
	}
	if r.MinimumHealth < 0 || r.MinimumHealth > 1 {
		return errors.New("routing.minimumHealth must be 0..1")
	}
	if r.LowShare <= 0 || r.LowShare > 100 || r.SpentShare < r.LowShare || r.SpentShare > 100 {
		return errors.New("routing.lowShare/spentShare must satisfy 0 < low <= spent <= 100")
	}

	q := &c.Quota
	defInt(&q.IntervalSec, d.Quota.IntervalSec)
	defInt(&q.Concurrency, d.Quota.Concurrency)
	defInt(&q.StaleSec, d.Quota.StaleSec)
	if err := clampInt(&q.IntervalSec, 30, 3600, "quota.intervalSec"); err != nil {
		return err
	}
	if err := clampInt(&q.Concurrency, 1, 16, "quota.concurrency"); err != nil {
		return err
	}
	if err := clampInt(&q.StaleSec, 60, 86400, "quota.staleSec"); err != nil {
		return err
	}

	l := &c.Logs
	defInt(&l.RetentionDays, d.Logs.RetentionDays)
	defInt(&l.MaxRequests, d.Logs.MaxRequests)
	defInt(&l.MaxErrors, d.Logs.MaxErrors)
	defInt(&l.MaxTotalMB, d.Logs.MaxTotalMB)
	defInt(&l.MaxDetails, d.Logs.MaxDetails)
	defInt(&l.DetailBodyKB, d.Logs.DetailBodyKB)
	if err := clampInt(&l.RetentionDays, 1, 365, "logs.retentionDays"); err != nil {
		return err
	}
	if err := clampInt(&l.MaxRequests, 100, 500000, "logs.maxRequests"); err != nil {
		return err
	}
	if err := clampInt(&l.MaxErrors, 100, 200000, "logs.maxErrors"); err != nil {
		return err
	}
	if err := clampInt(&l.MaxTotalMB, 10, 10240, "logs.maxTotalMB"); err != nil {
		return err
	}
	if err := clampInt(&l.MaxDetails, 10, 5000, "logs.maxDetails"); err != nil {
		return err
	}
	if err := clampInt(&l.DetailBodyKB, 1, 256, "logs.detailBodyKB"); err != nil {
		return err
	}
	return nil
}

// Clone deep-copies c.
func (c *Config) Clone() *Config {
	data, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}
	out := &Config{}
	if err := json.Unmarshal(data, out); err != nil {
		panic(err)
	}
	return out
}

func (c *Config) Account(id string) (Account, bool) {
	for _, a := range c.Accounts {
		if a.ID == id {
			return a, true
		}
	}
	return Account{}, false
}

// ClientKeyByHash finds an enabled client key.
func (c *Config) ClientKeyByHash(h string) (ClientKey, bool) {
	for _, k := range c.ClientKeys {
		if k.KeyHash == h && k.Enabled {
			return k, true
		}
	}
	return ClientKey{}, false
}

// ImportKeyByHash finds an enabled import key.
func (c *Config) ImportKeyByHash(h string) (ImportKey, bool) {
	for _, k := range c.ImportKeys {
		if k.KeyHash == h && k.Enabled {
			return k, true
		}
	}
	return ImportKey{}, false
}

// ErrConflict is returned when expectedRevision is stale.
var ErrConflict = errors.New("configuration revision conflict")

type Store struct {
	path string
	mu   sync.Mutex // serializes writers
	cur  atomic.Pointer[Config]
	// OnChange is called after a successful update with old and new.
	OnChange func(old, new *Config)
}

// Load reads path, creating a default config when missing. A corrupt file is
// a startup error and is left as it is.
func Load(path string) (*Store, error) {
	s := &Store{path: path}
	c := &Config{}
	err := fsutil.ReadJSON(path, c)
	if errors.Is(err, fsutil.ErrNotExist) {
		c = Default()
		if err := c.Normalize(); err != nil {
			return nil, err
		}
		if err := fsutil.WriteJSONAtomic(path, c); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	} else if err := c.Normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.cur.Store(c)
	return s, nil
}

// Get is the current snapshot; callers must not mutate it.
func (s *Store) Get() *Config { return s.cur.Load() }

// Update applies fn to a clone. expectedRevision < 0 skips the optimistic
// lock. The file is written before the new snapshot becomes visible.
func (s *Store) Update(expectedRevision int64, fn func(c *Config) error) (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.cur.Load()
	if expectedRevision >= 0 && expectedRevision != old.Revision {
		return nil, ErrConflict
	}
	next := old.Clone()
	if err := fn(next); err != nil {
		return nil, err
	}
	if err := next.Normalize(); err != nil {
		return nil, err
	}
	next.Revision = old.Revision + 1
	if err := fsutil.WriteJSONAtomic(s.path, next); err != nil {
		return nil, err
	}
	s.cur.Store(next)
	if s.OnChange != nil {
		s.OnChange(old, next)
	}
	return next, nil
}

// Reload rereads the file (SIGHUP); in-flight requests keep their snapshot.
func (s *Store) Reload() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &Config{}
	if err := fsutil.ReadJSON(s.path, c); err != nil {
		return err
	}
	if err := c.Normalize(); err != nil {
		return err
	}
	old := s.cur.Load()
	s.cur.Store(c)
	if s.OnChange != nil {
		s.OnChange(old, c)
	}
	return nil
}

// Mask shows a key's prefix and last 4 characters only.
func Mask(key string) string {
	if key == "" {
		return ""
	}
	prefix := ""
	for _, p := range []string{"vela_ctrl_", "sk-amr-", "sk-"} {
		if strings.HasPrefix(key, p) {
			prefix = p
			break
		}
	}
	if len(key) <= len(prefix)+8 {
		return prefix + "****"
	}
	return prefix + "****" + key[len(key)-4:]
}
