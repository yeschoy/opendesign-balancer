// Package stats keeps 24h of per-minute request counters, overall and per
// account. It is written when a request ends and never derived from logs.
package stats

import (
	"sync"
	"time"
)

type Bucket struct {
	Minute    int64   `json:"m"`
	Requests  int64   `json:"r"`
	Success   int64   `json:"s"`
	Failed    int64   `json:"f"`
	Cancelled int64   `json:"c"`
	Input     int64   `json:"i"`
	Output    int64   `json:"o"`
	Cached    int64   `json:"k"`
	Prompt    int64   `json:"p,omitempty"` // total prompt tokens (cache hits included), for the hit rate
	CostCNY   float64 `json:"cc,omitempty"`
	CostUSD   float64 `json:"cu,omitempty"`
}

func (b *Bucket) add(o Bucket) {
	b.Requests += o.Requests
	b.Success += o.Success
	b.Failed += o.Failed
	b.Cancelled += o.Cancelled
	b.Input += o.Input
	b.Output += o.Output
	b.Cached += o.Cached
	b.Prompt += o.Prompt
	b.CostCNY += o.CostCNY
	b.CostUSD += o.CostUSD
}

type ring [1440]Bucket

func (r *ring) slot(m int64) *Bucket {
	b := &r[m%1440]
	if b.Minute != m {
		*b = Bucket{Minute: m}
	}
	return b
}

type Stats struct {
	mu      sync.Mutex
	global  ring
	account map[string]*ring
	model   map[string]*ring
	proxy   map[string]*ring
	now     func() time.Time
}

func New() *Stats {
	return &Stats{account: map[string]*ring{}, model: map[string]*ring{}, proxy: map[string]*ring{}, now: time.Now}
}

// maxModels bounds the per-model rings: model names come from clients.
const maxModels = 100

// maxProxies bounds the per-egress rings: one per distinct account proxy.
const maxProxies = 200

// Sample is one finished request.
type Sample struct {
	AccountID, Model string
	Proxy            string // egress label: proxy host:port, or "direct"
	Result           string // success | failed | client_cancelled
	Input, Output    *int64
	Cached           *int64
	Prompt           int64 // total prompt tokens, cache hits included
	Currency         string
	Cost             float64
}

// Record adds one finished request.
func (s *Stats) Record(x Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.now().Unix() / 60
	d := Bucket{Requests: 1, Prompt: x.Prompt}
	switch x.Result {
	case "success":
		d.Success = 1
	case "client_cancelled":
		d.Cancelled = 1
	default:
		d.Failed = 1
	}
	switch x.Currency {
	case "CNY":
		d.CostCNY = x.Cost
	case "USD":
		d.CostUSD = x.Cost
	}
	for _, y := range []struct {
		p   *int64
		dst *int64
	}{{x.Input, &d.Input}, {x.Output, &d.Output}, {x.Cached, &d.Cached}} {
		if y.p != nil {
			*y.dst = *y.p
		}
	}
	s.global.slot(m).add(d)
	if x.AccountID != "" {
		r := s.account[x.AccountID]
		if r == nil {
			r = &ring{}
			s.account[x.AccountID] = r
		}
		r.slot(m).add(d)
	}
	if x.Proxy != "" {
		r := s.proxy[x.Proxy]
		if r == nil && len(s.proxy) < maxProxies {
			r = &ring{}
			s.proxy[x.Proxy] = r
		}
		if r != nil {
			r.slot(m).add(d)
		}
	}
	if x.Model != "" {
		r := s.model[x.Model]
		if r == nil && len(s.model) < maxModels {
			r = &ring{}
			s.model[x.Model] = r
		}
		if r != nil {
			r.slot(m).add(d)
		}
	}
}

type Summary struct {
	Total     Bucket            `json:"total"`
	Hourly    []Bucket          `json:"hourly"` // 24 entries, oldest first; Minute is the hour's first minute
	Accounts  map[string]Bucket `json:"accounts"`
	Models    map[string]Bucket `json:"models"`
	Proxies   map[string]Bucket `json:"proxies"`
	LastHour  Bucket            `json:"lastHour"`
	Generated time.Time         `json:"generated"`
}

