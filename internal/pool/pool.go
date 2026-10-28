// Package pool is the runtime side of the account pool: which accounts can
// serve now, session bindings, the concurrency/RPM leases and the per-account
// states the error rules set. Everything that decides and reserves happens
// under one mutex with no I/O in between, so selection, admission, counting
// and binding are atomic within the process.
package pool

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"opendesign-balancer/internal/config"
	"opendesign-balancer/internal/quota"
	"opendesign-balancer/internal/routes"
)

// AccountState is set by error rules and cleared by the admin or by time.
type AccountState struct {
	CooldownUntil   time.Time              `json:"cooldownUntil,omitzero"`
	HardQuarantined bool                   `json:"hardQuarantined,omitempty"`
	StatusCode      int                    `json:"statusCode,omitempty"`
	Reason          string                 `json:"reason,omitempty"`
	RuleID          string                 `json:"ruleId,omitempty"`
	UpdatedAt       time.Time              `json:"updatedAt,omitzero"`
	Models          map[string]*ModelState `json:"models,omitempty"`
	// LearnedConcurrency / LearnedRPM are the membership limits the gateway
	// named in a tier_limit_exceeded refusal ("concurrency limit exceeded
	// 6/5"); they cap the account's own maxConcurrent / maxRpm until an
	// admin recovers the account.
	LearnedConcurrency int `json:"learnedConcurrency,omitempty"`
	LearnedRPM         int `json:"learnedRpm,omitempty"`
	// Classes are cooldowns of a whole funding class on the account
	// ("package": every 套餐模型), set when money ran out for one of them.
	Classes map[string]*ModelState `json:"classes,omitempty"`
	// Strikes count consecutive escalating cooldowns per key (plan,
	// package, model:<id>); a success clears them.
	Strikes map[string]int `json:"strikes,omitempty"`
}

// clone deep-copies a state for readers outside the lock.
func (st *AccountState) clone() *AccountState {
	cp := *st
	cp.Models = make(map[string]*ModelState, len(st.Models))
	for m, ms := range st.Models {
		c := *ms
		cp.Models[m] = &c
	}
	cp.Classes = make(map[string]*ModelState, len(st.Classes))
	for k, ms := range st.Classes {
		c := *ms
		cp.Classes[k] = &c
	}
	cp.Strikes = make(map[string]int, len(st.Strikes))
	for k, n := range st.Strikes {
		cp.Strikes[k] = n
	}
	return &cp
}

type ModelState struct {
	CooldownUntil time.Time `json:"cooldownUntil"`
	StatusCode    int       `json:"statusCode,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	RuleID        string    `json:"ruleId,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type Counter struct {
	Count        int64 `json:"count"`
	LastSelected int64 `json:"lastSelected"`
}

type Counters struct {
	Version  int                 `json:"version"`
	Sequence int64               `json:"sequence"`
	Accounts map[string]*Counter `json:"accounts"`
}

type rpmWindow struct {
	stamps       []time.Time
	reservations int
}

func (w *rpmWindow) prune(now time.Time) {
	cut := 0
	for cut < len(w.stamps) && now.Sub(w.stamps[cut]) >= time.Minute {
		cut++
	}
	if cut > 0 {
		w.stamps = append(w.stamps[:0], w.stamps[cut:]...)
	}
}

type binding struct {
	key        string
	accountID  string
	explicit   bool
	expiresAt  time.Time
	confirmed  bool
	generation uint64
	owner      string
}

type healthSlot struct {
	minute     int64
	succ, degr int32
}

// HealthRing is 24h of per-minute success/degrade samples.
type HealthRing [1440]healthSlot

type decay struct {
	v  float64
	at time.Time
}

func (d *decay) value(now time.Time) float64 {
	if d == nil || d.at.IsZero() {
		return 0
	}
	return d.v * math.Pow(0.5, now.Sub(d.at).Hours())
}

// MinHealthSamples: fewer samples than this read as unknown health.
const MinHealthSamples = 20

const maxBindings = 4096

type Pool struct {
	mu           sync.Mutex
	states       map[string]*AccountState
	active       map[string]int
	rpm          map[string]*rpmWindow
	bindings     map[string]*list.Element
	lru          *list.List
	gen          uint64
	counters     Counters
	health       map[string]*HealthRing
	tokens       map[string]*decay
	changed      chan struct{}
	classes      map[string]classEntry
	cursor       int64
	quota        func(id string) *quota.Snapshot // read-only: may be the poller's own snapshot
	now          func() time.Time
	dirty        bool // states changed since the last export
	samplesDirty bool // health samples changed since the last export
	cntDirty     bool
	OnRefresh    func(accountID string) // a rule asked to read quota again
}

