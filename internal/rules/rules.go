// Package rules holds the ordered error rules (what a failed attempt does to
// an account or an account×model) and retry rules (when to stop switching).
// The first matching rule wins, ignore included. Nothing here cools an
// account down without an explicit rule: an unmatched failure only counts as
// a degrade sample.
package rules

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	ScopeAccount = "account"
	ScopeModel   = "model"
	// ScopeFunding cools every model paid from the same source as the failed
	// one: a Design Plan model refused for money means the windows, the
	// 套餐模型额度 and the wallet are all empty (the whole account); a 套餐模型
	// refused means the 套餐模型额度 and wallet are (all 套餐模型 on it).
	ScopeFunding = "funding"

	ActionIgnore         = "ignore"
	ActionDegrade        = "degrade"
	ActionCooldown       = "cooldown"
	ActionHardQuarantine = "hard-quarantine"

	DecisionStop = "stop"
)

// Match is a rule's condition. Each kind is OR within itself; kinds are
// AND'ed. An empty Match never matches.
type Match struct {
	Statuses     []int    `json:"statuses,omitempty"`
	BodyContains []string `json:"bodyContains,omitempty"`
	// Headers maps a response header name to a case-insensitive substring
	// its value must contain; "" only requires the header to be present.
	Headers map[string]string `json:"headers,omitempty"`
}

// Reset says how long a cooldown lasts: the header, else an RFC3339
// instant following one of BodyAfter's phrases in the body ("try again
// after 2026-10-23T05:00:00Z"), else Fallback; never more than Max.
type Reset struct {
	Header    string   `json:"header,omitempty"`
	Format    string   `json:"format,omitempty"` // retry-after|unix-seconds|unix-milliseconds|duration
	BodyAfter []string `json:"bodyAfter,omitempty"`
	Fallback  string   `json:"fallback"`
	Max       string   `json:"max,omitempty"`
	// Escalate doubles the fallback for each consecutive hit on the same
	// account (reset by a success), up to Max: with no control key there is
	// no other way to learn when money comes back.
	Escalate bool `json:"escalate,omitempty"`
}

type ErrorRule struct {
	ID       string `json:"id"`
	Disabled bool   `json:"disabled,omitempty"`
	Scope    string `json:"scope"`
	Action   string `json:"action"`
	When     Match  `json:"when"`
	Reset    *Reset `json:"reset,omitempty"`
	// RefreshQuota asks the quota poller to read the account again at once.
	RefreshQuota bool `json:"refreshQuota,omitempty"`
	// DownstreamStatus is what the client gets when this is the request's
	// last failure (0 keeps the default mapping). A relay such as new-api
	// retries another channel on 429/5xx but treats a 400 as the caller's
	// fault, so an account-side 400 must not be passed through.
	DownstreamStatus int    `json:"downstreamStatus,omitempty"`
	Note             string `json:"note,omitempty"`
}

type RetryRule struct {
	ID       string `json:"id"`
	Disabled bool   `json:"disabled,omitempty"`
	Decision string `json:"decision"`
	When     Match  `json:"when"`
	Note     string `json:"note,omitempty"`
}

var idRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// DefaultErrorRules are the OpenDesign (vela link) failures, from the
// account-pool handoff and the official client's error catalogue
// (nexu-io/open-design specs/current/run-error-catalog.md,
// apps/daemon/src/integrations/vela-errors.ts,
// packages/contracts/src/runtime/{model-window-limit,membership-concurrency-limit}.ts).
// Order matters: the first match wins.
func DefaultErrorRules() []ErrorRule {
	return []ErrorRule{
		{ID: "platform-provider-credentials", Scope: ScopeAccount, Action: ActionIgnore, DownstreamStatus: 502,
			When: Match{BodyContains: []string{"upstream_provider_unauthenticated", "upstream_provider_forbidden"}},
			Note: "网关自身的供应商凭据故障（link 把上游 401/403 改写成 500），与账号无关：不扣健康分、不换号"},
		{ID: "account-suspended", Scope: ScopeAccount, Action: ActionHardQuarantine, DownstreamStatus: 503,
			When: Match{BodyContains: []string{"account_suspended"}},
			Note: "账号被风控封禁，需人工处理"},
		{ID: "runtime-key-invalid", Scope: ScopeAccount, Action: ActionHardQuarantine,
			When: Match{Statuses: []int{401}, BodyContains: []string{"invalid_api_key", "api key not found", "unauthenticated", "auth_required"}},
			Note: "sk-amr 运行密钥失效，需要重新设备授权"},
		{ID: "model-window-limit", Scope: ScopeModel, Action: ActionCooldown, DownstreamStatus: 429,
			When:  Match{BodyContains: []string{"model_limit_exceeded", "hour usage limit"}},
			Reset: &Reset{Header: "Retry-After", Format: "retry-after", BodyAfter: []string{"try again after"}, Fallback: "30m", Max: "8d"},
			Note:  "该账号此模型的滚动用量窗口用尽（HTTP 400，不扣费），到点自动恢复；换号继续"},
		{ID: "membership-concurrency", Scope: ScopeAccount, Action: ActionCooldown, DownstreamStatus: 429,
			When:  Match{BodyContains: []string{"tier_limit_exceeded"}},
			Reset: &Reset{BodyAfter: []string{"resets at", "reset at", "resets"}, Fallback: "5s", Max: "1m"},
			Note:  "套餐并发/RPM 上限（HTTP 400，如 membership concurrency limit exceeded 6/5），会自动学习上限；短冷却后换号"},
		{ID: "usage-limit-exceeded", Scope: ScopeAccount, Action: ActionCooldown, DownstreamStatus: 429,
			When:  Match{BodyContains: []string{"usage_limit_exceeded"}},
			Reset: &Reset{Header: "Retry-After", Format: "retry-after", BodyAfter: []string{"try again after", "resets at"}, Fallback: "1h", Max: "8d"},
			Note:  "API Key 美元用量上限"},
		{ID: "insufficient-balance", Scope: ScopeFunding, Action: ActionCooldown, RefreshQuota: true, DownstreamStatus: 503,
			When:  Match{BodyContains: []string{"insufficient_balance", "insufficient balance", "insufficient credits", "余额不足", "额度不足", "预扣费额度失败"}},
			Reset: &Reset{Fallback: "10m", Max: "6h", Escalate: true},
			Note:  "付费来源不够：Design Plan 模型报此错则整号冷却，套餐模型报此错则该号所有套餐模型冷却；连续命中冷却翻倍（10 分钟起，最长 6 小时），成功一次清零；有控制密钥的同时刷新额度"},
		{ID: "tier-model-not-entitled", Scope: ScopeModel, Action: ActionCooldown, DownstreamStatus: 503,
			When:  Match{BodyContains: []string{"tier_model_not_entitled", "tier_request_kind_not_entitled"}},
			Reset: &Reset{Fallback: "24h"},
			Note:  "当前套餐不包含该模型或请求类型（如 Plus 不含 Opus 5.5 / Fable 5.1 / GPT-6 Astra）"},
		{ID: "rate-limited", Scope: ScopeAccount, Action: ActionCooldown,
			When:  Match{Statuses: []int{429}},
			Reset: &Reset{Header: "Retry-After", Format: "retry-after", Fallback: "30s", Max: "5m"},
			Note:  "上游限流"},
		{ID: "forbidden", Scope: ScopeAccount, Action: ActionCooldown, DownstreamStatus: 503,
			When:  Match{Statuses: []int{403}},
			Reset: &Reset{Fallback: "5m"},
			Note:  "账号被拒绝访问"},
	}
}

func DefaultRetryRules() []RetryRule {
	return []RetryRule{
		{ID: "bad-request", Decision: DecisionStop, When: Match{Statuses: []int{400, 404, 413, 422}},
			Note: "确定性的请求错误，不换号重试（命中冷却/隔离类错误规则的不算请求错误）"},
	}
}

func validateMatch(m Match) error {
	if len(m.Statuses) == 0 && len(m.BodyContains) == 0 && len(m.Headers) == 0 {
		return fmt.Errorf("when must have at least one condition")
	}
	for _, s := range m.Statuses {
		if s < 100 || s > 599 {
			return fmt.Errorf("invalid status %d", s)
		}
	}
	for _, b := range m.BodyContains {
		if strings.TrimSpace(b) == "" || len(b) > 512 {
			return fmt.Errorf("bodyContains entries must be 1-512 chars")
		}
	}
	for h := range m.Headers {
		if strings.TrimSpace(h) == "" {
			return fmt.Errorf("empty header name")
		}
	}
	return nil
}

