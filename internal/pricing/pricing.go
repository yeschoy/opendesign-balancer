// Package pricing prices a request from the model's per-1M-token rates.
// Rates are kept in the vendor's own currency; nothing is converted.
package pricing

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Entry is one model's rates. Several entries may share a model: the one
// with the latest From that has begun is used (for announced price changes).
type Entry struct {
	Model       string  `json:"model"`
	Currency    string  `json:"currency"` // CNY | USD
	Input       float64 `json:"input"`    // per 1M uncached input tokens
	CachedInput float64 `json:"cachedInput"`
	Output      float64 `json:"output"`
	// PeakMultiplier > 1 multiplies every rate in the peak window (see Peak);
	// the rates above are then the off-peak ones.
	PeakMultiplier float64 `json:"peakMultiplier,omitempty"`
	From           string  `json:"from,omitempty"` // YYYY-MM-DD, Beijing time; empty: always
	// A request whose prompt has at least LongThreshold tokens is billed at
	// the Long* rates (the whole request, not just the excess).
	LongThreshold   int64   `json:"longThreshold,omitempty"`
	LongInput       float64 `json:"longInput,omitempty"`
	LongCachedInput float64 `json:"longCachedInput,omitempty"`
	LongOutput      float64 `json:"longOutput,omitempty"`
	Note            string  `json:"note,omitempty"`
}

type Config struct {
	// Models is nil until first set; Normalize then fills the defaults.
	Models []Entry `json:"models"`
	// Holidays are Beijing dates (YYYY-MM-DD) that count as off-peak even on weekdays.
	Holidays []string `json:"holidays"`
}

var beijing = time.FixedZone("CST", 8*3600)

// Peak says whether t is in the peak window: Beijing time, Monday to Friday
// outside holidays, 09:00-12:00 and 14:00-18:00.
func (c *Config) Peak(t time.Time) bool {
	t = t.In(beijing)
	if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	h := t.Hour()
	if !(h >= 9 && h < 12 || h >= 14 && h < 18) {
		return false
	}
	day := t.Format("2006-01-02")
	for _, d := range c.Holidays {
		if d == day {
			return false
		}
	}
	return true
}

// Lookup finds the entry in force for model at t.
func (c *Config) Lookup(model string, t time.Time) (Entry, bool) {
	today := t.In(beijing).Format("2006-01-02")
	var best Entry
	found := false
	for _, e := range c.Models {
		if e.Model != model || e.From > today {
			continue
		}
		if !found || e.From >= best.From {
			best, found = e, true
		}
	}
	return best, found
}

// Cost prices a finished request. in/cached/out are the usage a response
// reported; anthropic says input excludes cache reads (Messages usage)
// instead of including them (Chat and Responses usage). ok is false when
// the model has no price or no usage was reported.
func (c *Config) Cost(model string, t time.Time, in, cached, out *int64, anthropic bool) (cost float64, currency string, ok bool) {
	if in == nil && out == nil {
		return 0, "", false
	}
	e, found := c.Lookup(model, t)
	if !found {
		return 0, "", false
	}
	val := func(p *int64) int64 {
		if p == nil {
			return 0
		}
		return *p
	}
	prompt, hit := val(in), val(cached)
	uncached := prompt - hit
	if anthropic {
		uncached, prompt = prompt, prompt+hit
	}
	uncached = max(uncached, 0)
	pi, pc, po := e.Input, e.CachedInput, e.Output
	if e.LongThreshold > 0 && prompt >= e.LongThreshold {
		pi, pc, po = e.LongInput, e.LongCachedInput, e.LongOutput
	}
	cost = (float64(uncached)*pi + float64(hit)*pc + float64(val(out))*po) / 1e6
	if e.PeakMultiplier > 1 && c.Peak(t) {
		cost *= e.PeakMultiplier
	}
	return cost, e.Currency, true
}

// Normalize fills the defaults into an unset table and validates the rest.
func (c *Config) Normalize() error {
	if c.Models == nil {
		c.Models = Defaults()
	}
	if c.Holidays == nil {
		c.Holidays = []string{}
	}
	if len(c.Models) > 1000 {
		return fmt.Errorf("pricing.models: at most 1000 entries")
	}
	for i := range c.Models {
		e := &c.Models[i]
		e.Model, e.Currency = strings.TrimSpace(e.Model), strings.ToUpper(strings.TrimSpace(e.Currency))
		if e.Model == "" {
			return fmt.Errorf("pricing.models[%d]: model is required", i)
		}
		if e.Currency != "CNY" && e.Currency != "USD" {
			return fmt.Errorf("pricing.models[%d] %s: currency must be CNY or USD", i, e.Model)
		}
		for _, v := range []float64{e.Input, e.CachedInput, e.Output, e.PeakMultiplier, e.LongInput, e.LongCachedInput, e.LongOutput} {
			if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) || v > 1e6 {
				return fmt.Errorf("pricing.models[%d] %s: rates must be between 0 and 1000000", i, e.Model)
			}
		}
		if e.LongThreshold < 0 {
			return fmt.Errorf("pricing.models[%d] %s: longThreshold must not be negative", i, e.Model)
		}
		if e.From != "" {
			if _, err := time.Parse("2006-01-02", e.From); err != nil {
				return fmt.Errorf("pricing.models[%d] %s: from must be YYYY-MM-DD", i, e.Model)
			}
		}
	}
	for i, d := range c.Holidays {
		if _, err := time.Parse("2006-01-02", d); err != nil {
			return fmt.Errorf("pricing.holidays[%d]: %q is not YYYY-MM-DD", i, d)
		}
	}
	return nil
}