func New(quotaFn func(string) *quota.Snapshot) *Pool {
	if quotaFn == nil {
		quotaFn = func(string) *quota.Snapshot { return nil }
	}
	return &Pool{
		states: map[string]*AccountState{}, active: map[string]int{}, rpm: map[string]*rpmWindow{},
		bindings: map[string]*list.Element{}, lru: list.New(),
		counters: Counters{Version: 1, Accounts: map[string]*Counter{}},
		health:   map[string]*HealthRing{}, tokens: map[string]*decay{},
		changed: make(chan struct{}), quota: quotaFn, now: time.Now, classes: map[string]classEntry{},
	}
}

// SetClock replaces time.Now (tests).
func (p *Pool) SetClock(now func() time.Time) { p.now = now }

func (p *Pool) broadcastLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

// ---- errors returned by Acquire ----

// UnavailableError: no account of the group can serve (503).
type UnavailableError struct {
	Reason  string         // no-accounts | all-unavailable | health-filter
	Details map[string]int // why each excluded account was excluded
	// RetryAfter is set when every account is only cooling down (for this
	// model): the soonest one is back then, so the caller can answer 429.
	RetryAfter time.Duration
}

func (e *UnavailableError) Error() string { return "no available account: " + e.Reason }

// CapacityError: accounts exist but are all at their concurrency/RPM limit (429).
type CapacityError struct {
	BlockedBy  string // concurrency | rpm | mixed | binding
	RetryAfter time.Duration
}

func (e *CapacityError) Error() string { return "all accounts busy: " + e.BlockedBy }

// ---- selection ----

type Request struct {
	Group     string
	Model     string
	RequestID string
	// Affinity is the session fingerprint; "" when the request has none.
	Affinity string
	Explicit bool
	Excluded map[string]bool
	Wait     time.Duration
}

type Decision struct {
	AccountID     string   `json:"accountId"`
	Reason        string   `json:"reason"`
	BindingResult string   `json:"bindingResult"`
	Overflow      bool     `json:"overflow"`
	Candidates    int      `json:"candidates"`
	Order         []string `json:"order,omitempty"`
}

type Lease struct {
	p           *Pool
	Account     config.Account
	Decision    Decision
	bindKey     string
	bindGen     uint64
	provisional bool
	permit      bool // an RPM permit reserved and not committed yet
	released    bool
}

// stateLocked returns the account's state, clearing expired cooldowns.
func (p *Pool) stateLocked(id string, now time.Time) *AccountState {
	st := p.states[id]
	if st == nil {
		return nil
	}
	if !st.CooldownUntil.IsZero() && !now.Before(st.CooldownUntil) {
		st.CooldownUntil = time.Time{}
		p.dirty = true
	}
	for m, ms := range st.Models {
		if !now.Before(ms.CooldownUntil) {
			delete(st.Models, m)
			p.dirty = true
		}
	}
	for c, ms := range st.Classes {
		if !now.Before(ms.CooldownUntil) {
			delete(st.Classes, c)
			p.dirty = true
		}
	}
	return st
}

type classEntry struct {
	class string
	at    time.Time
}

// classLocked says how model is paid for: "plan" (Design Plan model),
// "package" (套餐模型) or "unknown". Preflight on any account with a control
// key decides; else the built-in Design Plan list, a model only listed by
// some key's catalogue then being a 套餐模型.
func (p *Pool) classLocked(cfg *config.Config, model string, now time.Time) string {
	if model == "" {
		return "unknown"
	}
	if e, ok := p.classes[model]; ok && now.Sub(e.at) < time.Minute {
		return e.class
	}
	class := "unknown"
	inCatalog := false
	for _, a := range cfg.Accounts {
		snap := p.quota(a.ID)
		if c := snap.ModelClass(model); c != "unknown" {
			class = c
			break
		}
		if snap != nil && snap.Catalog[model] {
			inCatalog = true
		}
	}
	if class == "unknown" {
		switch {
		case quota.DefaultDesignPlanModels[model]:
			class = "plan"
		case inCatalog:
			class = "package"
		}
	}
	if len(p.classes) > 500 {
		p.classes = map[string]classEntry{}
	}
	p.classes[model] = classEntry{class, now}
	return class
}

