package quota

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"opendesign-balancer/internal/config"
)

// Snapshot is what was last read for one account. Each part keeps its own
// timestamp: a failed part keeps its previous value.
type Snapshot struct {
	WorkspaceID       string    `json:"workspaceId,omitempty"`
	WorkspaceMemberID string    `json:"workspaceMemberId,omitempty"`
	WorkspaceName     string    `json:"workspaceName,omitempty"`
	WorkspaceType     string    `json:"workspaceType,omitempty"`
	MembershipTier    string    `json:"membershipTier,omitempty"`
	WorkspaceAt       time.Time `json:"workspaceAt,omitzero"`

	PlanAt              time.Time `json:"planAt,omitzero"`
	Eligible            bool      `json:"eligible"`
	Tier                string    `json:"tier,omitempty"`
	Windows             []Window  `json:"windows"`
	Funding             string    `json:"funding,omitempty"`
	ModelCovered        *bool     `json:"modelCovered,omitempty"`
	PreflightBalanceUSD string    `json:"preflightBalanceUsd,omitempty"`

	WalletUSD    string    `json:"walletUsd,omitempty"`
	WalletSource string    `json:"walletSource,omitempty"` // wallet | preflight
	WalletAt     time.Time `json:"walletAt,omitzero"`

	Summary   *Summary  `json:"summary,omitempty"`
	SummaryAt time.Time `json:"summaryAt,omitzero"`

	// Coverage is preflight?modelId= per model: whether the model is a
	// Design Plan model on this account's tier (covered) or a 套餐模型 paid
	// from subscription credits / wallet (not covered).
	Coverage map[string]*Coverage `json:"coverage,omitempty"`

	// Catalog is the runtime key's own /v1/models: model id → enabled.
	// Which models a plan may use is read here, not from the pricing page,
	// whose display names don't match the API ids.
	Catalog      map[string]bool `json:"catalog,omitempty"`
	CatalogAt    time.Time       `json:"catalogAt,omitzero"`
	CatalogError string          `json:"catalogError,omitempty"`

	AuthFailed  bool      `json:"authFailed"`
	LastError   string    `json:"lastError,omitempty"`
	LastErrorAt time.Time `json:"lastErrorAt,omitzero"`
	LastOKAt    time.Time `json:"lastOkAt,omitzero"`
	Failures    int       `json:"failures"`
	NextAt      time.Time `json:"nextAt,omitzero"`
}

// Coverage is one model's preflight answer.
type Coverage struct {
	Covered *bool     `json:"covered"`
	Funding string    `json:"funding"`
	At      time.Time `json:"at"`
}

const (
	coverageTTL  = 6 * time.Hour
	catalogTTL   = 10 * time.Minute
	catalogStale = 24 * time.Hour
)

// DefaultDesignPlanModels are the Design Plan models as of 2026-10-23: the
// pricing page's list, and exactly what a Go-tier key's /v1/models returns.
// They classify a model only while no account with a control key has asked
// preflight about it.
var DefaultDesignPlanModels = map[string]bool{
	"claude-haiku-5.5": true, "deepseek-v4-flash": true, "deepseek-v4-flash-vision-exp": true,
	"deepseek-v4-pro": true, "deepseek-v4.1-flash": true, "glm-5.3-flash": true, "glm-5.3-flashx": true,
	"gpt-6-luna": true, "hy4-preview": true, "kimi-k2.7-code": true, "mimo-v2.6-flash": true, "mimo-v2.6-pro": true,
}

// NotEntitled: there is evidence the account's plan can't call model —
// its catalogue lacks it or marks it disabled, or it is a 套餐模型 on Go,
// which has no 套餐模型 at all.
func (s *Snapshot) NotEntitled(model string, now time.Time) bool {
	if s == nil || model == "" {
		return false
	}
	if len(s.Catalog) > 0 && now.Sub(s.CatalogAt) < catalogStale {
		if enabled, ok := s.Catalog[model]; !ok || !enabled {
			return true
		}
	}
	tier := s.Tier
	if tier == "" {
		tier = s.MembershipTier
	}
	return strings.EqualFold(tier, "go") && s.ModelClass(model) == "package"
}

