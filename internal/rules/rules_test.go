package rules

import (
	"net/http"
	"testing"
	"time"
)

func TestDefaultsValidateAndMatch(t *testing.T) {
	rs := DefaultErrorRules()
	if err := ValidateErrorRules(rs); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRetryRules(DefaultRetryRules()); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{401, `{"error":{"code":"invalid_api_key"}}`, "runtime-key-invalid"},
		{429, `HTTP 429: {"code":"insufficient_balance","message":"insufficient balance"}`, "insufficient-balance"},
		{500, `预扣费额度失败`, "insufficient-balance"},
		{400, `{"error":{"code":"model_limit_exceeded","message":"You have reached the 5-hour usage limit for deepseek-v4-flash. Try again after 2026-10-23T05:00:00Z."}}`, "model-window-limit"},
		{400, `{"error":{"code":"tier_limit_exceeded","message":"membership concurrency limit exceeded 6/5"}}`, "membership-concurrency"},
		{429, `{"error":{"code":"usage_limit_exceeded"}}`, "usage-limit-exceeded"},
		{403, `tier_model_not_entitled`, "tier-model-not-entitled"},
		{403, `{"error":{"code":"account_suspended"}}`, "account-suspended"},
		{500, `{"error":{"code":"upstream_provider_unauthenticated","message":"Upstream provider credentials are missing or invalid."}}`, "platform-provider-credentials"},
		{429, `slow down`, "rate-limited"},
		{500, `boom`, ""},
	}
	for _, c := range cases {
		r := MatchError(rs, c.status, c.body, http.Header{})
		got := ""
		if r != nil {
			got = r.ID
		}
		if got != c.want {
			t.Errorf("%d %s → %q, want %q", c.status, c.body, got, c.want)
		}
	}
}

func TestCooldownHeaderAndCap(t *testing.T) {
	r := &ErrorRule{ID: "x", Scope: ScopeAccount, Action: ActionCooldown, When: Match{Statuses: []int{429}},
		Reset: &Reset{Header: "Retry-After", Format: "retry-after", Fallback: "30s", Max: "5m"}}
	h := http.Header{}
	if d := Cooldown(r, h, "", time.Now()); d != 30*time.Second {
		t.Fatalf("fallback = %v", d)
	}
	h.Set("Retry-After", "90")
	if d := Cooldown(r, h, "", time.Now()); d != 90*time.Second {
		t.Fatalf("header = %v", d)
	}
	h.Set("Retry-After", "99999")
	if d := Cooldown(r, h, "", time.Now()); d != 5*time.Minute {
		t.Fatalf("cap = %v", d)
	}
	if d, err := ParseDuration("1d2h3m4s"); err != nil || d != 26*time.Hour+3*time.Minute+4*time.Second {
		t.Fatalf("parse = %v %v", d, err)
	}
	if _, err := ParseDuration("abc"); err == nil {
		t.Fatal("bad duration accepted")
	}
}

func TestValidateRejects(t *testing.T) {
	bad := []ErrorRule{{ID: "x", Scope: ScopeAccount, Action: ActionCooldown, When: Match{Statuses: []int{429}}}}
	if ValidateErrorRules(bad) == nil {
		t.Fatal("cooldown without reset accepted")
	}
	bad = []ErrorRule{{ID: "x", Scope: ScopeModel, Action: ActionHardQuarantine, When: Match{Statuses: []int{401}}}}
	if ValidateErrorRules(bad) == nil {
		t.Fatal("model-scope quarantine accepted")
	}
	bad = []ErrorRule{{ID: "x", Scope: ScopeAccount, Action: ActionDegrade}}
	if ValidateErrorRules(bad) == nil {
		t.Fatal("empty match accepted")
	}
}

func TestCooldownFromBodyInstant(t *testing.T) {
	now := time.Date(2026, 10, 23, 4, 0, 0, 0, time.UTC)
	var r *ErrorRule
	for i, x := range DefaultErrorRules() {
		if x.ID == "model-window-limit" {
			r = &DefaultErrorRules()[i]
		}
	}
	body := `{"error":{"code":"model_limit_exceeded","message":"You have reached the 5-hour usage limit for m. Try again after 2026-10-23T06:30:00Z. This request was not charged to Wallet Credits."}}`
	if d := Cooldown(r, http.Header{}, body, now); d != 150*time.Minute {
		t.Fatalf("body instant = %v", d)
	}
	h := http.Header{}
	h.Set("Retry-After", "60")
	if d := Cooldown(r, h, body, now); d != time.Minute {
		t.Fatalf("header must win: %v", d)
	}
	if tm, ok := InstantAfter("resets at 2026-10-23T12:00:00+0800", "resets at"); !ok || !tm.Equal(time.Date(2026, 10, 23, 4, 0, 0, 0, time.UTC)) {
		t.Fatalf("offset instant = %v %v", tm, ok)
	}
}

func TestEscalatingCooldown(t *testing.T) {
	r := &ErrorRule{ID: "x", Scope: ScopeFunding, Action: ActionCooldown, When: Match{BodyContains: []string{"x"}},
		Reset: &Reset{Fallback: "10m", Max: "6h", Escalate: true}}
	want := []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute, 80 * time.Minute, 160 * time.Minute, 320 * time.Minute, 6 * time.Hour, 6 * time.Hour}
	for i, w := range want {
		if d := CooldownAfter(r, http.Header{}, "", time.Now(), i+1); d != w {
			t.Fatalf("strike %d = %v, want %v", i+1, d, w)
		}
	}
	r.Reset.Escalate = false
	if d := CooldownAfter(r, http.Header{}, "", time.Now(), 5); d != 10*time.Minute {
		t.Fatalf("no escalation = %v", d)
	}
}