// Class is classLocked for callers outside the pool.
func (p *Pool) Class(cfg *config.Config, model string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.classLocked(cfg, model, p.now())
}

// Strike counts one more consecutive escalating hit on key and returns it.
func (p *Pool) Strike(id, key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[id]
	if st == nil {
		st = &AccountState{}
		p.states[id] = st
	}
	if st.Strikes == nil {
		st.Strikes = map[string]int{}
	}
	st.Strikes[key]++
	p.dirty = true
	return st.Strikes[key]
}

// Succeeded clears the strikes a success on model disproves.
func (p *Pool) Succeeded(cfg *config.Config, id, model string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[id]
	if st == nil || len(st.Strikes) == 0 {
		return
	}
	delete(st.Strikes, "plan")
	delete(st.Strikes, "model:"+model)
	delete(st.Strikes, p.classLocked(cfg, model, p.now()))
	p.dirty = true
}

// ClassCooldown cools a whole funding class on the account.
func (p *Pool) ClassCooldown(id, class string, until time.Time, status int, reason, ruleID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[id]
	if st == nil {
		st = &AccountState{}
		p.states[id] = st
	}
	if st.Classes == nil {
		st.Classes = map[string]*ModelState{}
	}
	if cur := st.Classes[class]; cur == nil || until.After(cur.CooldownUntil) {
		st.Classes[class] = &ModelState{CooldownUntil: until, StatusCode: status, Reason: reason, RuleID: ruleID, UpdatedAt: p.now()}
	}
	p.dirty = true
}

// Unavailability says why a can't be scheduled for model, "" when it can.
func (p *Pool) unavailableLocked(cfg *config.Config, a config.Account, model string, now time.Time) string {
	if !a.Enabled {
		return "disabled"
	}
	if a.RuntimeKey == "" {
		return "no-key"
	}
	if st := p.stateLocked(a.ID, now); st != nil {
		if st.HardQuarantined {
			return "quarantined"
		}
		if !st.CooldownUntil.IsZero() {
			return "cooldown"
		}
		if model != "" {
			if _, ok := st.Models[model]; ok {
				return "model-cooldown"
			}
			if len(st.Classes) > 0 {
				if _, ok := st.Classes[p.classLocked(cfg, model, now)]; ok {
					return "funding-cooldown"
				}
			}
		}
	}
	stale := time.Duration(cfg.Quota.StaleSec) * time.Second
	snap := p.quota(a.ID)
	if !cfg.Routing.IgnoreCatalog && snap.NotEntitled(model, now) {
		return "not-entitled" // not on this account's plan
	}
	if a.Provider == config.ProviderOllama {
		// Ollama credits: the included allowance, then purchased credits
		if _, exhausted := snap.OllamaFunding(now, stale); exhausted {
			return "quota-exhausted"
		}
		return ""
	}
	if snap.Fresh(now, stale) {
		if _, exhausted := snap.FundingFor(now, stale, model, cfg.Routing.WalletFallback); exhausted {
			return "quota-exhausted"
		}
	}
	return ""
}

// cooldownEndLocked is when a's cooldown for model (of funding class) ends
// (zero if none).
func (p *Pool) cooldownEndLocked(id, model, class string) time.Time {
	st := p.states[id]
	if st == nil || st.HardQuarantined {
		return time.Time{} // a quarantined account is not coming back by itself
	}
	if !st.CooldownUntil.IsZero() {
		return st.CooldownUntil
	}
	if ms := st.Models[model]; ms != nil {
		return ms.CooldownUntil
	}
	if ms := st.Classes[class]; ms != nil {
		return ms.CooldownUntil
	}
	return time.Time{}
}

// limits are a's effective concurrency and RPM caps (0 = none): its own,
// lowered by what the gateway said the membership allows.
func (p *Pool) limitsLocked(a config.Account) (conc, rpm int) {
	conc, rpm = a.MaxConc, a.MaxRPM
	if st := p.states[a.ID]; st != nil {
		if l := st.LearnedConcurrency; l > 0 && (conc == 0 || l < conc) {
			conc = l
		}
		if l := st.LearnedRPM; l > 0 && (rpm == 0 || l < rpm) {
			rpm = l
		}
	}
	return conc, rpm
}

// Learn records the membership limits a refusal named (0 leaves one as is).
func (p *Pool) Learn(id string, conc, rpm int) {
	if conc <= 0 && rpm <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[id]
	if st == nil {
		st = &AccountState{}
		p.states[id] = st
	}
	if conc > 0 {
		st.LearnedConcurrency = conc
	}
	if rpm > 0 {
		st.LearnedRPM = rpm
	}
	p.dirty = true
}

