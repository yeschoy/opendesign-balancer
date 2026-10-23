package pool

import (
	"context"
	"errors"
	"testing"
	"time"

	"opendesign-balancer/internal/config"
	"opendesign-balancer/internal/quota"
)

func testConfig(accts ...config.Account) *config.Config {
	c := config.Default()
	for i := range accts {
		if accts[i].Group == "" {
			accts[i].Group = "default"
		}
		if accts[i].RuntimeKey == "" {
			accts[i].RuntimeKey = "sk-amr-" + accts[i].ID
		}
		accts[i].Enabled = true
	}
	c.Accounts = accts
	if err := c.Normalize(); err != nil {
		panic(err)
	}
	return c
}

func acquire(t *testing.T, p *Pool, cfg *config.Config, req Request) *Lease {
	t.Helper()
	if req.Group == "" {
		req.Group = "default"
	}
	l, err := p.Acquire(context.Background(), cfg, req)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	return l
}

func TestReleaseIsIdempotent(t *testing.T) {
	p := New(nil)
	cfg := testConfig(config.Account{ID: "a", MaxConc: 2})
	l := acquire(t, p, cfg, Request{RequestID: "r1"})
	l.Release()
	l.Release()
	if p.active["a"] != 0 {
		t.Fatalf("active = %d after double release", p.active["a"])
	}
	l2 := acquire(t, p, cfg, Request{RequestID: "r2"})
	l3 := acquire(t, p, cfg, Request{RequestID: "r3"})
	l2.Release()
	l2.Release()
	if p.active["a"] != 1 {
		t.Fatalf("double release freed someone else's slot: active = %d", p.active["a"])
	}
	l3.Release()
}

func TestConcurrencyFullSpendsNoRPM(t *testing.T) {
	p := New(nil)
	cfg := testConfig(config.Account{ID: "a", MaxConc: 1, MaxRPM: 5})
	l := acquire(t, p, cfg, Request{RequestID: "r1"})
	for range 10 {
		_, err := p.Acquire(context.Background(), cfg, Request{Group: "default", RequestID: "x"})
		var ce *CapacityError
		if !errors.As(err, &ce) || ce.BlockedBy != "concurrency" {
			t.Fatalf("want concurrency block, got %v", err)
		}
	}
	if w := p.rpm["a"]; w.reservations != 1 || len(w.stamps) != 0 {
		t.Fatalf("rpm window touched by blocked requests: %+v", *w)
	}
	l.Release()
}

func TestUnsentPermitIsReturned(t *testing.T) {
	p := New(nil)
	cfg := testConfig(config.Account{ID: "a", MaxRPM: 1})
	l := acquire(t, p, cfg, Request{RequestID: "r1"})
	l.Release() // never sent
	if w := p.rpm["a"]; w.reservations != 0 || len(w.stamps) != 0 {
		t.Fatalf("unsent permit kept: %+v", *w)
	}
	l = acquire(t, p, cfg, Request{RequestID: "r2"})
	l.MarkSent()
	l.Release()
	if _, err := p.Acquire(context.Background(), cfg, Request{Group: "default", RequestID: "r3"}); err == nil {
		t.Fatal("a sent permit must stay in the window")
	} else {
		var ce *CapacityError
		if !errors.As(err, &ce) || ce.BlockedBy != "rpm" || ce.RetryAfter <= 0 {
			t.Fatalf("want rpm block with retry, got %v", err)
		}
	}
}

func TestBindingHitAndProvisionalOwnership(t *testing.T) {
	p := New(nil)
	cfg := testConfig(config.Account{ID: "a"}, config.Account{ID: "b"})
	l1 := acquire(t, p, cfg, Request{RequestID: "r1", Affinity: "s1", Explicit: true})
	if l1.Decision.BindingResult != "miss" {
		t.Fatalf("first = %s", l1.Decision.BindingResult)
	}
	// a concurrent request of the same session sees the provisional binding
	l2 := acquire(t, p, cfg, Request{RequestID: "r2", Affinity: "s1", Explicit: true})
	if l2.Decision.AccountID != l1.Decision.AccountID || l2.Decision.BindingResult != "provisional" {
		t.Fatalf("second = %+v", l2.Decision)
	}
	// r2 does not own the binding: its release must not remove it
	l2.Release()
	if p.bindings["s1"] == nil {
		t.Fatal("non-owner removed the provisional binding")
	}
	// the owner fails before sending: its provisional binding goes
	l1.Release()
	if p.bindings["s1"] != nil {
		t.Fatal("owner's unsent provisional binding survived")
	}
	l3 := acquire(t, p, cfg, Request{RequestID: "r3", Affinity: "s1", Explicit: true})
	l3.MarkSent()
	l3.Release()
	l4 := acquire(t, p, cfg, Request{RequestID: "r4", Affinity: "s1", Explicit: true})
	if l4.Decision.BindingResult != "hit" || l4.Decision.AccountID != l3.Decision.AccountID {
		t.Fatalf("confirmed binding not hit: %+v", l4.Decision)
	}
	l4.Release()
}

