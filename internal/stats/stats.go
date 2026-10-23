// Package stats keeps 24h of per-minute request counters, overall and per
// account. It is written when a request ends and never derived from logs.
package stats

import (
	"sync"
	"time"
)

type Bucket struct {
	Minute    int64 `json:"m"`
	Requests  int64 `json:"r"`
	Success   int64 `json:"s"`
	Failed    int64 `json:"f"`
	Cancelled int64 `json:"c"`
	Input     int64 `json:"i"`
	Output    int64 `json:"o"`
	Cached    int64 `json:"k"`
}

func (b *Bucket) add(o Bucket) {
	b.Requests += o.Requests
	b.Success += o.Success
	b.Failed += o.Failed
	b.Cancelled += o.Cancelled
	b.Input += o.Input
	b.Output += o.Output
	b.Cached += o.Cached
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
	now     func() time.Time
}

func New() *Stats { return &Stats{account: map[string]*ring{}, now: time.Now} }

// Record adds one finished request. result is success | failed | client_cancelled.
func (s *Stats) Record(accountID, result string, input, output, cached *int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.now().Unix() / 60
	d := Bucket{Requests: 1}
	switch result {
	case "success":
		d.Success = 1
	case "client_cancelled":
		d.Cancelled = 1
	default:
		d.Failed = 1
	}
	for _, x := range []struct {
		p   *int64
		dst *int64
	}{{input, &d.Input}, {output, &d.Output}, {cached, &d.Cached}} {
		if x.p != nil {
			*x.dst = *x.p
		}
	}
	s.global.slot(m).add(d)
	if accountID != "" {
		r := s.account[accountID]
		if r == nil {
			r = &ring{}
			s.account[accountID] = r
		}
		r.slot(m).add(d)
	}
}

type Summary struct {
	Total     Bucket            `json:"total"`
	Hourly    []Bucket          `json:"hourly"` // 24 entries, oldest first; Minute is the hour's first minute
	Accounts  map[string]Bucket `json:"accounts"`
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
	out := Summary{Hourly: make([]Bucket, 24), Accounts: map[string]Bucket{}, Generated: now.UTC()}
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
	return out
}

type Export struct {
	Global   []Bucket            `json:"global"`
	Accounts map[string][]Bucket `json:"accounts"`
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
	e := Export{Global: pick(&s.global), Accounts: map[string][]Bucket{}}
	for id, r := range s.account {
		if l := pick(r); len(l) > 0 {
			e.Accounts[id] = l
		}
	}
	return e
}

func (s *Stats) Import(e Export) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range e.Global {
		s.global[b.Minute%1440] = b
	}
	for id, l := range e.Accounts {
		r := &ring{}
		for _, b := range l {
			r[b.Minute%1440] = b
		}
		s.account[id] = r
	}
}

func (s *Stats) Forget(id string) {
	s.mu.Lock()
	delete(s.account, id)
	s.mu.Unlock()
}
