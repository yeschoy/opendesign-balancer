package pricing

import (
	"math"
	"testing"
	"time"
)

func p(v int64) *int64 { return &v }

func TestPeakWindowIsBeijingWeekdays(t *testing.T) {
	c := &Config{Holidays: []string{"2026-10-26"}}
	at := func(s string) time.Time { // s is Beijing time
		tm, err := time.ParseInLocation("2006-01-02 15:04", s, time.FixedZone("x", 8*3600))
		if err != nil {
			t.Fatal(err)
		}
		return tm.UTC()
	}
	for s, want := range map[string]bool{
		"2026-10-23 10:00": true,  // Friday
		"2026-10-23 12:00": false, // lunch
		"2026-10-23 14:00": true,
		"2026-10-23 18:00": false,
		"2026-10-23 08:59": false,
		"2026-10-24 10:00": false, // Saturday
		"2026-10-26 10:00": false, // listed holiday
		"2026-10-27 17:59": true,
	} {
		if got := c.Peak(at(s)); got != want {
			t.Errorf("Peak(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestCost(t *testing.T) {
	c := &Config{Models: Defaults()}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	peak := time.Date(2026, 10, 23, 2, 0, 0, 0, time.UTC) // 10:00 Beijing, Friday
	idle := peak.Add(10 * time.Hour)                      // 20:00 Beijing
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

	// 1M prompt (500k cached), 100k out, flash: 0.5*1 + 0.5*0.02 + 0.1*4 = 0.91 off-peak
	got, cur, ok := c.Cost("deepseek-v4.1-flash", idle, p(1_000_000), p(500_000), p(100_000), false)
	if !ok || cur != "CNY" || !near(got, 0.91) {
		t.Fatalf("off-peak = %v %s %v", got, cur, ok)
	}
	if got, _, _ := c.Cost("deepseek-v4.1-flash", peak, p(1_000_000), p(500_000), p(100_000), false); !near(got, 1.82) {
		t.Fatalf("peak = %v", got)
	}
	// Anthropic usage: input excludes the cache read. 100k uncached + 900k cached, 10k out on fable-5
	if got, _, _ := c.Cost("claude-fable-5", idle, p(100_000), p(900_000), p(10_000), true); !near(got, 0.1*10+0.9*1+0.01*50) {
		t.Fatalf("anthropic = %v", got)
	}
	// long-context tier applies to the whole request
	if got, _, _ := c.Cost("grok-4.5", idle, p(250_000), p(0), p(1000), false); !near(got, 0.25*4+0.001*12) {
		t.Fatalf("long tier = %v", got)
	}
	if _, _, ok := c.Cost("no-such-model", idle, p(1), nil, p(1), false); ok {
		t.Fatal("unknown model priced")
	}
	if _, _, ok := c.Cost("grok-4.5", idle, nil, nil, nil, false); ok {
		t.Fatal("priced a request without usage")
	}
}

func TestScheduledPriceChange(t *testing.T) {
	c := &Config{Models: Defaults()}
	before, _ := c.Lookup("gemini-4-argon", time.Date(2026, 11, 30, 12, 0, 0, 0, time.UTC))
	after, _ := c.Lookup("gemini-4-argon", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) // 08:00 Beijing
	if before.Input != 2 || after.Input != 4 {
		t.Fatalf("before %v after %v", before.Input, after.Input)
	}
}

func TestNormalizeRejectsBadRows(t *testing.T) {
	for _, e := range []Entry{{Model: "", Currency: "USD"}, {Model: "m", Currency: "EUR"}, {Model: "m", Currency: "USD", Input: -1}, {Model: "m", Currency: "USD", From: "soon"}} {
		c := &Config{Models: []Entry{e}}
		if c.Normalize() == nil {
			t.Errorf("accepted %+v", e)
		}
	}
	c := &Config{Models: []Entry{}}
	if err := c.Normalize(); err != nil || len(c.Models) != 0 {
		t.Fatalf("an emptied table must stay empty: %v %d", err, len(c.Models))
	}
}