func ValidateErrorRules(rs []ErrorRule) error {
	seen := map[string]bool{}
	for i, r := range rs {
		if !idRe.MatchString(r.ID) {
			return fmt.Errorf("errorRules[%d]: invalid id", i)
		}
		if seen[r.ID] {
			return fmt.Errorf("errorRules: duplicate id %q", r.ID)
		}
		seen[r.ID] = true
		if r.Scope != ScopeAccount && r.Scope != ScopeModel && r.Scope != ScopeFunding {
			return fmt.Errorf("errorRules[%s]: scope must be account, model or funding", r.ID)
		}
		if r.Scope == ScopeFunding && r.Action == ActionHardQuarantine {
			return fmt.Errorf("errorRules[%s]: funding scope cannot hard-quarantine", r.ID)
		}
		switch r.Action {
		case ActionIgnore, ActionDegrade, ActionHardQuarantine:
		case ActionCooldown:
			if r.Reset == nil {
				return fmt.Errorf("errorRules[%s]: cooldown needs reset", r.ID)
			}
			if _, err := ParseDuration(r.Reset.Fallback); err != nil {
				return fmt.Errorf("errorRules[%s]: reset.fallback: %v", r.ID, err)
			}
			if r.Reset.Max != "" {
				if _, err := ParseDuration(r.Reset.Max); err != nil {
					return fmt.Errorf("errorRules[%s]: reset.max: %v", r.ID, err)
				}
			}
			for _, p := range r.Reset.BodyAfter {
				if strings.TrimSpace(p) == "" || len(p) > 64 {
					return fmt.Errorf("errorRules[%s]: reset.bodyAfter entries must be 1-64 chars", r.ID)
				}
			}
			switch r.Reset.Format {
			case "", "retry-after", "unix-seconds", "unix-milliseconds", "duration":
			default:
				return fmt.Errorf("errorRules[%s]: invalid reset.format", r.ID)
			}
			if r.Reset.Header != "" && r.Reset.Format == "" {
				return fmt.Errorf("errorRules[%s]: reset.header needs reset.format", r.ID)
			}
		default:
			return fmt.Errorf("errorRules[%s]: invalid action", r.ID)
		}
		if r.DownstreamStatus != 0 && (r.DownstreamStatus < 400 || r.DownstreamStatus > 599) {
			return fmt.Errorf("errorRules[%s]: downstreamStatus must be 400..599", r.ID)
		}
		if r.Scope == ScopeModel && r.Action == ActionHardQuarantine {
			return fmt.Errorf("errorRules[%s]: model scope cannot hard-quarantine", r.ID)
		}
		if err := validateMatch(r.When); err != nil {
			return fmt.Errorf("errorRules[%s]: %v", r.ID, err)
		}
	}
	return nil
}

func ValidateRetryRules(rs []RetryRule) error {
	seen := map[string]bool{}
	for i, r := range rs {
		if !idRe.MatchString(r.ID) {
			return fmt.Errorf("retryRules[%d]: invalid id", i)
		}
		if seen[r.ID] {
			return fmt.Errorf("retryRules: duplicate id %q", r.ID)
		}
		seen[r.ID] = true
		if r.Decision != DecisionStop {
			return fmt.Errorf("retryRules[%s]: decision must be stop", r.ID)
		}
		if err := validateMatch(r.When); err != nil {
			return fmt.Errorf("retryRules[%s]: %v", r.ID, err)
		}
	}
	return nil
}