func TestOverflowDoesNotRebind(t *testing.T) {
	p := New(nil)
	cfg := testConfig(config.Account{ID: "a", MaxConc: 1}, config.Account{ID: "b", MaxConc: 1})
	l1 := acquire(t, p, cfg, Request{RequestID: "r1", Affinity: "s", Explicit: true})
	l1.MarkSent()
	bound := l1.Decision.AccountID
	l2 := acquire(t, p, cfg, Request{RequestID: "r2", Affinity: "s", Explicit: true})
	if l2.Decision.BindingResult != "temporary-overflow" || l2.Decision.AccountID == bound {
		t.Fatalf("want overflow to the other account, got %+v", l2.Decision)
	}
	l2.MarkSent()
	l2.Release()
	l1.Release()
	if b := p.bindings["s"].Value.(*binding); b.accountID != bound {
		t.Fatalf("overflow rebound the session to %s", b.accountID)
	}
}

func TestRejectWhenBoundBusy(t *testing.T) {
	p := New(nil)
	cfg := testConfig(config.Account{ID: "a", MaxConc: 1}, config.Account{ID: "b", MaxConc: 1})
	cfg.Routing.OnBindingBusy = "reject"
	l1 := acquire(t, p, cfg, Request{RequestID: "r1", Affinity: "s", Explicit: true})
	l1.MarkSent()
	_, err := p.Acquire(context.Background(), cfg, Request{Group: "default", RequestID: "r2", Affinity: "s", Explicit: true})
	var ce *CapacityError
	if !errors.As(err, &ce) {
		t.Fatalf("reject mode must answer capacity error, got %v", err)
	}
	l1.Release()
}

func TestExcludedNeverReselected(t *testing.T) {
	p := New(nil)
	cfg := testConfig(config.Account{ID: "a"}, config.Account{ID: "b"})
	for range 20 {
		l := acquire(t, p, cfg, Request{RequestID: "r", Excluded: map[string]bool{"a": true}})
		if l.Account.ID == "a" {
			t.Fatal("excluded account selected")
		}
		l.Release()
	}
	_, err := p.Acquire(context.Background(), cfg, Request{Group: "default", Excluded: map[string]bool{"a": true, "b": true}})
	var ue *UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("want unavailable, got %v", err)
	}
}

func TestGroupsNeverBorrow(t *testing.T) {
	p := New(nil)
	cfg := testConfig(config.Account{ID: "a", Group: "x"})
	_, err := p.Acquire(context.Background(), cfg, Request{Group: "default"})
	var ue *UnavailableError
	if !errors.As(err, &ue) || ue.Reason != "no-accounts" {
		t.Fatalf("want no-accounts, got %v", err)
	}
}