// HealthRate is the success share of the last 24h; ok is false below
// MinHealthSamples.
func (p *Pool) healthLocked(id string, now time.Time) (rate float64, samples int, ok bool) {
	r := p.health[id]
	if r == nil {
		return 0, 0, false
	}
	cut := now.Unix()/60 - 1440
	var s, d int
	for _, slot := range r {
		if slot.minute > cut {
			s += int(slot.succ)
			d += int(slot.degr)
		}
	}
	n := s + d
	if n < MinHealthSamples {
		return 0, n, false
	}
	return float64(s) / float64(n), n, true
}

func (p *Pool) admitLocked(a config.Account, now time.Time) (ok bool, blockedBy string, retry time.Duration) {
	maxConc, maxRPM := p.limitsLocked(a)
	if maxConc > 0 && p.active[a.ID] >= maxConc {
		return false, "concurrency", time.Second // concurrency is checked first and spends no RPM
	}
	if maxRPM > 0 {
		w := p.rpm[a.ID]
		if w == nil {
			w = &rpmWindow{}
			p.rpm[a.ID] = w
		}
		w.prune(now)
		if len(w.stamps)+w.reservations >= maxRPM {
			retry = time.Second
			if len(w.stamps) > 0 {
				retry = max(w.stamps[0].Add(time.Minute).Sub(now), 100*time.Millisecond)
			}
			return false, "rpm", retry
		}
		w.reservations++
	}
	p.active[a.ID]++
	return true, "", 0
}

func (p *Pool) leaseLocked(a config.Account, d Decision, counted bool) *Lease {
	if counted {
		p.counters.Sequence++
		c := p.counters.Accounts[a.ID]
		if c == nil {
			c = &Counter{}
			p.counters.Accounts[a.ID] = c
		}
		c.Count++
		c.LastSelected = p.counters.Sequence
		p.cntDirty = true
	}
	_, rpm := p.limitsLocked(a)
	return &Lease{p: p, Account: a, Decision: d, permit: rpm > 0}
}

func (p *Pool) getBindingLocked(key string, now time.Time) *binding {
	el := p.bindings[key]
	if el == nil {
		return nil
	}
	b := el.Value.(*binding)
	if !now.Before(b.expiresAt) {
		p.lru.Remove(el)
		delete(p.bindings, key)
		return nil
	}
	return b
}

func (p *Pool) deleteBindingLocked(key string) {
	if el := p.bindings[key]; el != nil {
		p.lru.Remove(el)
		delete(p.bindings, key)
	}
}

func ttl(cfg *config.Config, explicit bool) time.Duration {
	if explicit {
		return time.Duration(cfg.Routing.ExplicitTTLSec) * time.Second
	}
	return time.Duration(cfg.Routing.FallbackTTLSec) * time.Second
}

func (p *Pool) putProvisionalLocked(cfg *config.Config, req Request, accountID string, now time.Time) (uint64, bool) {
	p.deleteBindingLocked(req.Affinity)
	p.gen++
	b := &binding{key: req.Affinity, accountID: accountID, explicit: req.Explicit,
		expiresAt: now.Add(ttl(cfg, req.Explicit)), generation: p.gen, owner: req.RequestID}
	p.bindings[req.Affinity] = p.lru.PushFront(b)
	for p.lru.Len() > maxBindings {
		old := p.lru.Back()
		p.lru.Remove(old)
		delete(p.bindings, old.Value.(*binding).key)
	}
	return p.gen, true
}

// Bind sets a confirmed binding for key, e.g. a Responses response id the
// account answered, so a follow-up naming it as previous_response_id lands
// on the same account.
func (p *Pool) Bind(cfg *config.Config, key, accountID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	p.deleteBindingLocked(key)
	p.gen++
	b := &binding{key: key, accountID: accountID, explicit: true, expiresAt: now.Add(ttl(cfg, true)), confirmed: true, generation: p.gen}
	p.bindings[key] = p.lru.PushFront(b)
	for p.lru.Len() > maxBindings {
		old := p.lru.Back()
		p.lru.Remove(old)
		delete(p.bindings, old.Value.(*binding).key)
	}
}

type candidate struct {
	a    config.Account
	seat routes.Seat
}

