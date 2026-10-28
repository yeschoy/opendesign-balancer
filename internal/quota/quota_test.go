package quota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// shapes as read from the real control plane on 2026-10-23
const preflightJSON = `{"schemaVersion":1,"workspaceId":"ws1","workspaceMemberId":"m1","modelId":null,"balanceUsd":"0.0000","codingPlan":{"workspaceId":"ws1","eligible":true,"tier":"plus","windows":[{"policyId":"p","durationSeconds":604800,"resetMode":"anchored_recurring","usedCredits":"30702","limitCredits":"200000","remainingCredits":"169298","windowStart":"2026-10-20T12:02:09.499Z","resetsAt":"2026-10-27T12:02:09.499Z"}]},"modelCovered":null,"funding":"gateway"}`
const summaryJSON = `{"balanceUsd":"0.0000","creditsPerUsd":10000,"balances":{"subscriptionCredits":"0","rechargeCredits":"0","totalAvailableCredits":"0"},"subscriptionCreditsUsd":"0.0000","membershipTier":"plus","subscriptionStatus":"active","subscriptionCurrentPeriodStart":"2026-10-17T20:58:16.000Z","subscriptionCurrentPeriodEnd":"2026-11-17T20:58:16.000Z","pendingSubscriptionChange":null,"totalConsumedUsd":"31.5687","usageCount":8313,"rechargeCount":2}`

func TestClientParsesControlPlane(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer vela_ctrl_good" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"unauthenticated"}`))
			return
		}
		switch r.URL.Path {
		case "/api/v1/workspaces/current":
			w.Write([]byte(`{"workspaceId":"ws1","activeWorkspaceId":"ws1","workspaceMemberId":"m1","personalMembershipTier":"plus"}`))
		case "/api/v1/workspaces/ws1/billing/preflight":
			w.Write([]byte(preflightJSON))
		case "/api/v1/billing/summary":
			w.Write([]byte(summaryJSON))
		case "/api/v1/workspaces/ws1/wallet/balance":
			w.Write([]byte(`{"balanceUsd":"1.5000","workspaceId":"ws1"}`))
		}
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	ctx := context.Background()
	pf, err := c.Preflight(ctx, "vela_ctrl_good", "ws1", "")
	if err != nil {
		t.Fatal(err)
	}
	if !pf.Eligible || len(pf.Windows) != 1 || pf.Windows[0].Remaining != 169298 || pf.Windows[0].ResetsAt.IsZero() {
		t.Fatalf("preflight = %+v", pf)
	}
	sum, err := c.Summary(ctx, "vela_ctrl_good", "ws1")
	if err != nil || sum.SubscriptionCredits != "0" || sum.UsageCount != 8313 || sum.CreditsPerUSD != 10000 {
		t.Fatalf("summary = %+v %v", sum, err)
	}
	_, err = c.Workspace(ctx, "vela_ctrl_bad")
	if !IsAuth(err) || err.Error() != "HTTP 401 unauthenticated" {
		t.Fatalf("auth error = %v", err)
	}

	snap := &Snapshot{PlanAt: time.Now(), Eligible: true, Windows: pf.Windows, WalletUSD: "0.0000"}
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	if !snap.PlanRoom(now) || snap.WalletPositive() {
		t.Fatal("plan room / wallet wrong")
	}
	if u := snap.UsedShare(now); u < 15.3 || u > 15.4 {
		t.Fatalf("used share = %v", u)
	}
	// once the window's reset has gone by its full limit is back
	if snap.Windows[0].EffectiveRemaining(now.Add(10*24*time.Hour)) != 200000 {
		t.Fatal("effective remaining after reset")
	}
}

func TestPollerKeepsLastGoodValueOnFailure(t *testing.T) {
	s := &Snapshot{WalletUSD: "2.0000", PlanAt: time.Now()}
	// a failed read must not zero what was read before
	c := s.clone()
	c.LastError = "HTTP 500"
	if c.WalletUSD != "2.0000" {
		t.Fatal("clone lost wallet")
	}
	if backoff(1) != time.Minute || backoff(3) != 4*time.Minute || backoff(10) != 15*time.Minute {
		t.Fatalf("backoff = %v %v %v", backoff(1), backoff(3), backoff(10))
	}
}

// The two /api/balance shapes from docs.ollama.com/api/balance: the plan's
// included allowance over a period, and a legacy plan's session/weekly
// percentages, either with a purchased balance next to it.
const ollamaIncludedJSON = `{"included":{"balance_usd":72.5,"allowance_usd":100,"period":{"from":"2026-09-15T09:30:00Z","until":"2026-10-15T09:30:00Z"}},"purchased":{"balance_usd":25}}`
const ollamaLegacyJSON = `{"included":{"session":{"remaining_percent":75,"resets_at":"2026-11-01T07:00:00Z"},"weekly":{"remaining_percent":40,"resets_at":"2026-11-05T00:00:00Z"}},"purchased":{"balance_usd":25}}`

