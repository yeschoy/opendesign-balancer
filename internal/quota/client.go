// Package quota reads each account's OpenDesign control plane with its
// vela_ctrl_ key: the workspace, the Design Plan windows (billing/preflight),
// the billing summary and the wallet. The three amounts are kept apart and
// never converted into one another. A failed read keeps the last good value
// and never turns into "0 left".
package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HTTPError is a non-2xx control-plane answer. Only the status and a fixed
// error code are kept, never the body.
type HTTPError struct {
	Status int
	Code   string
}

func (e *HTTPError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("HTTP %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// IsAuth reports a control key the control plane refused (401/403).
func IsAuth(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && (he.Status == 401 || he.Status == 403)
}

type Client struct {
	Base string
	HTTP *http.Client
}

var codeRe = func(s string) string {
	// keep only [a-z0-9_] codes such as "unauthenticated"
	if len(s) > 64 {
		return ""
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return ""
		}
	}
	return s
}

func (c *Client) get(ctx context.Context, key, path, workspace string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	if workspace != "" {
		req.Header.Set("x-vela-workspace-id", workspace)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("network error")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read error")
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error any `json:"error"`
		}
		code := ""
		if json.Unmarshal(body, &e) == nil {
			if s, ok := e.Error.(string); ok {
				code = codeRe(s)
			} else if m, ok := e.Error.(map[string]any); ok {
				if s, ok := m["code"].(string); ok {
					code = codeRe(s)
				}
			}
		}
		return &HTTPError{Status: resp.StatusCode, Code: code}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("invalid JSON from %s", strings.SplitN(path, "?", 2)[0])
	}
	return nil
}

type Workspace struct {
	ID                     string `json:"workspaceId"`
	ActiveID               string `json:"activeWorkspaceId"`
	MemberID               string `json:"workspaceMemberId"`
	Name                   string `json:"workspaceName"`
	Type                   string `json:"workspaceType"`
	PersonalMembershipTier string `json:"personalMembershipTier"`
	AppUserID              string `json:"appUserId"`
}

func (w Workspace) WorkspaceID() string {
	if w.ActiveID != "" {
		return w.ActiveID
	}
	return w.ID
}

func (c *Client) Workspace(ctx context.Context, key string) (Workspace, error) {
	var w Workspace
	err := c.get(ctx, key, "/api/v1/workspaces/current", "", &w)
	if err == nil && w.WorkspaceID() == "" {
		err = errors.New("workspace id missing")
	}
	return w, err
}

// Window is one Design Plan usage window.
type Window struct {
	DurationSeconds int64     `json:"durationSeconds"`
	Used            int64     `json:"used"`
	Limit           int64     `json:"limit"`
	Remaining       int64     `json:"remaining"`
	ResetMode       string    `json:"resetMode,omitempty"`
	WindowStart     time.Time `json:"windowStart"`
	ResetsAt        time.Time `json:"resetsAt"`
	PolicyID        string    `json:"policyId,omitempty"`
}

type Preflight struct {
	Eligible     bool     `json:"eligible"`
	Tier         string   `json:"tier"`
	Windows      []Window `json:"windows"`
	Funding      string   `json:"funding"`
	ModelCovered *bool    `json:"modelCovered"`
	BalanceUSD   string   `json:"balanceUsd"`
}

type rawPreflight struct {
	BalanceUSD string `json:"balanceUsd"`
	CodingPlan *struct {
		Eligible bool   `json:"eligible"`
		Tier     string `json:"tier"`
		Windows  []struct {
			DurationSeconds  int64  `json:"durationSeconds"`
			UsedCredits      string `json:"usedCredits"`
			LimitCredits     string `json:"limitCredits"`
			RemainingCredits string `json:"remainingCredits"`
			ResetMode        string `json:"resetMode"`
			WindowStart      string `json:"windowStart"`
			ResetsAt         string `json:"resetsAt"`
			PolicyID         string `json:"policyId"`
		} `json:"windows"`
	} `json:"codingPlan"`
	ModelCovered *bool  `json:"modelCovered"`
	Funding      string `json:"funding"`
}

func parseCredits(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty credits")
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid credits %q", s)
	}
	return int64(f), nil
}