// Models are the catalogue's enabled models (nil while unknown or stale).
func (s *Snapshot) Models(now time.Time) []string {
	if s == nil || len(s.Catalog) == 0 || now.Sub(s.CatalogAt) >= catalogStale {
		return nil
	}
	out := make([]string, 0, len(s.Catalog))
	for m, en := range s.Catalog {
		if en {
			out = append(out, m)
		}
	}
	return out
}

func (s *Snapshot) clone() *Snapshot {
	if s == nil {
		return nil
	}
	c := *s
	c.Windows = append([]Window(nil), s.Windows...)
	if s.Catalog != nil {
		c.Catalog = make(map[string]bool, len(s.Catalog))
		for m, v := range s.Catalog {
			c.Catalog[m] = v
		}
	}
	if s.Coverage != nil {
		c.Coverage = make(map[string]*Coverage, len(s.Coverage))
		for m, v := range s.Coverage {
			cv := *v
			c.Coverage[m] = &cv
		}
	}
	if s.Summary != nil {
		sum := *s.Summary
		c.Summary = &sum
	}
	if s.ModelCovered != nil {
		v := *s.ModelCovered
		c.ModelCovered = &v
	}
	return &c
}

// Fresh: the plan windows were read within stale and the control key works.
func (s *Snapshot) Fresh(now time.Time, stale time.Duration) bool {
	return s != nil && !s.AuthFailed && !s.PlanAt.IsZero() && now.Sub(s.PlanAt) < stale
}

// EffectiveRemaining is a window's remaining credits, its whole limit once
// its reset has gone by.
func (w Window) EffectiveRemaining(now time.Time) int64 {
	if !w.ResetsAt.IsZero() && !now.Before(w.ResetsAt) {
		return w.Limit
	}
	return w.Remaining
}

// UsedShare is the share (0–100) of the fullest window used.
func (s *Snapshot) UsedShare(now time.Time) float64 {
	share := 0.0
	for _, w := range s.Windows {
		if w.Limit <= 0 {
			share = 100
			continue
		}
		used := float64(w.Limit-w.EffectiveRemaining(now)) / float64(w.Limit) * 100
		share = max(share, min(max(used, 0), 100))
	}
	return share
}

// PlanRoom: a Design Plan is active and every window has credits left.
func (s *Snapshot) PlanRoom(now time.Time) bool {
	if !s.Eligible || len(s.Windows) == 0 {
		return false
	}
	for _, w := range s.Windows {
		if w.EffectiveRemaining(now) <= 0 {
			return false
		}
	}
	return true
}

// WalletPositive: the wallet is above $0.
func (s *Snapshot) WalletPositive() bool {
	v, ok := ParseUSD(s.WalletUSD)
	return ok && v > 0
}

// ModelClass is "plan" for a Design Plan model, "package" for a 套餐模型
// (never drawn from the plan windows), "unknown" before preflight has said.
func (s *Snapshot) ModelClass(model string) string {
	if s == nil || model == "" {
		return "unknown"
	}
	c := s.Coverage[model]
	if c == nil || c.Covered == nil {
		return "unknown"
	}
	if *c.Covered {
		return "plan"
	}
	return "package"
}

func positiveCredits(v string) bool {
	f, ok := ParseUSD(v)
	return ok && f > 0
}

// SubscriptionCredits: 套餐模型额度 left, and whether the summary saying so
// is fresh.
func (s *Snapshot) SubscriptionCredits(now time.Time, stale time.Duration) (positive, known bool) {
	if s.Summary == nil || now.Sub(s.SummaryAt) >= stale {
		return false, false
	}
	return positiveCredits(s.Summary.SubscriptionCredits) || positiveCredits(s.Summary.SubscriptionCreditsUSD), true
}