func TestOllamaBalance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/balance" {
			w.WriteHeader(404)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer good":
			w.Write([]byte(ollamaIncludedJSON))
		case "Bearer legacy":
			w.Write([]byte(ollamaLegacyJSON))
		case "Bearer empty":
			w.Write([]byte(`{"included":{"balance_usd":0,"allowance_usd":100,"period":{"from":"2026-09-15T09:30:00Z","until":"2026-11-15T09:30:00Z"}},"purchased":{"balance_usd":0}}`))
		default:
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"invalid credentials"}`))
		}
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, HTTP: srv.Client()}
	ctx := context.Background()

	b, err := c.Balance(ctx, srv.URL, "good")
	if err != nil {
		t.Fatal(err)
	}
	if b.IncludedUSD != "72.5" || b.AllowanceUSD != "100" || b.PurchasedUSD != "25" || b.PeriodUntil.IsZero() {
		t.Fatalf("included balance = %+v", b)
	}
	if b.SessionPercent != nil || b.WeeklyPercent != nil {
		t.Fatalf("a plan without legacy limits reports none: %+v", b)
	}

	b, err = c.Balance(ctx, srv.URL, "legacy")
	if err != nil || b.SessionPercent == nil || *b.SessionPercent != 75 || b.WeeklyPercent == nil || *b.WeeklyPercent != 40 || b.SessionResetsAt.IsZero() {
		t.Fatalf("legacy balance = %+v %v", b, err)
	}

	// a refused key is an auth error, so the poller can quarantine the account
	if _, err := c.Balance(ctx, srv.URL, "bad"); !IsAuth(err) {
		t.Fatalf("bad key = %v", err)
	}

	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	stale := time.Hour
	snap := &Snapshot{Ollama: &b, BalanceAt: now}
	// legacy: session 75% / weekly 40% left, plus $25 purchased
	if src, ex := snap.OllamaFunding(now, stale); src != "included" || ex {
		t.Fatalf("legacy funding = %q %v", src, ex)
	}
	if u := snap.OllamaUsedShare(now); u != 60 { // 100 - 40 (the weekly limit binds)
		t.Fatalf("legacy used share = %v", u)
	}

	inc := OllamaBalance{IncludedUSD: "72.5", AllowanceUSD: "100", PurchasedUSD: "25", PeriodUntil: now.Add(48 * time.Hour)}
	snap = &Snapshot{Ollama: &inc, BalanceAt: now}
	if src, ex := snap.OllamaFunding(now, stale); src != "included" || ex {
		t.Fatalf("included funding = %q %v", src, ex)
	}
	if u := snap.OllamaUsedShare(now); u < 27.4 || u > 27.6 {
		t.Fatalf("included used share = %v", u)
	}
	// the included allowance is gone but the purchased credits still pay
	inc2 := OllamaBalance{IncludedUSD: "0", AllowanceUSD: "100", PurchasedUSD: "25", PeriodUntil: now.Add(48 * time.Hour)}
	snap = &Snapshot{Ollama: &inc2, BalanceAt: now}
	if src, ex := snap.OllamaFunding(now, stale); src != "purchased" || ex {
		t.Fatalf("purchased funding = %q %v", src, ex)
	}
	// ... and with neither there is positive evidence the account can't pay
	inc3 := OllamaBalance{IncludedUSD: "0", AllowanceUSD: "100", PurchasedUSD: "0", PeriodUntil: now.Add(48 * time.Hour)}
	snap = &Snapshot{Ollama: &inc3, BalanceAt: now}
	if src, ex := snap.OllamaFunding(now, stale); !ex {
		t.Fatalf("empty funding = %q %v", src, ex)
	}
	// a period that has run out is a fresh allowance, not an empty one
	inc4 := OllamaBalance{IncludedUSD: "0", AllowanceUSD: "100", PurchasedUSD: "0", PeriodUntil: now.Add(-time.Minute)}
	snap = &Snapshot{Ollama: &inc4, BalanceAt: now}
	if src, ex := snap.OllamaFunding(now, stale); src != "included" || ex {
		t.Fatalf("expired period funding = %q %v", src, ex)
	}
	// a stale or unread balance proves nothing
	snap = &Snapshot{Ollama: &inc3, BalanceAt: now.Add(-2 * time.Hour)}
	if src, ex := snap.OllamaFunding(now, stale); src != "unknown" || ex {
		t.Fatalf("stale funding = %q %v", src, ex)
	}
	// a failed read never turns into "0 left"
	snap = &Snapshot{Ollama: &inc3, BalanceAt: now, AuthFailed: true}
	if src, ex := snap.OllamaFunding(now, stale); src != "unknown" || ex {
		t.Fatalf("failed read funding = %q %v", src, ex)
	}
}