func (p *Pool) seatLocked(cfg *config.Config, a config.Account, model string, now time.Time) routes.Seat {
	s := routes.Seat{ID: a.ID, Priority: a.Priority, Weight: a.Weight, Active: p.active[a.ID], Tokens: p.tokens[a.ID].value(now)}
	if c := p.counters.Accounts[a.ID]; c != nil {
		s.Count, s.LastSelected = c.Count, c.LastSelected
	}
	stale := time.Duration(cfg.Quota.StaleSec) * time.Second
	snap := p.quota(a.ID)
	if a.Provider == config.ProviderOllama {
		// the balance is the pace signal: included allowance first, purchased
		// credits only fill in behind it
		if snap.FreshOllama(now, stale) {
			s.Known = true
			switch src, _ := snap.OllamaFunding(now, stale); src {
			case "included":
				s.Used, s.Soon = snap.OllamaUsedShare(now), snap.OllamaRenewals(now)
			case "purchased":
				s.Fallback, s.Used = 2, 100
			default:
				s.Known = false
			}
		}
		return s
	}
	if snap.Fresh(now, stale) {
		s.Known = true
		switch src, _ := snap.FundingFor(now, stale, model, cfg.Routing.WalletFallback); src {
		case "plan":
			s.Used = snap.UsedShare(now)
			s.Soon = snap.Renewals()
			s.Pace = snap.Pace(now)
		case "subscription":
			// 套餐模型额度 doesn't carry over: the period's end is its reset
			s.Fallback, s.Used = 1, 100
			if snap.Summary != nil && !snap.Summary.PeriodEnd.IsZero() {
				s.Soon = []time.Time{snap.Summary.PeriodEnd}
			}
		case "wallet":
			s.Fallback, s.Used = 2, 100
		default:
			s.Known = false
		}
	}
	return s
}

// tryLocked is one selection attempt. forceOverflow skips waiting on a busy
// bound account (wait-overflow after its budget).
func (p *Pool) tryLocked(cfg *config.Config, req Request, forceOverflow bool) (*Lease, error) {
	now := p.now()
	var cands []candidate
	details := map[string]int{}
	inGroup := 0
	for _, a := range cfg.Accounts {
		if a.Group != req.Group {
			continue
		}
		inGroup++
		if req.Excluded[a.ID] {
			details["excluded"]++
			continue
		}
		if why := p.unavailableLocked(cfg, a, req.Model, now); why != "" {
			details[why]++
			continue
		}
		cands = append(cands, candidate{a: a})
	}
	if inGroup == 0 {
		return nil, &UnavailableError{Reason: "no-accounts", Details: details}
	}
	if len(cands) == 0 {
		ue := &UnavailableError{Reason: "all-unavailable", Details: details}
		// some are only cooling down: say when the soonest is back
		if details["cooldown"]+details["model-cooldown"]+details["funding-cooldown"] > 0 {
			var soonest time.Time
			for _, a := range cfg.Accounts {
				if a.Group != req.Group || req.Excluded[a.ID] {
					continue
				}
				if t := p.cooldownEndLocked(a.ID, req.Model, p.classLocked(cfg, req.Model, now)); !t.IsZero() && (soonest.IsZero() || t.Before(soonest)) {
					soonest = t
				}
			}
			if !soonest.IsZero() {
				ue.RetryAfter = max(soonest.Sub(now), time.Second)
			}
		}
		return nil, ue
	}
	if cfg.Routing.HealthFilter {
		kept := cands[:0]
		for _, c := range cands {
			// unknown health counts as healthy (optimistic); at the threshold is allowed
			if rate, _, ok := p.healthLocked(c.a.ID, now); !ok || rate >= cfg.Routing.MinimumHealth {
				kept = append(kept, c)
			} else {
				details["health"]++
			}
		}
		if len(kept) == 0 {
			return nil, &UnavailableError{Reason: "health-filter", Details: details}
		}
		cands = kept
	}

	d := Decision{BindingResult: "not-applicable", Candidates: len(cands)}
	sticky := cfg.Routing.Sticky && req.Affinity != ""
	overflow := false
	blocked := map[string]bool{}
	var retry time.Duration
	if sticky {
		d.BindingResult = "miss"
		if b := p.getBindingLocked(req.Affinity, now); b != nil {
			idx := -1
			for i, c := range cands {
				if c.a.ID == b.accountID {
					idx = i
				}
			}
			if idx < 0 {
				// the bound account left the schedulable set (or failed this request)
				p.deleteBindingLocked(req.Affinity)
				d.BindingResult = "invalidated"
			} else {
				a := cands[idx].a
				ok, by, ra := p.admitLocked(a, now)
				if ok {
					b.expiresAt = now.Add(ttl(cfg, b.explicit)) // sliding
					p.lru.MoveToFront(p.bindings[req.Affinity])
					d.AccountID, d.Reason = a.ID, "binding"
					d.BindingResult = "hit"
					if !b.confirmed {
						d.BindingResult = "provisional"
					}
					return p.leaseLocked(a, d, false), nil
				}
				switch {
				case cfg.Routing.OnBindingBusy == "reject":
					return nil, &CapacityError{BlockedBy: by, RetryAfter: ra}
				case cfg.Routing.OnBindingBusy == "wait-overflow" && !forceOverflow:
					return nil, &CapacityError{BlockedBy: "binding", RetryAfter: ra}
				}
				overflow = true
				d.BindingResult = "temporary-overflow"
				blocked[by] = true
				retry = ra
				cands = append(cands[:idx:idx], cands[idx+1:]...)
			}
		}
	}

	seats := make([]routes.Seat, len(cands))
	for i, c := range cands {
		seats[i] = p.seatLocked(cfg, c.a, req.Model, now)
	}
	p.cursor++
	order := routes.Order(cfg.Routing.Strategy, req.Group, seats, routes.Params{
		LowShare: cfg.Routing.LowShare, SpentShare: cfg.Routing.SpentShare, Cursor: p.cursor,
	})
	for _, i := range order {
		d.Order = append(d.Order, cands[i].a.ID)
		if len(d.Order) >= 8 {
			break
		}
	}
	for _, i := range order {
		a := cands[i].a
		ok, by, ra := p.admitLocked(a, now)
		if !ok {
			blocked[by] = true
			if retry == 0 || ra < retry {
				retry = ra
			}
			continue
		}
		d.AccountID, d.Overflow = a.ID, overflow
		d.Reason = "strategy:" + cfg.Routing.Strategy
		if overflow {
			d.Reason = "overflow"
		}
		l := p.leaseLocked(a, d, true)
		if sticky && !overflow {
			l.bindGen, l.provisional = p.putProvisionalLocked(cfg, req, a.ID, now)
			l.bindKey = req.Affinity
		}
		return l, nil
	}
	by := "mixed"
	if len(blocked) == 1 {
		for k := range blocked {
			by = k
		}
	}
	if retry <= 0 {
		retry = time.Second
	}
	return nil, &CapacityError{BlockedBy: by, RetryAfter: retry}
}

