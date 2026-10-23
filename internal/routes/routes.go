// Package routes orders one pool of candidate accounts for a request whose
// conversation nobody is bound to. It is pure: the pool lists candidates,
// handles bindings, leases and failover; a strategy only says the order.
//
// The quota-aware strategies follow magpie's internal/routes (Smart, Pace,
// Least used, Weighted): an account at LowShare of a plan window goes behind
// those with room, one at SpentShare behind those, then one paying from its
// 套餐模型额度 (subscription credits), and one paying from its wallet last
// of all, since that spends real money.
// Accounts are first grouped by Priority (lower first); the strategy orders
// each group.
package routes

import (
	"sort"
	"sync"
	"time"
)

// Seat is one candidate account as a strategy weighs it.
type Seat struct {
	ID       string
	Priority int
	Weight   int
	// Known: a fresh quota snapshot exists; Used, Soon and Pace come from it.
	Known bool
	// Used is the share of the fullest plan window used, 0–100.
	Used float64
	// Fallback is what pays when the plan windows don't: 0 the plan itself,
	// 1 subscription credits (套餐模型额度), 2 wallet / recharge.
	Fallback int
	// Soon is when what pays renews: the plan windows (longest first), or
	// the billing period's end for subscription credits, which don't carry over.
	Soon []time.Time
	// Pace is the longest window's share left (0–100) per hour until it
	// renews (at least one hour).
	Pace         float64
	Active       int
	Count        int64
	LastSelected int64
	// Tokens is what this pool sent the account lately (decayed).
	Tokens float64
}

type Params struct {
	LowShare   float64
	SpentShare float64
	// Cursor rotates least-selections ties so equal counts spread.
	Cursor int64
}

func (p Params) tier(s Seat) int {
	switch {
	case s.Fallback >= 2:
		return 4
	case s.Fallback == 1:
		return 3
	case s.Used >= p.SpentShare:
		return 2
	case s.Used >= p.LowShare:
		return 1
	}
	return 0
}

// Order returns a permutation of seats' indexes, the first to try first.
func Order(strategy, pool string, seats []Seat, p Params) []int {
	if len(seats) == 0 {
		return nil
	}
	if p.LowShare == 0 {
		p.LowShare = 90
	}
	if p.SpentShare == 0 {
		p.SpentShare = 98
	}
	byPriority := map[int][]int{}
	var prios []int
	for i, s := range seats {
		if _, ok := byPriority[s.Priority]; !ok {
			prios = append(prios, s.Priority)
		}
		byPriority[s.Priority] = append(byPriority[s.Priority], i)
	}
	sort.Ints(prios)
	out := make([]int, 0, len(seats))
	for _, pr := range prios {
		idx := byPriority[pr]
		sub := make([]Seat, len(idx))
		for k, i := range idx {
			sub[k] = seats[i]
		}
		for _, k := range orderGroup(strategy, pool, sub, p) {
			out = append(out, idx[k])
		}
	}
	return out
}

func identity(n int) []int {
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	return idx
}

func orderGroup(strategy, pool string, seats []Seat, p Params) []int {
	idx := identity(len(seats))
	if len(seats) < 2 {
		return idx
	}
	// spread ties: the fewest selections, then the fewest in flight
	spread := func(a, b Seat) (bool, bool) {
		if a.Count != b.Count {
			return a.Count < b.Count, true
		}
		if a.Active != b.Active {
			return a.Active < b.Active, true
		}
		return false, false
	}
	sortBy := func(less func(a, b Seat) (bool, bool)) {
		sort.SliceStable(idx, func(i, j int) bool {
			r, decided := less(seats[idx[i]], seats[idx[j]])
			return decided && r
		})
	}
	switch strategy {
	case "ordered":
		sortBy(func(a, b Seat) (bool, bool) {
			if ta, tb := p.tier(a), p.tier(b); ta != tb && (ta >= 3 || tb >= 3) {
				return ta < tb, true
			}
			return false, false
		})
	case "roundrobin":
		sortBy(func(a, b Seat) (bool, bool) {
			if a.LastSelected != b.LastSelected {
				return a.LastSelected < b.LastSelected, true
			}
			return false, false
		})
	case "least-selections":
		n := int(p.Cursor % int64(len(seats)))
		if n < 0 {
			n = 0
		}
		idx = append(idx[n:], idx[:n]...)
		sortBy(spread)
	case "least-connections":
		sortBy(func(a, b Seat) (bool, bool) {
			if a.Active != b.Active {
				return a.Active < b.Active, true
			}
			return spread(a, b)
		})
	case "least-used":
		sortBy(func(a, b Seat) (bool, bool) {
			if ta, tb := p.tier(a), p.tier(b); ta != tb && (ta >= 3 || tb >= 3) {
				return ta < tb, true
			}
			if a.Used != b.Used {
				return a.Used < b.Used, true
			}
			if a.Tokens != b.Tokens {
				return a.Tokens < b.Tokens, true
			}
			return spread(a, b)
		})
	case "weighted":
		keys, weights := make([]string, len(seats)), make([]int, len(seats))
		for i, s := range seats {
			keys[i], weights[i] = s.ID, max(s.Weight, 1)
		}
		best := Round(pool, keys, weights)
		if best > 0 {
			idx = append(append([]int{best}, idx[:best]...), idx[best+1:]...)
		}
	case "pace":
		return pace(seats, p, spread)
	default: // smart
		return smart(seats, p, spread)
	}
	return idx
}