// PaidPositive: money that costs real payment is left (wallet or recharge credits).
func (s *Snapshot) PaidPositive() bool {
	if s.WalletPositive() {
		return true
	}
	return s.Summary != nil && (positiveCredits(s.Summary.RechargeCredits) || positiveCredits(s.Summary.RechargeCreditsUSD))
}

// FundingFor says what would pay for model now, in the official order: Design
// Plan windows (Design Plan models only), then 套餐模型额度, then wallet /
// recharge. source is plan | subscription | wallet | unknown. exhausted is
// true only with positive evidence that nothing pays (or only the wallet,
// when allowWallet is off): a zero wallet alone never proves it.
func (s *Snapshot) FundingFor(now time.Time, stale time.Duration, model string, allowWallet bool) (source string, exhausted bool) {
	if s.ModelClass(model) != "package" && s.PlanRoom(now) {
		return "plan", false
	}
	sub, known := s.SubscriptionCredits(now, stale)
	if sub {
		return "subscription", false
	}
	if s.PaidPositive() {
		return "wallet", !allowWallet
	}
	if !known {
		return "unknown", false
	}
	return "", true
}

// ExhaustedUntil is when a used-up plan comes back: the latest reset among
// the windows with nothing left (zero when unknown).
func (s *Snapshot) ExhaustedUntil(now time.Time) time.Time {
	var t time.Time
	for _, w := range s.Windows {
		if w.EffectiveRemaining(now) <= 0 && w.ResetsAt.After(t) {
			t = w.ResetsAt
		}
	}
	return t
}

// Renewals are the windows' resets, the longest window first.
func (s *Snapshot) Renewals() []time.Time {
	out := make([]time.Time, 0, len(s.Windows))
	for _, w := range s.Windows {
		out = append(out, w.ResetsAt)
	}
	return out
}

// Pace is the longest window's share left per hour until it renews.
func (s *Snapshot) Pace(now time.Time) float64 {
	if len(s.Windows) == 0 || s.Windows[0].Limit <= 0 {
		return 0
	}
	w := s.Windows[0]
	left := float64(w.EffectiveRemaining(now)) / float64(w.Limit) * 100
	hours := max(w.ResetsAt.Sub(now).Hours(), 1)
	return left / hours
}

type Poller struct {
	Cfg      func() *config.Config
	HTTPFor  func(proxyURL string) *http.Client
	OnUpdate func(accountID string)

	mu      sync.Mutex
	snaps   map[string]*Snapshot
	running map[string]bool
	models  map[string]time.Time // models requested lately → last seen
	wake    chan struct{}
}

func NewPoller(cfg func() *config.Config, httpFor func(string) *http.Client) *Poller {
	return &Poller{Cfg: cfg, HTTPFor: httpFor, snaps: map[string]*Snapshot{}, running: map[string]bool{},
		models: map[string]time.Time{}, wake: make(chan struct{}, 1)}
}

// NoteModel records a requested model so each account's coverage of it is
// read. A model seen for the first time is read on every account soon.
func (p *Poller) NoteModel(model string) {
	if model == "" || len(model) > 100 {
		return
	}
	p.mu.Lock()
	_, seen := p.models[model]
	p.models[model] = time.Now()
	if len(p.models) > 200 {
		oldest, at := "", time.Now()
		for m, t := range p.models {
			if t.Before(at) {
				oldest, at = m, t
			}
		}
		delete(p.models, oldest)
	}
	if !seen {
		for id, s := range p.snaps {
			if s.ModelClass(model) == "unknown" {
				p.rescheduleLocked(id, s)
			}
		}
	}
	p.mu.Unlock()
	if !seen {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

// coverageDue lists up to n recent models whose coverage s lacks or has stale.
func (p *Poller) coverageDue(s *Snapshot, n int, now time.Time) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var missing, stale []string
	for m, seen := range p.models {
		if now.Sub(seen) > 24*time.Hour {
			continue
		}
		c := s.Coverage[m]
		switch {
		case c == nil:
			missing = append(missing, m)
		case now.Sub(c.At) > coverageTTL:
			stale = append(stale, m)
		}
	}
	out := append(missing, stale...)
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func (p *Poller) Load(m map[string]*Snapshot) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, s := range m {
		if s != nil {
			s.NextAt = time.Time{} // read again soon after a restart
			p.snaps[id] = s
		}
	}
}

func (p *Poller) Get(id string) *Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snaps[id].clone()
}