// Acquire selects and leases an account, waiting up to req.Wait for
// capacity. It returns *UnavailableError or *CapacityError when it can't.
func (p *Pool) Acquire(ctx context.Context, cfg *config.Config, req Request) (*Lease, error) {
	deadline := time.Now().Add(req.Wait)
	forceOverflow := false
	for {
		p.mu.Lock()
		l, err := p.tryLocked(cfg, req, forceOverflow)
		ch := p.changed
		p.mu.Unlock()
		var ce *CapacityError
		if err == nil || !errors.As(err, &ce) {
			return l, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			if ce.BlockedBy == "binding" && !forceOverflow {
				forceOverflow = true // its wait is over: overflow with no further wait
				continue
			}
			if ce.BlockedBy == "binding" {
				ce.BlockedBy = "concurrency"
			}
			return nil, ce
		}
		wait := min(left, max(ce.RetryAfter, 50*time.Millisecond))
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-ch:
		case <-t.C:
		}
		t.Stop()
	}
}

// Preview ranks the candidates without reserving anything.
func (p *Pool) Preview(cfg *config.Config, group, model string) (cands []string, excluded map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	excluded = map[string]string{}
	var accs []config.Account
	for _, a := range cfg.Accounts {
		if a.Group != group {
			continue
		}
		if why := p.unavailableLocked(cfg, a, model, now); why != "" {
			excluded[a.ID] = why
			continue
		}
		accs = append(accs, a)
	}
	seats := make([]routes.Seat, len(accs))
	for i, a := range accs {
		seats[i] = p.seatLocked(cfg, a, model, now)
	}
	// a copy of the round so preview doesn't advance weighted rotation
	strategy := cfg.Routing.Strategy
	if strategy == "weighted" {
		strategy = "ordered"
	}
	for _, i := range routes.Order(strategy, "preview/"+group, seats, routes.Params{
		LowShare: cfg.Routing.LowShare, SpentShare: cfg.Routing.SpentShare, Cursor: p.cursor + 1,
	}) {
		cands = append(cands, accs[i].ID)
	}
	return cands, excluded
}