func (m Match) matches(status int, lowerBody string, h http.Header) bool {
	if len(m.Statuses) == 0 && len(m.BodyContains) == 0 && len(m.Headers) == 0 {
		return false
	}
	if len(m.Statuses) > 0 {
		ok := false
		for _, s := range m.Statuses {
			if s == status {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(m.BodyContains) > 0 {
		ok := false
		for _, b := range m.BodyContains {
			if strings.Contains(lowerBody, strings.ToLower(b)) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for name, want := range m.Headers {
		v := h.Values(name)
		if len(v) == 0 {
			return false
		}
		if want != "" && !strings.Contains(strings.ToLower(strings.Join(v, ",")), strings.ToLower(want)) {
			return false
		}
	}
	return true
}

// MatchError returns the first enabled error rule matching the failure, or
// nil. body must already be redacted and bounded.
func MatchError(rs []ErrorRule, status int, body string, h http.Header) *ErrorRule {
	lower := strings.ToLower(body)
	for i := range rs {
		if !rs[i].Disabled && rs[i].When.matches(status, lower, h) {
			return &rs[i]
		}
	}
	return nil
}

func MatchRetry(rs []RetryRule, status int, body string, h http.Header) *RetryRule {
	lower := strings.ToLower(body)
	for i := range rs {
		if !rs[i].Disabled && rs[i].When.matches(status, lower, h) {
			return &rs[i]
		}
	}
	return nil
}

// Cooldown is how long r's cooldown lasts for a response with headers h and
// (redacted, bounded) body.
func Cooldown(r *ErrorRule, h http.Header, body string, now time.Time) time.Duration {
	return CooldownAfter(r, h, body, now, 1)
}

// CooldownAfter is Cooldown for the strikes-th consecutive hit; with
// Reset.Escalate the fallback doubles per strike.
func CooldownAfter(r *ErrorRule, h http.Header, body string, now time.Time, strikes int) time.Duration {
	if r == nil || r.Reset == nil {
		return 0
	}
	fallback, _ := ParseDuration(r.Reset.Fallback)
	if r.Reset.Escalate {
		for i := 1; i < strikes && fallback < 30*24*time.Hour; i++ {
			fallback *= 2
		}
	}
	d := fallback
	found := false
	if r.Reset.Header != "" {
		if v := strings.TrimSpace(h.Get(r.Reset.Header)); v != "" {
			if got, ok := parseReset(v, r.Reset.Format, now); ok && got > 0 {
				d, found = got, true
			}
		}
	}
	if !found {
		for _, phrase := range r.Reset.BodyAfter {
			if t, ok := InstantAfter(body, phrase); ok {
				if got := t.Sub(now); got > 0 {
					d = got
				}
				break
			}
		}
	}
	if r.Reset.Max != "" {
		if mx, err := ParseDuration(r.Reset.Max); err == nil && d > mx {
			d = mx
		}
	}
	return d
}

var instantRe = regexp.MustCompile(`^\s*:?\s*(\d{4}-\d{2}-\d{2}T[\d:.]+(?:Z|[+-]\d{2}:?\d{2}))`)

// InstantAfter reads the RFC3339 instant that follows phrase in text
// (case-insensitive), as vela writes "Try again after 2026-10-23T05:00:00Z".
func InstantAfter(text, phrase string) (time.Time, bool) {
	lower, p := strings.ToLower(text), strings.ToLower(phrase)
	for from := 0; ; {
		i := strings.Index(lower[from:], p)
		if i < 0 {
			return time.Time{}, false
		}
		rest := text[from+i+len(p):]
		if m := instantRe.FindStringSubmatch(rest); m != nil {
			v := m[1]
			if len(v) > 5 && (v[len(v)-5] == '+' || v[len(v)-5] == '-') && v[len(v)-3] != ':' {
				v = v[:len(v)-2] + ":" + v[len(v)-2:] // +0800 → +08:00
			}
			if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
				return t, true
			}
		}
		from += i + len(p)
	}
}

func parseReset(v, format string, now time.Time) (time.Duration, bool) {
	switch format {
	case "retry-after":
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return time.Duration(n * float64(time.Second)), true
		}
		if t, err := http.ParseTime(v); err == nil {
			return t.Sub(now), true
		}
	case "unix-seconds":
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return time.Unix(0, int64(n*1e9)).Sub(now), true
		}
	case "unix-milliseconds":
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.UnixMilli(n).Sub(now), true
		}
	case "duration":
		if d, err := ParseDuration(v); err == nil {
			return d, true
		}
		if d, err := time.ParseDuration(v); err == nil {
			return d, true
		}
	}
	return 0, false
}

var durRe = regexp.MustCompile(`^(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$`)

// ParseDuration reads "1d2h3m4s" style durations (at least one part).
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	m := durRe.FindStringSubmatch(s)
	if s == "" || m == nil {
		return 0, fmt.Errorf("invalid duration %q (use 1d2h3m4s)", s)
	}
	units := []time.Duration{24 * time.Hour, time.Hour, time.Minute, time.Second}
	var d time.Duration
	for i, u := range units {
		if m[i+1] != "" {
			n, _ := strconv.Atoi(m[i+1])
			d += time.Duration(n) * u
		}
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration %q must be positive", s)
	}
	return d, nil
}