func (s *Stats) Summary() Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	m := now.Unix() / 60
	cut := m - 1440
	hourStart := (m/60 - 23) * 60
	out := Summary{Hourly: make([]Bucket, 24), Accounts: map[string]Bucket{}, Models: map[string]Bucket{}, Proxies: map[string]Bucket{}, Generated: now.UTC()}
	for i := range out.Hourly {
		out.Hourly[i].Minute = hourStart + int64(i)*60
	}
	for _, b := range s.global {
		if b.Minute <= cut || b.Requests == 0 {
			continue
		}
		out.Total.add(b)
		if b.Minute > m-60 {
			out.LastHour.add(b)
		}
		if h := (b.Minute - hourStart) / 60; h >= 0 && h < 24 {
			out.Hourly[h].add(b)
		}
	}
	for id, r := range s.account {
		var t Bucket
		for _, b := range r {
			if b.Minute > cut {
				t.add(b)
			}
		}
		if t.Requests > 0 {
			out.Accounts[id] = t
		}
	}
	for name, r := range s.model {
		var t Bucket
		for _, b := range r {
			if b.Minute > cut {
				t.add(b)
			}
		}
		if t.Requests > 0 {
			out.Models[name] = t
		}
	}
	for name, r := range s.proxy {
		var t Bucket
		for _, b := range r {
			if b.Minute > cut {
				t.add(b)
			}
		}
		if t.Requests > 0 {
			out.Proxies[name] = t
		}
	}
	return out
}

type Export struct {
	Global   []Bucket            `json:"global"`
	Accounts map[string][]Bucket `json:"accounts"`
	Models   map[string][]Bucket `json:"models,omitempty"`
	Proxies  map[string][]Bucket `json:"proxies,omitempty"`
}

func (s *Stats) Export() Export {
	s.mu.Lock()
	defer s.mu.Unlock()
	cut := s.now().Unix()/60 - 1440
	pick := func(r *ring) []Bucket {
		var out []Bucket
		for _, b := range r {
			if b.Minute > cut && b.Requests > 0 {
				out = append(out, b)
			}
		}
		return out
	}
	e := Export{Global: pick(&s.global), Accounts: map[string][]Bucket{}, Models: map[string][]Bucket{}, Proxies: map[string][]Bucket{}}
	for id, r := range s.account {
		if l := pick(r); len(l) > 0 {
			e.Accounts[id] = l
		}
	}
	for name, r := range s.model {
		if l := pick(r); len(l) > 0 {
			e.Models[name] = l
		}
	}
	for name, r := range s.proxy {
		if l := pick(r); len(l) > 0 {
			e.Proxies[name] = l
		}
	}
	return e
}

// withPrompt fills Prompt for buckets recorded before it existed: those
// requests reported input including the cache hits, so the prompt is the input.
func withPrompt(b Bucket) Bucket {
	if b.Prompt == 0 {
		b.Prompt = b.Input
	}
	return b
}

func (s *Stats) Import(e Export) {
	s.mu.Lock()
	defer s.mu.Unlock()
	load := func(l []Bucket) *ring {
		r := &ring{}
		for _, b := range l {
			r[b.Minute%1440] = withPrompt(b)
		}
		return r
	}
	for _, b := range e.Global {
		s.global[b.Minute%1440] = withPrompt(b)
	}
	for id, l := range e.Accounts {
		s.account[id] = load(l)
	}
	for name, l := range e.Models {
		if len(s.model) >= maxModels {
			break
		}
		s.model[name] = load(l)
	}
	s.importProxies(e, load)
}

func (s *Stats) importProxies(e Export, load func([]Bucket) *ring) {
	for name, l := range e.Proxies {
		if len(s.proxy) >= maxProxies {
			break
		}
		s.proxy[name] = load(l)
	}
}

func (s *Stats) Forget(id string) {
	s.mu.Lock()
	delete(s.account, id)
	s.mu.Unlock()
}