// MarkSent is called once the request has been written upstream: the RPM
// permit is spent for good and a provisional binding becomes confirmed.
func (l *Lease) MarkSent() {
	p := l.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if l.permit {
		l.permit = false
		w := p.rpm[l.Account.ID]
		if w != nil {
			w.reservations--
			w.stamps = append(w.stamps, p.now())
		}
	}
	if l.provisional {
		l.provisional = false
		if el := p.bindings[l.bindKey]; el != nil {
			if b := el.Value.(*binding); b.generation == l.bindGen {
				b.confirmed = true
			}
		}
	}
}

// Release frees the lease once; a permit never spent is given back and a
// binding this request created but never confirmed is removed.
func (l *Lease) Release() {
	p := l.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	if p.active[l.Account.ID] > 0 {
		p.active[l.Account.ID]--
	}
	if l.permit {
		l.permit = false
		if w := p.rpm[l.Account.ID]; w != nil && w.reservations > 0 {
			w.reservations--
		}
	}
	if l.provisional {
		l.provisional = false
		if el := p.bindings[l.bindKey]; el != nil {
			if b := el.Value.(*binding); b.generation == l.bindGen && b.owner != "" && !b.confirmed {
				p.deleteBindingLocked(l.bindKey)
			}
		}
	}
	p.broadcastLocked()
}

// ---- outcomes ----

// Sample records one attempt's health sample: success or degrade.
func (p *Pool) Sample(id string, success bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.health[id]
	if r == nil {
		r = &HealthRing{}
		p.health[id] = r
	}
	m := p.now().Unix() / 60
	slot := &r[m%1440]
	if slot.minute != m {
		*slot = healthSlot{minute: m}
	}
	if success {
		slot.succ++
	} else {
		slot.degr++
	}
	p.samplesDirty = true
}

func (p *Pool) AddTokens(id string, n int64) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	d := p.tokens[id]
	if d == nil {
		d = &decay{}
		p.tokens[id] = d
	}
	d.v = d.value(now) + float64(n)
	d.at = now
}

// Cooldown sets an account (model == "") or account×model cooldown.
func (p *Pool) Cooldown(id, model string, until time.Time, status int, reason, ruleID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	st := p.states[id]
	if st == nil {
		st = &AccountState{}
		p.states[id] = st
	}
	if model == "" {
		if until.After(st.CooldownUntil) {
			st.CooldownUntil = until
		}
		st.StatusCode, st.Reason, st.RuleID, st.UpdatedAt = status, reason, ruleID, now
	} else {
		if st.Models == nil {
			st.Models = map[string]*ModelState{}
		}
		ms := st.Models[model]
		if ms == nil || until.After(ms.CooldownUntil) {
			st.Models[model] = &ModelState{CooldownUntil: until, StatusCode: status, Reason: reason, RuleID: ruleID, UpdatedAt: now}
		}
	}
	p.dirty = true
}

func (p *Pool) Quarantine(id string, status int, reason, ruleID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.states[id]
	if st == nil {
		st = &AccountState{}
		p.states[id] = st
	}
	st.HardQuarantined = true
	st.StatusCode, st.Reason, st.RuleID, st.UpdatedAt = status, reason, ruleID, p.now()
	p.dirty = true
}

// Recover clears cooldowns and quarantine.
func (p *Pool) Recover(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.states, id)
	p.dirty = true
	p.broadcastLocked()
}

func (p *Pool) ResetHealth(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.health, id)
	p.dirty = true
}

// Invalidate drops what depends on an account's key or proxy: its
// bindings and RPM window (and its states when full).
func (p *Pool) Invalidate(id string, full bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, el := range p.bindings {
		if el.Value.(*binding).accountID == id {
			p.lru.Remove(el)
			delete(p.bindings, key)
		}
	}
	delete(p.rpm, id)
	if full {
		delete(p.states, id)
		delete(p.health, id)
		delete(p.tokens, id)
		if _, ok := p.counters.Accounts[id]; ok {
			delete(p.counters.Accounts, id)
			p.cntDirty = true
		}
	}
	p.dirty = true
	p.broadcastLocked()
}

func (p *Pool) ResetCounts() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counters = Counters{Version: 1, Accounts: map[string]*Counter{}}
	p.cntDirty = true
}