// View returns the stored snapshot itself, without a copy, for hot read-only
// paths such as account selection. A stored snapshot is never modified in
// place (every change stores a new one), so it stays valid; callers must not
// modify it.
func (p *Poller) View(id string) *Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snaps[id]
}

// rescheduleLocked stores a copy of s due at the next tick (caller holds p.mu).
func (p *Poller) rescheduleLocked(id string, s *Snapshot) {
	c := *s
	c.NextAt = time.Time{}
	p.snaps[id] = &c
}

func (p *Poller) All() map[string]*Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]*Snapshot, len(p.snaps))
	for id, s := range p.snaps {
		out[id] = s.clone()
	}
	return out
}

// Forget drops an account's snapshot (deleted, or its control key changed).
func (p *Poller) Forget(id string) {
	p.mu.Lock()
	delete(p.snaps, id)
	p.mu.Unlock()
}

// Refresh asks for id to be read at the next tick, unless it was read in
// the last 10 seconds.
func (p *Poller) Refresh(id string) {
	p.mu.Lock()
	s := p.snaps[id]
	if s == nil {
		s = &Snapshot{}
		p.snaps[id] = s
	}
	if time.Since(s.LastOKAt) > 10*time.Second {
		p.rescheduleLocked(id, s)
	}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *Poller) Run(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.wake:
		}
		p.dispatch(ctx)
	}
}

func (p *Poller) dispatch(ctx context.Context) {
	cfg := p.Cfg()
	if !cfg.Quota.Enabled {
		return
	}
	now := time.Now()
	p.mu.Lock()
	inFlight := 0
	for _, r := range p.running {
		if r {
			inFlight++
		}
	}
	var due []config.Account
	for _, a := range cfg.Accounts {
		if a.RuntimeKey == "" || p.running[a.ID] {
			continue
		}
		s := p.snaps[a.ID]
		if s == nil || !now.Before(s.NextAt) {
			due = append(due, a)
		}
	}
	for _, a := range due {
		if inFlight >= cfg.Quota.Concurrency {
			break
		}
		inFlight++
		p.running[a.ID] = true
		go func(a config.Account) {
			c, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			_ = p.fetch(c, cfg, a)
			p.mu.Lock()
			delete(p.running, a.ID)
			p.mu.Unlock()
		}(a)
	}
	p.mu.Unlock()
}

// RefreshNow reads one account synchronously (admin button).
func (p *Poller) RefreshNow(ctx context.Context, a config.Account) error {
	if a.ControlKey == "" {
		// only the catalogue can be read
		p.mu.Lock()
		if s := p.snaps[a.ID]; s != nil {
			c := *s
			c.CatalogAt = time.Time{}
			p.snaps[a.ID] = &c
		}
		p.mu.Unlock()
	}
	return p.fetch(ctx, p.Cfg(), a)
}

func backoff(failures int) time.Duration {
	d := time.Minute
	for i := 1; i < failures && d < 15*time.Minute; i++ {
		d *= 2
	}
	return min(d, 15*time.Minute)
}