func (c *Client) Preflight(ctx context.Context, key, workspace, model string) (Preflight, error) {
	path := "/api/v1/workspaces/" + url.PathEscape(workspace) + "/billing/preflight"
	if model != "" {
		path += "?modelId=" + url.QueryEscape(model)
	}
	var raw rawPreflight
	if err := c.get(ctx, key, path, workspace, &raw); err != nil {
		return Preflight{}, err
	}
	p := Preflight{Funding: raw.Funding, ModelCovered: raw.ModelCovered, BalanceUSD: raw.BalanceUSD, Windows: []Window{}}
	if raw.CodingPlan != nil {
		p.Eligible, p.Tier = raw.CodingPlan.Eligible, raw.CodingPlan.Tier
		for _, w := range raw.CodingPlan.Windows {
			used, e1 := parseCredits(w.UsedCredits)
			limit, e2 := parseCredits(w.LimitCredits)
			rem, e3 := parseCredits(w.RemainingCredits)
			if err := errors.Join(e1, e2, e3); err != nil {
				return Preflight{}, err
			}
			win := Window{DurationSeconds: w.DurationSeconds, Used: used, Limit: limit, Remaining: rem,
				ResetMode: w.ResetMode, PolicyID: w.PolicyID}
			win.WindowStart, _ = time.Parse(time.RFC3339Nano, w.WindowStart)
			win.ResetsAt, _ = time.Parse(time.RFC3339Nano, w.ResetsAt)
			p.Windows = append(p.Windows, win)
		}
	}
	// the longest window first: it decides routing
	sort.SliceStable(p.Windows, func(i, j int) bool { return p.Windows[i].DurationSeconds > p.Windows[j].DurationSeconds })
	return p, nil
}

type Summary struct {
	MembershipTier           string    `json:"membershipTier"`
	SubscriptionStatus       string    `json:"subscriptionStatus"`
	EntitlementStatus        string    `json:"subscriptionEntitlementStatus"`
	BillingInterval          string    `json:"billingInterval"`
	PeriodStart              time.Time `json:"subscriptionCurrentPeriodStart"`
	PeriodEnd                time.Time `json:"subscriptionCurrentPeriodEnd"`
	CancelAtPeriodEnd        bool      `json:"subscriptionCancelAtPeriodEnd"`
	SubscriptionCredits      string    `json:"subscriptionCredits"`
	RechargeCredits          string    `json:"rechargeCredits"`
	TotalAvailableCredits    string    `json:"totalAvailableCredits"`
	SubscriptionCreditsUSD   string    `json:"subscriptionCreditsUsd"`
	RechargeCreditsUSD       string    `json:"rechargeCreditsUsd"`
	TotalAvailableCreditsUSD string    `json:"totalAvailableCreditsUsd"`
	BalanceUSD               string    `json:"balanceUsd"`
	TodayConsumedUSD         string    `json:"todayConsumedUsd"`
	TotalConsumedUSD         string    `json:"totalConsumedUsd"`
	TotalRechargedUSD        string    `json:"totalRechargedUsd"`
	UsageCount               int64     `json:"usageCount"`
	RechargeCount            int64     `json:"rechargeCount"`
	CreditsPerUSD            int64     `json:"creditsPerUsd"`
}

type rawSummary struct {
	Summary
	Balances *struct {
		SubscriptionCredits   any `json:"subscriptionCredits"`
		RechargeCredits       any `json:"rechargeCredits"`
		TotalAvailableCredits any `json:"totalAvailableCredits"`
	} `json:"balances"`
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case json.Number:
		return x.String()
	}
	return ""
}

func (c *Client) Summary(ctx context.Context, key, workspace string) (Summary, error) {
	var raw rawSummary
	if err := c.get(ctx, key, "/api/v1/billing/summary", workspace, &raw); err != nil {
		return Summary{}, err
	}
	s := raw.Summary
	if raw.Balances != nil {
		s.SubscriptionCredits = str(raw.Balances.SubscriptionCredits)
		s.RechargeCredits = str(raw.Balances.RechargeCredits)
		s.TotalAvailableCredits = str(raw.Balances.TotalAvailableCredits)
	}
	return s, nil
}

type Wallet struct {
	BalanceUSD string `json:"balanceUsd"`
}

func (c *Client) Wallet(ctx context.Context, key, workspace string) (Wallet, error) {
	var w Wallet
	err := c.get(ctx, key, "/api/v1/workspaces/"+url.PathEscape(workspace)+"/wallet/balance", workspace, &w)
	if err == nil && strings.TrimSpace(w.BalanceUSD) == "" {
		err = errors.New("balanceUsd missing")
	}
	return w, err
}

// ParseUSD reads a decimal USD string; ok is false for a missing value.
func ParseUSD(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil
}

// FetchCatalog lists the models a runtime key may call, from the key's own
// /v1/models: the catalogue carries plan entitlement (a model above the
// key's plan is absent or `enabled: false`), which is what the official
// client gates its model picker on. It costs nothing.
func FetchCatalog(ctx context.Context, hc *http.Client, runtimeBase, runtimeKey string) (map[string]bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, runtimeBase+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+runtimeKey)
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{Status: resp.StatusCode}
	}
	var body struct {
		Data []struct {
			ID      string `json:"id"`
			Enabled *bool  `json:"enabled"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("invalid models JSON")
	}
	out := make(map[string]bool, len(body.Data))
	for _, m := range body.Data {
		if m.ID != "" {
			out[m.ID] = m.Enabled == nil || *m.Enabled
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty model catalogue")
	}
	return out, nil
}
