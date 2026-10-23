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