func TestFundingOrderAndExclusion(t *testing.T) {
	now := time.Now()
	yes, no := true, false
	win := func(rem int64) []quota.Window {
		return []quota.Window{{DurationSeconds: 604800, Limit: 100, Remaining: rem, ResetsAt: now.Add(time.Hour)}}
	}
	sum := func(sub, rech string) *quota.Summary {
		return &quota.Summary{SubscriptionCredits: sub, RechargeCredits: rech, PeriodEnd: now.Add(48 * time.Hour)}
	}
	cov := map[string]*quota.Coverage{"plan-m": {Covered: &yes}, "pkg-m": {Covered: &no}}
	snap := func(rem int64, wallet, sub string) *quota.Snapshot {
		return &quota.Snapshot{PlanAt: now, SummaryAt: now, Eligible: true, Windows: win(rem), WalletUSD: wallet, Summary: sum(sub, "0"), Coverage: cov}
	}
	snaps := map[string]*quota.Snapshot{
		"spent":   snap(0, "0.0000", "0"),
		"wallet":  snap(0, "3.5000", "0"),
		"sub":     snap(0, "0", "5000"),
		"room":    snap(40, "0", "0"),
		"nosum":   {PlanAt: now, Eligible: true, Windows: win(0), WalletUSD: "0", Coverage: cov},
		"roomsub": snap(40, "0", "5000"),
	}
	p := New(func(id string) *quota.Snapshot { return snaps[id] })
	cfg := testConfig(config.Account{ID: "spent"}, config.Account{ID: "wallet"}, config.Account{ID: "sub"}, config.Account{ID: "room"},
		config.Account{ID: "nosum"}, config.Account{ID: "roomsub"}, config.Account{ID: "cool"})
	p.Cooldown("cool", "", now.Add(time.Minute), 429, "x", "x")

	// a Design Plan model: plan room first, then subscription credits, then wallet
	order, excluded := p.Preview(cfg, "default", "plan-m")
	if excluded["spent"] != "quota-exhausted" || excluded["cool"] != "cooldown" {
		t.Fatalf("excluded = %v", excluded)
	}
	if excluded["nosum"] != "" {
		t.Fatal("a zero wallet alone (summary unknown) must not prove exhaustion")
	}
	pos := map[string]int{}
	for i, id := range order {
		pos[id] = i
	}
	if !(pos["room"] < pos["sub"] && pos["roomsub"] < pos["sub"] && pos["sub"] < pos["wallet"]) {
		t.Fatalf("plan-model order = %v", order)
	}
	// a 套餐模型 never draws on the plan windows
	_, excluded = p.Preview(cfg, "default", "pkg-m")
	if excluded["room"] != "quota-exhausted" || excluded["roomsub"] != "" || excluded["sub"] != "" {
		t.Fatalf("package-model exclusion = %v", excluded)
	}
	cfg.Routing.WalletFallback = false
	if _, ex := p.Preview(cfg, "default", "plan-m"); ex["wallet"] != "quota-exhausted" || ex["sub"] != "" {
		t.Fatalf("wallet fallback off must exclude wallet only: %v", ex)
	}
	p.Cooldown("room", "m1", now.Add(time.Minute), 429, "x", "x")
	if _, ex := p.Preview(cfg, "default", "m1"); ex["room"] != "model-cooldown" {
		t.Fatalf("model cooldown not applied: %v", ex)
	}
	if _, ex := p.Preview(cfg, "default", "plan-m"); ex["room"] != "" {
		t.Fatalf("model cooldown leaked to another model: %v", ex)
	}
}

func TestLearnedLimitCapsConcurrency(t *testing.T) {
	p := New(nil)
	cfg := testConfig(config.Account{ID: "a", MaxConc: 10})
	p.Learn("a", 1, 0)
	l := acquire(t, p, cfg, Request{RequestID: "r1"})
	_, err := p.Acquire(context.Background(), cfg, Request{Group: "default"})
	var ce *CapacityError
	if !errors.As(err, &ce) || ce.BlockedBy != "concurrency" {
		t.Fatalf("learned limit 1 not applied: %v", err)
	}
	l.Release()
	p.Recover("a")
	l1 := acquire(t, p, cfg, Request{RequestID: "r2"})
	l2 := acquire(t, p, cfg, Request{RequestID: "r3"})
	l1.Release()
	l2.Release()
}

func TestAllCoolingDownGivesRetryAfter(t *testing.T) {
	p := New(nil)
	cfg := testConfig(config.Account{ID: "a"}, config.Account{ID: "b"})
	now := time.Now()
	p.Cooldown("a", "m", now.Add(time.Minute), 400, "x", "x")
	p.Cooldown("b", "m", now.Add(10*time.Second), 400, "x", "x")
	_, err := p.Acquire(context.Background(), cfg, Request{Group: "default", Model: "m"})
	var ue *UnavailableError
	if !errors.As(err, &ue) || ue.RetryAfter <= 0 || ue.RetryAfter > 10*time.Second {
		t.Fatalf("want retry-after ~10s, got %v", err)
	}
	p.Quarantine("b", 401, "x", "x")
	_, err = p.Acquire(context.Background(), cfg, Request{Group: "default", Model: "m"})
	if !errors.As(err, &ue) || ue.RetryAfter < 30*time.Second {
		t.Fatalf("a quarantined account isn't waitable but a still is: %v", err)
	}
}

func TestWaitForCapacity(t *testing.T) {
	p := New(nil)
	cfg := testConfig(config.Account{ID: "a", MaxConc: 1})
	l := acquire(t, p, cfg, Request{RequestID: "r1"})
	go func() { time.Sleep(50 * time.Millisecond); l.Release() }()
	start := time.Now()
	l2 := acquire(t, p, cfg, Request{RequestID: "r2", Wait: 2 * time.Second})
	if time.Since(start) > time.Second {
		t.Fatal("waiter was not woken by release")
	}
	l2.Release()
}

