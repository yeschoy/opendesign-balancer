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
	"opendesign-balancer/internal/pricing"
	"opendesign-balancer/internal/rules"
)

const (
	DefaultGroup = "default"
	// RiverGroup is where River accounts land when no group is named.
	RiverGroup = "river"
	// OllamaGroup is where Ollama accounts land when no group is named.
	OllamaGroup = "ollama"
	// ModalGroup is where Modal accounts land when no group is named.
	ModalGroup = "modal"
)

// DefaultGroupFor is the group an account of key's provider joins by default.
func DefaultGroupFor(key string) string {
	return GroupForProvider(ProviderForKey(key))
}

// GroupForProvider is the default group of a provider's pool.
func GroupForProvider(provider string) string {
	switch provider {
	case ProviderRiver:
		return RiverGroup
	case ProviderOllama:
		return OllamaGroup
	case ProviderModal:
		return ModalGroup
	}
	return DefaultGroup
}

var (
	idRe     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)
	groupRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,50}$`)
	headerRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)
)

type Upstream struct {
	RuntimeBase string `json:"runtimeBase"`
	ControlBase string `json:"controlBase"`
	// RiverBase is the OpenAI-compatible endpoint of River (docs.river.ai)
	// that accounts with Provider "river" are sent to.
	RiverBase string `json:"riverBase"`
	// OllamaBase is where accounts with Provider "ollama" send inference.
	// https://ollama.com/v1 serves OpenAI Chat Completions, OpenAI Responses
	// (stateless only) and Anthropic Messages (/v1/messages); a self-hosted
	// server answers at http://host:11434/v1 with the same three paths.
	OllamaBase string `json:"ollamaBase"`
	// ModalBase is the single inference host of Modal's Shared Endpoints.
	// Unlike the other pools a Modal account does not change host per model:
	// the request's model field IS the Endpoint's hostname
	// (my-endpoint.us-west.modal.direct), so one base serves every model.
	// Like the other bases it ends in /v1, which every path is joined to
	// (the inference host answers 404 without it).
	ModalBase string `json:"modalBase"`
}

const (
	ProviderOpenDesign = "opendesign"
	ProviderRiver      = "river"
	ProviderOllama     = "ollama"
	ProviderModal      = "modal"
	RiverKeyPrefix     = "rv_"
	// OllamaKeyPrefix is the prefix the console/import accept for an Ollama
	// key. ollama.com's own keys carry no documented prefix, so an account
	// may also be declared provider "ollama" and keep the key verbatim.
	OllamaKeyPrefix = "oll-"
	// ModalKeyPrefix is a Modal proxy token's Token ID: the full credential is
	// "wk-<id>.ws-<secret>" and goes to the upstream verbatim as the bearer.
	ModalKeyPrefix = "wk-"
)

// Providers are the pools Validate accepts.
var Providers = []string{ProviderOpenDesign, ProviderRiver, ProviderOllama, ProviderModal}

// ProviderForKey infers the provider from a runtime key's prefix. It returns
// "" for a key whose prefix identifies no provider (an ollama.com API key or a
// key of a self-hosted Ollama): such an account must name its provider.
func ProviderForKey(key string) string {
	switch {
	case strings.HasPrefix(key, RiverKeyPrefix):
		return ProviderRiver
	case strings.HasPrefix(key, OllamaKeyPrefix):
		return ProviderOllama
	case strings.HasPrefix(key, ModalKeyPrefix):
		return ProviderModal
	case strings.HasPrefix(key, "sk-"):
		return ProviderOpenDesign
	}
	return ""
}

// ValidRuntimeKey reports whether key can be a runtime key of some provider.
// An Ollama key has no documented shape: ollama.com issues opaque strings and
// a self-hosted server ignores the value ("ollama"), so any token without
// whitespace is accepted for that provider.
func ValidRuntimeKey(key string) bool {
	return ValidRuntimeKeyFor(ProviderForKey(key), key)
}

// ModalTokenIncomplete reports a Modal proxy token that is only the Token ID:
// the credential is "wk-<id>.ws-<secret>", and Modal's proxy answers a bare ID
// with 401 "supply your proxy token as `Bearer wk-<id>.ws-<secret>`" on every
// host and path, never with a model or billing error.
func ModalTokenIncomplete(key string) bool {
	return strings.HasPrefix(key, ModalKeyPrefix) && !strings.Contains(key, ".")
}

// ValidRuntimeKeyFor reports whether key may be provider's runtime key.
func ValidRuntimeKeyFor(provider, key string) bool {
	if key == "" || len(key) > 512 || strings.TrimSpace(key) != key {
		return false
	}
	switch provider {
	case ProviderRiver:
		return strings.HasPrefix(key, RiverKeyPrefix)
	case ProviderOllama:
		return !strings.ContainsAny(key, " \t\r\n")
	case ProviderModal:
		return strings.HasPrefix(key, ModalKeyPrefix) && !strings.ContainsAny(key, " \t\r\n")
	default:
		return strings.HasPrefix(key, "sk-")
	}
}

// KnownProvider reports whether p names a pool.
func KnownProvider(p string) bool {
	for _, x := range Providers {
		if x == p {
			return true
		}
	}
	return false
}

// ResolveProvider settles an account's provider from its declared provider and
// its key. A key prefix that names a provider always wins, so a swapped key
// can't leave a stale provider behind; a key no prefix identifies (an
// ollama.com API key, a self-hosted server's placeholder) keeps the declared
// provider, and OpenDesign when that is empty (a config from before the other
// pools had keys without a prefix).
func ResolveProvider(declared, key string) string {
	switch inferred := ProviderForKey(key); inferred {
	case ProviderOpenDesign, ProviderRiver, ProviderOllama, ProviderModal:
		return inferred
	}
	if declared == "" {
		return ProviderOpenDesign
	}
	return declared
}

// runtimeKeyError is the hint for a key its declared provider can't accept.
func runtimeKeyError(provider string) string {
	switch provider {
	case ProviderRiver:
		return "runtime key must start with rv_"
	case ProviderOllama:
		return "runtime key must not contain whitespace"
	case ProviderModal:
		return "runtime key must be Modal's proxy token wk-<id>.<secret>"
	default:
		return "runtime key must start with sk-amr- (or rv_ for River, oll- for Ollama, wk- for Modal)"
	}
}

// RulesFor are the error and retry rules of an account's pool.
func (c *Config) RulesFor(provider string) ([]rules.ErrorRule, []rules.RetryRule) {
	switch provider {
	case ProviderRiver:
		return c.RiverErrorRules, c.RiverRetryRules
	case ProviderOllama:
		return c.OllamaErrorRules, c.OllamaRetryRules
	case ProviderModal:
		return c.ModalErrorRules, c.ModalRetryRules
	}
	return c.ErrorRules, c.RetryRules
}

// DefaultErrorRulesFor is a pool's stock error rules (the console's
// "restore defaults").
func DefaultErrorRulesFor(provider string) []rules.ErrorRule {
	switch provider {
	case ProviderRiver:
		return rules.DefaultRiverErrorRules()
	case ProviderOllama:
		return rules.DefaultOllamaErrorRules()
	case ProviderModal:
		return rules.DefaultModalErrorRules()
	}
	return rules.DefaultErrorRules()
}

// DefaultRetryRulesFor is a pool's stock retry rules.
func DefaultRetryRulesFor(provider string) []rules.RetryRule {
	switch provider {
	case ProviderRiver:
		return rules.DefaultRiverRetryRules()
	case ProviderOllama:
		return rules.DefaultOllamaRetryRules()
	case ProviderModal:
		return rules.DefaultModalRetryRules()
	}
	return rules.DefaultRetryRules()
}

// GroupProvider is the provider whose accounts group holds ("" when it has
// none yet). Normalize keeps a group to one provider.
func (c *Config) GroupProvider(group string) string {
	for _, a := range c.Accounts {
		if a.Group == group {
			return a.Provider
		}
	}
	return ""
}

// GroupPool is the pool a group belongs to, for keys, import keys and logs:
// an account of it decides; a group that still holds no account is read off
// its keys, then off its name (so `ollama` is Ollama's pool before the first
// account arrives). "" means no pool could be told.
func (c *Config) GroupPool(group string) string {
	if p := c.GroupProvider(group); p != "" {
		return p
	}
	// the stock group names come first: a key must not be able to redefine a
	// pool's own group by declaring itself another pool's
	for _, p := range Providers {
		if GroupForProvider(p) == group {
			return p
		}
	}
	for _, k := range c.ClientKeys {
		if k.Group == group && k.Provider != "" {
			return k.Provider
		}
	}
	for _, k := range c.ImportKeys {
		if k.Group == group && k.Provider != "" {
			return k.Provider
		}
	}
	return ""
}

// keyProvider settles a key's pool: the pool its group already belongs to
// decides, otherwise the value the key carries, otherwise the group's name,
// and OpenDesign last. A declared provider that contradicts the group is
// refused by keyProviderErr rather than quietly rewritten, so a key can never
// be moved into another pool by a typo.
func (c *Config) keyProvider(declared, group string) (string, error) {
	if declared != "" && !KnownProvider(declared) {
		return "", fmt.Errorf("provider must be opendesign, river, ollama or modal")
	}
	// a group an account already holds is that pool's, full stop
	if pool := c.GroupProvider(group); pool != "" {
		if declared != "" && declared != pool {
			return "", fmt.Errorf("group %q holds %s accounts, so it belongs to the %s pool, not %s", group, pool, pool, declared)
		}
		return pool, nil
	}
	// no account yet: the group's name, else the key's own pool
	if pool := c.GroupPool(group); pool != "" {
		if declared != "" && declared != pool {
			return "", fmt.Errorf("group %q is the %s pool's, but the key declares %s", group, pool, declared)
		}
		return pool, nil
	}
	if declared != "" {
		return declared, nil
	}
	return ProviderOpenDesign, nil
}

// RuntimeBase is the inference endpoint account a is served from.
func (c *Config) RuntimeBase(a Account) string {
	switch a.Provider {
	case ProviderRiver:
		return c.Upstream.RiverBase
	case ProviderOllama:
		return c.Upstream.OllamaBase
	case ProviderModal:
		return c.Upstream.ModalBase
	}
	return c.Upstream.RuntimeBase
}

// ControlBase is where an account's quota is read. Only OpenDesign has a
// separate control plane behind a vela_ctrl_ key; Ollama reports balances at
// /api/balance for the same API key, on the host of its inference base
// (https://ollama.com/v1 → https://ollama.com/api/balance), and River reports
// nothing.
func (c *Config) ControlBase(a Account) string {
	if a.Provider == ProviderOllama {
		return OllamaAPIRoot(c.Upstream.OllamaBase)
	}
	return c.Upstream.ControlBase
}

// OllamaAPIRoot turns an Ollama inference base into the root the native /api
// endpoints hang off: https://ollama.com/v1 → https://ollama.com.
func OllamaAPIRoot(base string) string {
	base = strings.TrimRight(base, "/")
	for _, suffix := range []string{"/v1", "/api"} {
		if strings.HasSuffix(base, suffix) {
			return strings.TrimSuffix(base, suffix)
		}
	}
	return base
}

// BalancePath is the path of the balance read on ControlBase ("" when the
// provider has none).
func BalancePath(provider string) string {
	if provider == ProviderOllama {
		return "/api/balance"
	}
	return ""
}

type Admin struct {
	// PasswordHash is "pbkdf2-sha256$<iter>$<salt hex>$<hash hex>".
	PasswordHash string `json:"passwordHash,omitempty"`
}

type ClientKey struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Group string `json:"group"`
	// Provider is the pool this key belongs to. It follows its group's pool
	// when empty, and an account of the group always decides over it, so a
	// stale value can't route a key into another pool.
	Provider  string    `json:"provider,omitempty"`
	KeyHash   string    `json:"keyHash"`
	KeyPrefix string    `json:"keyPrefix"`
	Key       string    `json:"key,omitempty"` // plaintext so the admin can copy it again; empty for keys created before this field existed
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

// ImportKey authorizes the open account-import endpoint
// (POST /open/v1/accounts/import). It is stored hashed like a ClientKey.
type ImportKey struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Group string `json:"group"` // the group imported accounts join when the request names none
	// Provider is the pool imported accounts join when the request names
	// none (see ClientKey.Provider).
	Provider  string    `json:"provider,omitempty"`
	KeyHash   string    `json:"keyHash"`
	KeyPrefix string    `json:"keyPrefix"`
	Key       string    `json:"key,omitempty"` // plaintext so the admin can copy it again; empty for keys created before this field existed
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

type Account struct {
	ID          string    `json:"id"`
	Provider    string    `json:"provider,omitempty"` // opendesign (default) | river | ollama
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
	Version    int         `json:"version"`
	Revision   int64       `json:"revision"`
	Upstream   Upstream    `json:"upstream"`
	Admin      Admin       `json:"admin"`
	ClientKeys []ClientKey `json:"clientKeys"`
	ImportKeys []ImportKey `json:"importKeys"`
	Accounts   []Account   `json:"accounts"`
	Routing    Routing     `json:"routing"`
	Quota      Quota       `json:"quota"`
	// ErrorRules / RetryRules are the OpenDesign pool's; River and Ollama
	// have their own.
	ErrorRules       []rules.ErrorRule `json:"errorRules"`
	RetryRules       []rules.RetryRule `json:"retryRules"`
	RiverErrorRules  []rules.ErrorRule `json:"riverErrorRules"`
	RiverRetryRules  []rules.RetryRule `json:"riverRetryRules"`
	OllamaErrorRules []rules.ErrorRule `json:"ollamaErrorRules"`
	OllamaRetryRules []rules.RetryRule `json:"ollamaRetryRules"`
	ModalErrorRules  []rules.ErrorRule `json:"modalErrorRules"`
	ModalRetryRules  []rules.RetryRule `json:"modalRetryRules"`
	Logs             Logs              `json:"logs"`
	Pricing          pricing.Config    `json:"pricing"`
}

func Default() *Config {
	return &Config{
		Version: 1,
		Upstream: Upstream{
			RuntimeBase: "https://amr-link.open-design.ai/v1",
			ControlBase: "https://amr-api.open-design.ai",
			RiverBase:   "https://api.river.ai/v1",
			OllamaBase:  "https://ollama.com/v1",
			ModalBase:   "https://inference.us-west.modal.direct/v1",
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
		Quota:            Quota{Enabled: true, IntervalSec: 60, Concurrency: 2, StaleSec: 900},
		ErrorRules:       rules.DefaultErrorRules(),
		RetryRules:       rules.DefaultRetryRules(),
		RiverErrorRules:  rules.DefaultRiverErrorRules(),
		RiverRetryRules:  rules.DefaultRiverRetryRules(),
		OllamaErrorRules: rules.DefaultOllamaErrorRules(),
		OllamaRetryRules: rules.DefaultOllamaRetryRules(),
		ModalErrorRules:  rules.DefaultModalErrorRules(),
		ModalRetryRules:  rules.DefaultModalRetryRules(),
		Pricing:          pricing.Config{Models: pricing.Defaults(), Holidays: []string{}},
		Logs:             Logs{RetentionDays: 30, MaxRequests: 50000, MaxErrors: 10000, MaxTotalMB: 100, MaxDetails: 500, DetailBodyKB: 6144},
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
	if c.Upstream.RiverBase == "" {
		c.Upstream.RiverBase = d.Upstream.RiverBase
	}
	if c.Upstream.OllamaBase == "" {
		c.Upstream.OllamaBase = d.Upstream.OllamaBase
	}
	if c.Upstream.ModalBase == "" {
		c.Upstream.ModalBase = d.Upstream.ModalBase
	}
	// every pool's base carries the /v1 every path is joined to; Modal's
	// host answers 404 without it, so a base configured as the bare host
	// (as the dashboard prints it) is completed here rather than failing
	// every request with somebody else's 404
	if c.Upstream.ModalBase != "" && !strings.HasSuffix(strings.TrimRight(c.Upstream.ModalBase, "/"), "/v1") {
		t := strings.TrimRight(c.Upstream.ModalBase, "/")
		if u, err := url.Parse(t); err == nil && (u.Path == "" || u.Path == "/") {
			c.Upstream.ModalBase = t + "/v1"
		}
	}
	c.Upstream.RiverBase = strings.TrimRight(c.Upstream.RiverBase, "/")
	c.Upstream.RuntimeBase = strings.TrimRight(c.Upstream.RuntimeBase, "/")
	c.Upstream.ControlBase = strings.TrimRight(c.Upstream.ControlBase, "/")
	c.Upstream.OllamaBase = strings.TrimRight(c.Upstream.OllamaBase, "/")
	c.Upstream.ModalBase = strings.TrimRight(c.Upstream.ModalBase, "/")
	for _, u := range []string{c.Upstream.RuntimeBase, c.Upstream.ControlBase, c.Upstream.RiverBase, c.Upstream.OllamaBase, c.Upstream.ModalBase} {
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
	// a config written before the River pool has no River rules: start from
	// the defaults (an explicit empty list stays empty)
	if c.RiverErrorRules == nil {
		c.RiverErrorRules = rules.DefaultRiverErrorRules()
	}
	if c.RiverRetryRules == nil {
		c.RiverRetryRules = rules.DefaultRiverRetryRules()
	}
	if err := rules.ValidateErrorRules(c.RiverErrorRules); err != nil {
		return fmt.Errorf("river: %w", err)
	}
	if err := rules.ValidateRetryRules(c.RiverRetryRules); err != nil {
		return fmt.Errorf("river: %w", err)
	}
	// same for a config written before the Ollama pool
	if c.OllamaErrorRules == nil {
		c.OllamaErrorRules = rules.DefaultOllamaErrorRules()
	}
	if c.OllamaRetryRules == nil {
		c.OllamaRetryRules = rules.DefaultOllamaRetryRules()
	}
	if err := rules.ValidateErrorRules(c.OllamaErrorRules); err != nil {
		return fmt.Errorf("ollama: %w", err)
	}
	if err := rules.ValidateRetryRules(c.OllamaRetryRules); err != nil {
		return fmt.Errorf("ollama: %w", err)
	}
	// and for a config written before the Modal pool
	if c.ModalErrorRules == nil {
		c.ModalErrorRules = rules.DefaultModalErrorRules()
	}
	if c.ModalRetryRules == nil {
		c.ModalRetryRules = rules.DefaultModalRetryRules()
	}
	if err := rules.ValidateErrorRules(c.ModalErrorRules); err != nil {
		return fmt.Errorf("modal: %w", err)
	}
	if err := rules.ValidateRetryRules(c.ModalRetryRules); err != nil {
		return fmt.Errorf("modal: %w", err)
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
	groupProvider := map[string]string{}
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
		// the key's prefix settles the provider when it names one, so a
		// swapped key can never leave a stale provider behind; otherwise the
		// stored provider stands (an Ollama key carries no prefix)
		a.Provider = ResolveProvider(a.Provider, a.RuntimeKey)
		if !ValidRuntimeKeyFor(a.Provider, a.RuntimeKey) {
			return fmt.Errorf("accounts[%s]: %v", a.ID, runtimeKeyError(a.Provider))
		}
		if a.Provider == ProviderRiver || a.Provider == ProviderOllama || a.Provider == ProviderModal {
			if a.ControlKey != "" {
				return fmt.Errorf("accounts[%s]: %s accounts have no control key", a.ID, a.Provider)
			}
		}
		if a.ControlKey != "" && !strings.HasPrefix(a.ControlKey, "vela_ctrl_") {
			return fmt.Errorf("accounts[%s]: control key must start with vela_ctrl_", a.ID)
		}
		if a.Group == "" {
			a.Group = GroupForProvider(a.Provider)
		}
		if !groupRe.MatchString(a.Group) {
			return fmt.Errorf("accounts[%s]: invalid group", a.ID)
		}
		if p, ok := groupProvider[a.Group]; ok && p != a.Provider {
			return fmt.Errorf("accounts[%s]: group %q already holds %s accounts; each pool needs its own group", a.ID, a.Group, p)
		}
		groupProvider[a.Group] = a.Provider
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
	// A key belongs to the pool of its group; that pool's accounts (if any)
	// decide, so a key can't be routed into another pool by a stale field.
	for i := range c.ClientKeys {
		p, err := c.keyProvider(c.ClientKeys[i].Provider, c.ClientKeys[i].Group)
		if err != nil {
			return fmt.Errorf("clientKeys[%s]: %w", c.ClientKeys[i].ID, err)
		}
		c.ClientKeys[i].Provider = p
	}
	for i := range c.ImportKeys {
		p, err := c.keyProvider(c.ImportKeys[i].Provider, c.ImportKeys[i].Group)
		if err != nil {
			return fmt.Errorf("importKeys[%s]: %w", c.ImportKeys[i].ID, err)
		}
		c.ImportKeys[i].Provider = p
	}

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
	if err := clampInt(&l.DetailBodyKB, 1, 6144, "logs.detailBodyKB"); err != nil {
		return err
	}
	if err := c.Pricing.Normalize(); err != nil {
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
	for _, p := range []string{"vela_ctrl_", "sk-amr-", "sk-", RiverKeyPrefix, OllamaKeyPrefix, ModalKeyPrefix} {
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
