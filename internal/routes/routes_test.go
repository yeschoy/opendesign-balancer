package routes

import (
	"testing"
	"time"
)

func ids(seats []Seat, order []int) []string {
	out := make([]string, len(order))
	for i, o := range order {
		out[i] = seats[o].ID
	}
	return out
}

func TestSmartSoonestResetFirstAndTiers(t *testing.T) {
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	seats := []Seat{
		{ID: "late", Priority: 50, Known: true, Used: 10, Soon: []time.Time{now.Add(96 * time.Hour)}},
		{ID: "soon", Priority: 50, Known: true, Used: 50, Soon: []time.Time{now.Add(5 * time.Hour)}},
		{ID: "low", Priority: 50, Known: true, Used: 93, Soon: []time.Time{now.Add(time.Hour)}},
		{ID: "unknown", Priority: 50},
		{ID: "wallet", Priority: 50, Known: true, Used: 100, Fallback: 2},
		{ID: "sub", Priority: 50, Known: true, Used: 100, Fallback: 1, Soon: []time.Time{now.Add(240 * time.Hour)}},
		{ID: "spent", Priority: 50, Known: true, Used: 99, Soon: []time.Time{now.Add(time.Hour)}},
	}
	got := ids(seats, Order("smart", "p", seats, Params{LowShare: 90, SpentShare: 98}))
	want := []string{"soon", "late", "unknown", "low", "spent", "sub", "wallet"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("smart order = %v, want %v", got, want)
		}
	}
}

func TestPriorityGroupsFirst(t *testing.T) {
	seats := []Seat{{ID: "b", Priority: 60}, {ID: "a", Priority: 10, Count: 100}}
	got := ids(seats, Order("least-selections", "p", seats, Params{}))
	if got[0] != "a" {
		t.Fatalf("priority 10 must come first, got %v", got)
	}
}

func TestWeightedSmooth(t *testing.T) {
	Forget()
	seats := []Seat{{ID: "a", Priority: 1, Weight: 3}, {ID: "b", Priority: 1, Weight: 1}}
	n := map[string]int{}
	for range 8 {
		n[ids(seats, Order("weighted", "w", seats, Params{}))[0]]++
	}
	if n["a"] != 6 || n["b"] != 2 {
		t.Fatalf("weighted 3:1 over 8 = %v", n)
	}
}

func TestLeastSelectionsSpreads(t *testing.T) {
	seats := []Seat{{ID: "a", Priority: 1, Count: 5}, {ID: "b", Priority: 1, Count: 2}, {ID: "c", Priority: 1, Count: 2, Active: 1}}
	got := ids(seats, Order("least-selections", "p", seats, Params{}))
	if got[0] != "b" || got[2] != "a" {
		t.Fatalf("least-selections = %v", got)
	}
}