func (p *Poller) fetch(ctx context.Context, cfg *config.Config, a config.Account) error {
	client := &Client{Base: cfg.Upstream.ControlBase, HTTP: p.HTTPFor(a.ProxyURL)}
	p.mu.Lock()
	prev := p.snaps[a.ID].clone()
	p.mu.Unlock()
	s := prev
	if s == nil {
		s = &Snapshot{}
	}
	now := time.Now()
	fail := func(err error) error {
		s.LastError, s.LastErrorAt = err.Error(), now
		s.AuthFailed = IsAuth(err)
		s.Failures++
		s.NextAt = now.Add(backoff(s.Failures))
		p.store(a.ID, s)
		return err
	}

	if a.RuntimeKey != "" && now.Sub(s.CatalogAt) >= catalogTTL {
		if cat, err := FetchCatalog(ctx, p.HTTPFor(a.ProxyURL), cfg.Upstream.RuntimeBase, a.RuntimeKey); err == nil {
			s.Catalog, s.CatalogAt, s.CatalogError = cat, now, ""
		} else {
			s.CatalogError = err.Error() // the last good catalogue stays
		}
	}
	if a.ControlKey == "" {
		s.NextAt = now.Add(max(time.Duration(cfg.Quota.IntervalSec)*time.Second, catalogTTL))
		p.store(a.ID, s)
		return nil
	}

	ws := s.WorkspaceID
	if ws == "" || now.Sub(s.WorkspaceAt) > 10*time.Minute {
		w, err := client.Workspace(ctx, a.ControlKey)
		if err != nil {
			return fail(err)
		}
		if s.WorkspaceID != "" && s.WorkspaceID != w.WorkspaceID() {
			// a different workspace: what was read for the old one is void
			s = &Snapshot{Catalog: s.Catalog, CatalogAt: s.CatalogAt, CatalogError: s.CatalogError}
		}
		s.WorkspaceID, s.WorkspaceMemberID = w.WorkspaceID(), w.MemberID
		s.WorkspaceName, s.WorkspaceType, s.MembershipTier = w.Name, w.Type, w.PersonalMembershipTier
		s.WorkspaceAt = now
		ws = s.WorkspaceID
	}

	pf, err := client.Preflight(ctx, a.ControlKey, ws, "")
	if err != nil {
		return fail(err)
	}
	s.PlanAt, s.Eligible, s.Tier, s.Windows = now, pf.Eligible, pf.Tier, pf.Windows
	s.Funding, s.ModelCovered, s.PreflightBalanceUSD = pf.Funding, pf.ModelCovered, pf.BalanceUSD
	s.AuthFailed = false

	var partErr error
	if w, err := client.Wallet(ctx, a.ControlKey, ws); err == nil {
		s.WalletUSD, s.WalletSource, s.WalletAt = w.BalanceUSD, "wallet", now
	} else {
		partErr = err
		if _, ok := ParseUSD(pf.BalanceUSD); ok {
			s.WalletUSD, s.WalletSource, s.WalletAt = pf.BalanceUSD, "preflight", now
		}
	}
	if sum, err := client.Summary(ctx, a.ControlKey, ws); err == nil {
		s.Summary, s.SummaryAt = &sum, now
	} else if partErr == nil {
		partErr = err
	}
	// which requested models are Design Plan models on this tier
	for _, m := range p.coverageDue(s, 6, now) {
		mp, err := client.Preflight(ctx, a.ControlKey, ws, m)
		if err != nil {
			if partErr == nil {
				partErr = err
			}
			break
		}
		if s.Coverage == nil {
			s.Coverage = map[string]*Coverage{}
		}
		s.Coverage[m] = &Coverage{Covered: mp.ModelCovered, Funding: mp.Funding, At: now}
	}

	s.LastOKAt = now
	s.Failures = 0
	if partErr != nil {
		s.LastError, s.LastErrorAt = "partial: "+partErr.Error(), now
	} else {
		s.LastError = ""
	}
	s.NextAt = now.Add(time.Duration(cfg.Quota.IntervalSec) * time.Second)
	p.store(a.ID, s)
	return nil
}

func (p *Poller) store(id string, s *Snapshot) {
	// an account deleted while it was read is not brought back
	if _, ok := p.Cfg().Account(id); !ok {
		return
	}
	p.mu.Lock()
	p.snaps[id] = s
	p.mu.Unlock()
	if p.OnUpdate != nil {
		p.OnUpdate(id)
	}
}