// smart: of those with room, the one whose allowance renews soonest goes
// first, because what it has left is lost at the reset. The longest window
// decides, the next only when the longest renew in the same hour. Unknown
// accounts follow the known ones; low and spent ones follow by share used.
func smart(seats []Seat, p Params, spread func(a, b Seat) (bool, bool)) []int {
	idx := identity(len(seats))
	sort.SliceStable(idx, func(i, j int) bool {
		a, b := seats[idx[i]], seats[idx[j]]
		ta, tb := p.tier(a), p.tier(b)
		if ta != tb {
			return ta < tb
		}
		if ta == 1 || ta == 2 {
			if a.Used != b.Used {
				return a.Used < b.Used
			}
			r, ok := spread(a, b)
			return ok && r
		}
		// with room, or on subscription credits: what renews soonest goes first
		for k := 0; k < len(a.Soon) || k < len(b.Soon); k++ {
			var x, y time.Time
			if k < len(a.Soon) {
				x = a.Soon[k].Truncate(time.Hour)
			}
			if k < len(b.Soon) {
				y = b.Soon[k].Truncate(time.Hour)
			}
			switch {
			case x.Equal(y):
				continue
			case x.IsZero() || y.IsZero():
				return y.IsZero()
			}
			return x.Before(y)
		}
		r, ok := spread(a, b)
		return ok && r
	})
	return idx
}

// pace: the most week left per hour until it renews goes first; paces within
// a tenth of a band's top count alike and are spread by recent tokens.
func pace(seats []Seat, p Params, spread func(a, b Seat) (bool, bool)) []int {
	var byPace []int
	for i, s := range seats {
		if p.tier(s) == 0 {
			byPace = append(byPace, i)
		}
	}
	sort.SliceStable(byPace, func(a, b int) bool { return seats[byPace[a]].Pace > seats[byPace[b]].Pace })
	band, top := make([]int, len(seats)), 0.0
	for k, i := range byPace {
		pc := seats[i].Pace
		switch {
		case k == 0:
			top = pc
		case pc < 0.9*top:
			top = pc
			band[i] = band[byPace[k-1]] + 1
		default:
			band[i] = band[byPace[k-1]]
		}
	}
	idx := identity(len(seats))
	sort.SliceStable(idx, func(i, j int) bool {
		a, b := seats[idx[i]], seats[idx[j]]
		ta, tb := p.tier(a), p.tier(b)
		if ta != tb {
			return ta < tb
		}
		if ta > 0 {
			if a.Used != b.Used {
				return a.Used < b.Used
			}
		} else if band[idx[i]] != band[idx[j]] {
			return band[idx[i]] < band[idx[j]]
		}
		if a.Tokens != b.Tokens {
			return a.Tokens < b.Tokens
		}
		r, ok := spread(a, b)
		return ok && r
	})
	return idx
}

// Smooth weighted round robin (NGINX's): weights 3 and 1 go a, a, b, a, …
var rounds = struct {
	sync.Mutex
	m map[string]map[string]int
}{m: map[string]map[string]int{}}

// Round picks one of keys by smooth weighted round robin for pool.
func Round(pool string, keys []string, weights []int) int {
	rounds.Lock()
	defer rounds.Unlock()
	cur := rounds.m[pool]
	if cur == nil {
		cur = map[string]int{}
		rounds.m[pool] = cur
	}
	on := make(map[string]bool, len(keys))
	for _, k := range keys {
		on[k] = true
	}
	for k := range cur {
		if !on[k] {
			delete(cur, k)
		}
	}
	best, total := -1, 0
	for i, k := range keys {
		cur[k] += weights[i]
		total += weights[i]
		if best < 0 || cur[k] > cur[keys[best]] {
			best = i
		}
	}
	if best >= 0 {
		cur[keys[best]] -= total
	}
	return best
}

// Forget clears round-robin state (tests).
func Forget() {
	rounds.Lock()
	rounds.m = map[string]map[string]int{}
	rounds.Unlock()
}