func TestSelectionCounting(t *testing.T) {
	p := New(nil)
	cfg := testConfig(config.Account{ID: "a"}, config.Account{ID: "b"})
	l := acquire(t, p, cfg, Request{RequestID: "r1", Affinity: "s", Explicit: true})
	l.MarkSent()
	l.Release()
	for range 3 {
		l := acquire(t, p, cfg, Request{RequestID: "r", Affinity: "s", Explicit: true})
		l.Release()
	}
	var total int64
	for _, c := range p.counters.Accounts {
		total += c.Count
	}
	if total != 1 {
		t.Fatalf("binding hits must not count: total = %d", total)
	}
}

func TestCatalogGatesPlanModels(t *testing.T) {
	now := time.Now()
	no := false
	snaps := map[string]*quota.Snapshot{
		"plus": {CatalogAt: now, Catalog: map[string]bool{"claude-sonnet-5.5": true, "claude-opus-5": false}},
		"max":  {CatalogAt: now, Catalog: map[string]bool{"claude-sonnet-5.5": true, "claude-opus-5": true}},
		"go":   {Tier: "go", Coverage: map[string]*quota.Coverage{"claude-sonnet-5.5": {Covered: &no}}},
		"old":  {CatalogAt: now.Add(-48 * time.Hour), Catalog: map[string]bool{}},
	}
	p := New(func(id string) *quota.Snapshot { return snaps[id] })
	cfg := testConfig(config.Account{ID: "plus"}, config.Account{ID: "max"}, config.Account{ID: "go"}, config.Account{ID: "old"})
	_, ex := p.Preview(cfg, "default", "claude-opus-5")
	if ex["plus"] != "not-entitled" || ex["max"] != "" || ex["old"] != "" {
		t.Fatalf("disabled in catalogue must exclude, a stale catalogue must not: %v", ex)
	}
	_, ex = p.Preview(cfg, "default", "gpt-6-astra")
	if ex["plus"] != "not-entitled" || ex["max"] != "not-entitled" {
		t.Fatalf("absent from catalogue must exclude: %v", ex)
	}
	_, ex = p.Preview(cfg, "default", "claude-sonnet-5.5")
	if ex["go"] != "not-entitled" || ex["plus"] != "" {
		t.Fatalf("Go has no 套餐模型: %v", ex)
	}
	cfg.Routing.IgnoreCatalog = true
	if _, ex = p.Preview(cfg, "default", "gpt-6-astra"); len(ex) != 0 {
		t.Fatalf("ignoreCatalog must turn the gate off: %v", ex)
	}
}

func TestStrikesClearOnSuccess(t *testing.T) {
	p := New(nil)
	cfg := testConfig(config.Account{ID: "a"})
	if p.Strike("a", "plan") != 1 || p.Strike("a", "plan") != 2 || p.Strike("a", "package") != 1 {
		t.Fatal("strikes must count per key")
	}
	p.Succeeded(cfg, "a", "deepseek-v4.1-flash") // a Design Plan model clears plan
	if p.states["a"].Strikes["plan"] != 0 || p.states["a"].Strikes["package"] != 1 {
		t.Fatalf("strikes after a plan success = %v", p.states["a"].Strikes)
	}
}

func TestPackageClassCooldownKeepsPlanModels(t *testing.T) {
	now := time.Now()
	snaps := map[string]*quota.Snapshot{"a": {CatalogAt: now, Catalog: map[string]bool{"deepseek-v4.1-flash": true, "claude-sonnet-5.5": true, "gpt-6-sol": true}}}
	p := New(func(id string) *quota.Snapshot { return snaps[id] })
	cfg := testConfig(config.Account{ID: "a"})
	if c := p.Class(cfg, "claude-sonnet-5.5"); c != "package" {
		t.Fatalf("a catalogued model off the Design Plan list is a 套餐模型, got %s", c)
	}
	if c := p.Class(cfg, "deepseek-v4.1-flash"); c != "plan" {
		t.Fatalf("built-in Design Plan list, got %s", c)
	}
	p.ClassCooldown("a", "package", now.Add(time.Hour), 402, "x", "x")
	if _, ex := p.Preview(cfg, "default", "gpt-6-sol"); ex["a"] != "funding-cooldown" {
		t.Fatalf("every 套餐模型 must cool: %v", ex)
	}
	if _, ex := p.Preview(cfg, "default", "deepseek-v4.1-flash"); ex["a"] != "" {
		t.Fatalf("Design Plan models must stay: %v", ex)
	}
	_, err := p.Acquire(context.Background(), cfg, Request{Group: "default", Model: "gpt-6-sol"})
	var ue *UnavailableError
	if !errors.As(err, &ue) || ue.RetryAfter < 59*time.Minute {
		t.Fatalf("want retry-after ~1h, got %v", err)
	}
}