// ---- views ----

type View struct {
	State      *AccountState `json:"state,omitempty"`
	Active     int           `json:"active"`
	RPM        int           `json:"rpm"`
	Health     *float64      `json:"health"`
	Samples    int           `json:"samples"`
	Selections int64         `json:"selections"`
	Bindings   int           `json:"bindings"`
	Status     string        `json:"status"` // ok | disabled | cooldown | quarantined | quota-exhausted | …
}

func (p *Pool) Views(cfg *config.Config) map[string]View {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	perAccount := map[string]int{}
	for _, el := range p.bindings {
		b := el.Value.(*binding)
		if now.Before(b.expiresAt) {
			perAccount[b.accountID]++
		}
	}
	out := map[string]View{}
	for _, a := range cfg.Accounts {
		v := View{Active: p.active[a.ID], Bindings: perAccount[a.ID]}
		if st := p.stateLocked(a.ID, now); st != nil {
			v.State = st.clone()
		}
		if w := p.rpm[a.ID]; w != nil {
			w.prune(now)
			v.RPM = len(w.stamps)
		}
		if rate, n, ok := p.healthLocked(a.ID, now); ok {
			v.Health = &rate
			v.Samples = n
		} else {
			v.Samples = n
		}
		if c := p.counters.Accounts[a.ID]; c != nil {
			v.Selections = c.Count
		}
		v.Status = p.unavailableLocked(cfg, a, "", now)
		if v.Status == "" {
			v.Status = "ok"
		}
		out[a.ID] = v
	}
	return out
}

func (p *Pool) BindingCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.bindings)
}

// ---- persistence ----

type HealthExport map[string]map[int64][2]int32

type Export struct {
	States map[string]*AccountState `json:"accountStates"`
	Health HealthExport             `json:"health"`
}

// Dirty says, without clearing anything, whether the states or the health
// samples changed since the last ExportState.
func (p *Pool) Dirty() (states, samples bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dirty, p.samplesDirty
}

// ExportState returns the persisted part and whether it changed since the
// last call.
func (p *Pool) ExportState() (Export, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := p.dirty || p.samplesDirty
	p.dirty, p.samplesDirty = false, false
	e := Export{States: map[string]*AccountState{}, Health: HealthExport{}}
	for id, st := range p.states {
		e.States[id] = st.clone()
	}
	cut := p.now().Unix()/60 - 1440
	for id, r := range p.health {
		m := map[int64][2]int32{}
		for _, s := range r {
			if s.minute > cut && s.succ+s.degr > 0 {
				m[s.minute] = [2]int32{s.succ, s.degr}
			}
		}
		if len(m) > 0 {
			e.Health[id] = m
		}
	}
	return e, changed
}

func (p *Pool) ImportState(e Export, valid map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, st := range e.States {
		if valid[id] && st != nil {
			p.states[id] = st
		}
	}
	for id, m := range e.Health {
		if !valid[id] {
			continue
		}
		r := &HealthRing{}
		for minute, v := range m {
			r[minute%1440] = healthSlot{minute: minute, succ: v[0], degr: v[1]}
		}
		p.health[id] = r
	}
}

func (p *Pool) ExportCounters(force bool) (Counters, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.cntDirty && !force {
		return Counters{}, false
	}
	p.cntDirty = false
	c := Counters{Version: 1, Sequence: p.counters.Sequence, Accounts: map[string]*Counter{}}
	for id, v := range p.counters.Accounts {
		cp := *v
		c.Accounts[id] = &cp
	}
	return c, true
}

// ImportCounters validates and loads a counters snapshot; an invalid one is
// a startup error.
func (p *Pool) ImportCounters(c Counters, valid map[string]bool) error {
	if c.Version != 1 || c.Sequence < 0 || c.Accounts == nil {
		return fmt.Errorf("invalid selection counters")
	}
	out := Counters{Version: 1, Sequence: c.Sequence, Accounts: map[string]*Counter{}}
	for id, v := range c.Accounts {
		if v == nil || v.Count < 0 || v.LastSelected < 0 || v.LastSelected > c.Sequence {
			return fmt.Errorf("invalid selection counter for %s", id)
		}
		if valid[id] {
			cp := *v
			out.Accounts[id] = &cp
		}
	}
	p.mu.Lock()
	p.counters = out
	p.mu.Unlock()
	return nil
}

// SortedIDs is a helper for stable output.
func SortedIDs[M ~map[string]V, V any](m M) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
