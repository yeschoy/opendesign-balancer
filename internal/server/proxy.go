package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"opendesign-balancer/internal/config"
	"opendesign-balancer/internal/pool"
	"opendesign-balancer/internal/rules"
	"opendesign-balancer/internal/stats"
)

// ---- log records: strict projections, never a request, account or error object ----

type attemptLog struct {
	Account        string `json:"account"`
	AccountName    string `json:"accountName"`
	Status         int    `json:"status"`
	UpstreamStatus *int   `json:"upstreamStatus"`
	Ms             int64  `json:"ms"`
	Action         string `json:"action,omitempty"`
	RuleID         string `json:"ruleId,omitempty"`
	Egress         string `json:"egress"`
	Terminal       string `json:"terminal"`
}

type workflowDecision struct {
	Account  string   `json:"account"`
	Reason   string   `json:"reason"`
	Binding  string   `json:"binding"`
	Overflow bool     `json:"overflow"`
	Order    []string `json:"order,omitempty"`
}

type requestLog struct {
	TS                 time.Time          `json:"ts"`
	RequestID          string             `json:"requestId"`
	ClientKeyID        string             `json:"clientKeyId,omitempty"`
	ClientKeyName      string             `json:"clientKeyName,omitempty"`
	Group              string             `json:"group,omitempty"`
	Path               string             `json:"path"`
	InboundProtocol    string             `json:"inboundProtocol"`
	Protocol           string             `json:"protocol"`
	Model              string             `json:"model"`
	Stream             bool               `json:"stream"`
	AccountID          string             `json:"accountId,omitempty"`
	AccountName        string             `json:"accountName,omitempty"`
	Proxy              string             `json:"proxy,omitempty"` // egress label: proxy host:port or "direct"
	SelectionReason    string             `json:"selectionReason,omitempty"`
	Selector           string             `json:"selector"`
	Overflow           bool               `json:"overflow"`
	Switched           bool               `json:"switched"`
	SessionSource      string             `json:"sessionSource"`
	AffinityKeyType    string             `json:"affinityKeyType"`
	AffinityConfidence string             `json:"affinityConfidence"`
	BindingResult      string             `json:"bindingResult"`
	Status             int                `json:"status"`
	Result             string             `json:"result"`
	UpstreamStatus     *int               `json:"upstreamStatus"`
	ErrorCategory      string             `json:"errorCategory,omitempty"`
	BlockedBy          string             `json:"blockedBy,omitempty"`
	RetryAfter         *int               `json:"retryAfter,omitempty"`
	DurationMs         int64              `json:"durationMs"`
	TTFTMs             *int64             `json:"ttftMs"`
	InputTokens        *int64             `json:"inputTokens"`
	OutputTokens       *int64             `json:"outputTokens"`
	CachedTokens       *int64             `json:"cachedTokens"`
	CacheHit           *bool              `json:"cacheHit"`
	CacheRate          *float64           `json:"cacheRate"`      // cached share of the prompt, 0..1
	Cost               *float64           `json:"cost,omitempty"` // in Currency, from the price table
	Currency           string             `json:"currency,omitempty"`
	UpstreamRequestID  string             `json:"upstreamRequestId,omitempty"` // x-request-id of the upstream response that was delivered
	Attempts           []attemptLog       `json:"attempts"`
	Workflow           []workflowDecision `json:"workflow,omitempty"`
}

type errorLog struct {
	TS                  time.Time `json:"ts"`
	RequestID           string    `json:"requestId"`
	AttemptIndex        int       `json:"attemptIndex"`
	ClientKeyID         string    `json:"clientKeyId,omitempty"`
	ClientKeyName       string    `json:"clientKeyName,omitempty"`
	AccountID           string    `json:"accountId"`
	AccountName         string    `json:"accountName"`
	Model               string    `json:"model"`
	Status              int       `json:"status"`
	UpstreamStatus      *int      `json:"upstreamStatus"`
	Category            string    `json:"category"`
	Terminal            string    `json:"terminal"`
	Reason              string    `json:"reason"`
	ReasonTruncated     bool      `json:"reasonTruncated"`
	RuleID              string    `json:"ruleId,omitempty"`
	RuleScope           string    `json:"ruleScope,omitempty"`
	RuleAction          string    `json:"ruleAction,omitempty"`
	MatchedBy           string    `json:"matchedBy"`
	RetryRuleID         string    `json:"retryRuleId,omitempty"`
	RetryDecision       string    `json:"retryDecision,omitempty"`
	HealthAction        string    `json:"healthAction"`
	RetryAfterMs        *int64    `json:"retryAfterMs,omitempty"`
	Switched            bool      `json:"switched"`
	ResponseContentType string    `json:"responseContentType,omitempty"`
	ResponseBytes       int       `json:"responseBytes"`
}

// cacheRate is the cached share of the prompt. Messages usage reports input
// without the cache reads; Chat and Responses usage includes them.
func cacheRate(in, cached *int64, anthropic bool) *float64 {
	if in == nil || cached == nil {
		return nil
	}
	total := *in
	if anthropic {
		total += *cached
	}
	if total <= 0 {
		return nil
	}
	r := min(float64(*cached)/float64(total), 1)
	return &r
}

// LogKeys are the logstore filter keys (logstore.Filters), as it would read
// them back from the JSON.
func (r *requestLog) LogKeys() map[string]string {
	return map[string]string{"requestId": r.RequestID, "accountId": r.AccountID, "status": strconv.Itoa(r.Status),
		"result": r.Result, "model": r.Model, "errorCategory": r.ErrorCategory, "bindingResult": r.BindingResult,
		"clientKeyId": r.ClientKeyID, "stream": strconv.FormatBool(r.Stream), "protocol": r.Protocol,
		"affinityKeyType": r.AffinityKeyType, "affinityConfidence": r.AffinityConfidence}
}

func (e errorLog) LogKeys() map[string]string {
	return map[string]string{"requestId": e.RequestID, "accountId": e.AccountID, "status": strconv.Itoa(e.Status),
		"category": e.Category, "ruleId": e.RuleID, "model": e.Model, "clientKeyId": e.ClientKeyID}
}

// ---- attempt results ----

type outcome int

const (
	outDelivered outcome = iota // a response went to the client
	outFailed                   // nothing written: may switch accounts
	outCancelled                // the client went away
)

type usage struct {
	in, out, cached *int64
}

type attemptResult struct {
	kind           outcome
	result         string // success | failed, for delivered
	status         int
	upstreamStatus int
	category       string // upstream | network | timeout | proxy
	terminal       string // success | upstream_http | upstream_envelope | timeout | proxy | network
	body           []byte
	header         http.Header
	contentType    string
	sent           bool
	wrapped        bool
	accountScoped  bool
	retryAfter     time.Duration
	// downstreamStatus / ruleID come from the matched error rule
	downstreamStatus int
	ruleID           string
	ttft             *int64
	usage            usage
	ms               int64
}

// setUpstreamRequestID exposes the upstream's own request id so downstream gateways can quote it to the provider.
func setUpstreamRequestID(dst, upstream http.Header) {
	if id := upstream.Get("X-Request-Id"); id != "" {
		dst.Set("X-Upstream-Request-Id", id)
	}
}

func newRequestID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "req_" + hex.EncodeToString(b)
}

func clientKeyFrom(r *http.Request) string {
	if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	if k := r.Header.Get("X-Api-Key"); k != "" {
		return strings.TrimSpace(k)
	}
	return strings.TrimSpace(r.Header.Get("X-Goog-Api-Key"))
}

func (s *Server) authClient(cfg *config.Config, r *http.Request) (config.ClientKey, bool) {
	k := clientKeyFrom(r)
	if k == "" {
		return config.ClientKey{}, false
	}
	return cfg.ClientKeyByHash(config.HashKey(k))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeLocalError answers in the inbound protocol's error shape.
func writeLocalError(w http.ResponseWriter, proto string, status int, typ, code, msg string, retryAfter int) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	if proto == "anthropic" {
		writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": msg}})
		return
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}})
}

func (s *Server) handleV1(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" && r.Method == http.MethodGet {
		s.handleModels(w, r)
		return
	}
	if r.Method != http.MethodPost {
		writeLocalError(w, "openai", http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "method not allowed", 0)
		return
	}
	s.handleProxy(w, r)
}

func ceilSeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	cfg := s.cfg.Get() // one snapshot for the whole request
	rid := newRequestID()
	proto := "openai"
	if strings.HasPrefix(r.URL.Path, "/v1/messages") {
		proto = "anthropic"
	}
	var cw *captureWriter
	var reqHeader http.Header
	var reqBody []byte
	if cfg.Logs.Detail {
		cw = &captureWriter{ResponseWriter: w, max: cfg.Logs.DetailBodyKB << 10}
		w, reqHeader = cw, r.Header.Clone()
	}
	w.Header().Set("X-Request-Id", rid)
	rec := &requestLog{TS: start.UTC(), RequestID: rid, Path: r.URL.Path, InboundProtocol: proto, Protocol: protocolOf(r.URL.Path), Selector: cfg.Routing.Strategy,
		SessionSource: "none", AffinityKeyType: "none", AffinityConfidence: "none", BindingResult: "not-applicable", Attempts: []attemptLog{}}
	var res *attemptResult
	finish := func(status int, result string) {
		rec.Status, rec.Result = status, result
		rec.DurationMs = time.Since(start).Milliseconds()
		if res != nil {
			rec.InputTokens, rec.OutputTokens, rec.CachedTokens = res.usage.in, res.usage.out, res.usage.cached
			if res.usage.cached != nil {
				hit := *res.usage.cached > 0
				rec.CacheHit = &hit
			}
			rec.TTFTMs = res.ttft
			if res.upstreamStatus != 0 {
				us := res.upstreamStatus
				rec.UpstreamStatus = &us
			}
		}
		anthropic := proto == "anthropic"
		rec.CacheRate = cacheRate(rec.InputTokens, rec.CachedTokens, anthropic)
		var cost float64
		if c, cur, ok := cfg.Pricing.Cost(rec.Model, rec.TS, rec.InputTokens, rec.CachedTokens, rec.OutputTokens, anthropic); ok {
			cost, rec.Cost, rec.Currency = c, &c, cur
		}
		s.logs.Append("requests", rec.TS, rec)
		if cw != nil {
			max := cw.max
			d := &detailLog{TS: rec.TS, RequestID: rid, Protocol: rec.Protocol, Method: r.Method, Path: r.URL.Path, Model: rec.Model,
				ClientKeyID: rec.ClientKeyID, ClientKeyName: rec.ClientKeyName, AccountID: rec.AccountID, Status: status, RequestHeaders: maskHeaders(cfg, reqHeader), RequestBodyBytes: len(reqBody),
				ResponseHeaders: maskHeaders(cfg, cw.Header()), ResponseBodyBytes: cw.n}
			d.RequestBody, d.RequestBodyTruncated = maskBody(cfg, reqBody, max)
			respBody, cut := cw.body()
			d.ResponseBody, _ = maskBody(cfg, respBody, len(respBody))
			d.ResponseBodyTruncated = cut
			s.logs.Append("details", rec.TS, d)
		}
		var prompt int64
		if rec.InputTokens != nil {
			prompt = *rec.InputTokens
			if anthropic && rec.CachedTokens != nil {
				prompt += *rec.CachedTokens
			}
		}
		s.stats.Record(stats.Sample{AccountID: rec.AccountID, Proxy: rec.Proxy, Model: rec.Model, Result: result, Input: rec.InputTokens, Output: rec.OutputTokens,
			Cached: rec.CachedTokens, Prompt: prompt, Currency: rec.Currency, Cost: cost})
		s.statsDirty.Store(true)
	}

	ck, ok := s.authClient(cfg, r)
	if !ok {
		rec.ErrorCategory = "auth"
		writeLocalError(w, proto, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "invalid API key", 0)
		finish(http.StatusUnauthorized, "failed")
		return
	}
	rec.ClientKeyID, rec.ClientKeyName, rec.Group = ck.ID, ck.Name, ck.Group
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, int64(cfg.Routing.MaxBodyMB)<<20))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeLocalError(w, proto, http.StatusRequestEntityTooLarge, "invalid_request_error", "body_too_large", "request body too large", 0)
			rec.ErrorCategory = "request"
			finish(http.StatusRequestEntityTooLarge, "failed")
			return
		}
		finish(499, "client_cancelled")
		return
	}
	reqBody = body
	obj, err := parseRequestBody(body)
	if err != nil {
		rec.ErrorCategory = "request"
		writeLocalError(w, proto, http.StatusBadRequest, "invalid_request_error", "invalid_json", "request body must be a JSON object", 0)
		finish(http.StatusBadRequest, "failed")
		return
	}
	rec.Model, _ = obj["model"].(string)
	rec.Stream, _ = obj["stream"].(bool)
	s.quota.NoteModel(rec.Model) // learn whether it is a Design Plan model on each account
	ident := s.identity(cfg, ck.Group, r.Header, obj)
	rec.SessionSource, rec.AffinityKeyType, rec.AffinityConfidence = ident.Source, ident.KeyType, ident.Confidence

	excluded := map[string]bool{}
	var last *attemptResult
	strippedTools := 0
	for i := 0; i < cfg.Routing.MaxAccounts; i++ {
		wait := time.Duration(cfg.Routing.ConcurrencyWaitMs) * time.Millisecond
		if i > 0 {
			wait = 0 // a switch probes the others without waiting
		}
		lease, err := s.pool.Acquire(r.Context(), cfg, pool.Request{
			Group: ck.Group, Model: rec.Model, RequestID: rid,
			Affinity: ident.Fingerprint, Explicit: ident.Confidence == "explicit",
			Excluded: excluded, Wait: wait,
		})
		if err != nil {
			if r.Context().Err() != nil {
				finish(499, "client_cancelled")
				return
			}
			if last != nil {
				break // nobody else to switch to: answer with the last failure
			}
			var ce *pool.CapacityError
			var ue *pool.UnavailableError
			switch {
			case errors.As(err, &ce):
				ra := ceilSeconds(ce.RetryAfter)
				rec.ErrorCategory, rec.BlockedBy, rec.RetryAfter = "capacity", ce.BlockedBy, &ra
				writeLocalError(w, proto, http.StatusTooManyRequests, "rate_limit_error", "pool_capacity", "all upstream accounts are busy, retry later", ra)
				finish(http.StatusTooManyRequests, "failed")
			case errors.As(err, &ue) && ue.RetryAfter > 0:
				// every account is only cooling down: a retryable 429, not a 503
				ra := ceilSeconds(ue.RetryAfter)
				rec.ErrorCategory, rec.BlockedBy, rec.RetryAfter = "unavailable", "cooldown", &ra
				writeLocalError(w, proto, http.StatusTooManyRequests, "rate_limit_error", "pool_cooling_down", "all upstream accounts are cooling down for this model, retry later", ra)
				finish(http.StatusTooManyRequests, "failed")
			case errors.As(err, &ue) && ue.Details["not-entitled"] > 0 && ue.Details["not-entitled"] == sumCounts(ue.Details):
				rec.ErrorCategory, rec.BlockedBy = "unavailable", "not-entitled"
				writeLocalError(w, proto, http.StatusServiceUnavailable, "api_error", "model_not_entitled", "model "+rec.Model+" is not included in any upstream account's plan", 0)
				finish(http.StatusServiceUnavailable, "failed")
			case errors.As(err, &ue):
				rec.ErrorCategory, rec.BlockedBy = "unavailable", "unavailable"
				writeLocalError(w, proto, http.StatusServiceUnavailable, "api_error", "pool_unavailable", "no upstream account is available ("+ue.Reason+")", 0)
				finish(http.StatusServiceUnavailable, "failed")
			default:
				writeLocalError(w, proto, http.StatusServiceUnavailable, "api_error", "pool_unavailable", "no upstream account is available", 0)
				finish(http.StatusServiceUnavailable, "failed")
			}
			return
		}
		d := lease.Decision
		acct := lease.Account
		if i == 0 {
			rec.BindingResult = d.BindingResult
		}
		if len(rec.Workflow) < 4 {
			order := d.Order
			if len(order) > 8 {
				order = order[:8]
			}
			rec.Workflow = append(rec.Workflow, workflowDecision{Account: acct.ID, Reason: d.Reason, Binding: d.BindingResult, Overflow: d.Overflow, Order: order})
		}
		rec.AccountID, rec.AccountName, rec.SelectionReason, rec.Overflow = acct.ID, acct.Name, d.Reason, d.Overflow
		rec.Proxy = egressLabel(acct.ProxyURL)

		res = s.attempt(w, r, cfg, lease, body, proto, rec, i+1, start, ck.Group)
		lease.Release()
		att := attemptLog{Account: acct.ID, AccountName: acct.Name, Status: res.status, Ms: res.ms, Egress: "direct", Terminal: res.terminal}
		if acct.ProxyURL != "" {
			att.Egress = "proxy"
		}
		if res.upstreamStatus != 0 {
			us := res.upstreamStatus
			att.UpstreamStatus = &us
		}
		switch res.kind {
		case outDelivered:
			rec.UpstreamRequestID = res.header.Get("X-Request-Id")
			rec.Attempts = append(rec.Attempts, att)
			if res.result == "success" {
				s.pool.Sample(acct.ID, true)
				s.pool.Succeeded(cfg, acct.ID, rec.Model)
			} else {
				s.pool.Sample(acct.ID, false)
				rec.ErrorCategory = res.category
			}
			var tok int64
			for _, p := range []*int64{res.usage.in, res.usage.out} {
				if p != nil {
					tok += *p
				}
			}
			s.pool.AddTokens(acct.ID, tok)
			finish(res.status, res.result)
			return
		case outCancelled:
			rec.Attempts = append(rec.Attempts, att)
			res.status = 499
			finish(499, "client_cancelled")
			return
		}
		ev := s.evaluate(cfg, acct, rec.Model, res, rid, i, rec.ClientKeyID, rec.ClientKeyName)
		att.Action, att.RuleID = ev.action, ev.ruleID
		rec.Attempts = append(rec.Attempts, att)
		last = res
		if res.status == http.StatusBadRequest && strippedTools < 3 {
			// the upstream refuses a hosted tool (e.g. web_search) on this route:
			// drop that tool type and ask the same account again
			if nb, ok := stripUnsupportedTool(body, res.body); ok {
				body = nb
				strippedTools++
				i--
				continue
			}
		}
		if !ev.switchAccount || i+1 >= cfg.Routing.MaxAccounts {
			break
		}
		excluded[acct.ID] = true
		rec.Switched = true
	}
	res = last
	status := s.writeFailure(w, cfg, proto, last, len(rec.Attempts))
	rec.ErrorCategory = last.category
	if status == http.StatusTooManyRequests {
		ra := ceilSeconds(last.retryAfter)
		rec.RetryAfter = &ra
	}
	finish(status, "failed")
}

var unsupportedToolRe = regexp.MustCompile(`unsupported_tool\][^"]*?unsupported \w+ tool type \\?"([A-Za-z0-9_.-]+)\\?"`)

// stripUnsupportedTool removes every tool of the type an upstream
// unsupported_tool 400 names from the request body. Clients such as Codex
// add hosted tools (web_search) that the chat route of non-OpenAI models
// rejects, while the rest of the request is fine.
func stripUnsupportedTool(body, errBody []byte) ([]byte, bool) {
	m := unsupportedToolRe.FindSubmatch(errBody)
	if m == nil {
		return nil, false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		return nil, false
	}
	var tools []json.RawMessage
	if json.Unmarshal(obj["tools"], &tools) != nil {
		return nil, false
	}
	kept := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		var ty struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(t, &ty) == nil && ty.Type == string(m[1]) {
			continue
		}
		kept = append(kept, t)
	}
	if len(kept) == len(tools) {
		return nil, false
	}
	if len(kept) == 0 {
		delete(obj, "tools")
		delete(obj, "tool_choice")
		delete(obj, "parallel_tool_calls")
	} else {
		obj["tools"], _ = json.Marshal(kept)
	}
	nb, err := json.Marshal(obj)
	return nb, err == nil
}

// wrappedStatus finds a deterministic request error an upstream wrapped in
// a 5xx, so it is answered as the 400/422 it is and cools nothing down.
func wrappedStatus(lowerBody string) int {
	for _, m := range []string{"status code 422", `"status":422`, `"status": 422`, "unprocessable entity"} {
		if strings.Contains(lowerBody, m) {
			return 422
		}
	}
	for _, m := range []string{"invalid_request_error", "status code 400", `"status":400`, `"status": 400`, `"code":400`, "bad request", "invalid_argument"} {
		if strings.Contains(lowerBody, m) {
			return 400
		}
	}
	return 0
}

type evaluation struct {
	switchAccount bool
	action        string
	ruleID        string
}

// evaluate applies the error rules to a failed attempt that wrote nothing
// to the client, logs it, and says whether to switch accounts.
func (s *Server) evaluate(cfg *config.Config, acct config.Account, model string, res *attemptResult, rid string, idx int, keyID, keyName string) evaluation {
	now := time.Now()
	reason, truncated := errorReason(cfg, res.body, res.contentType)
	redacted := redactSecrets(cfg, clip(string(res.body), 16<<10))
	matchText := strings.ToLower(redacted)
	el := errorLog{TS: now.UTC(), RequestID: rid, AttemptIndex: idx, ClientKeyID: keyID, ClientKeyName: keyName, AccountID: acct.ID, AccountName: acct.Name, Model: model,
		Status: res.status, Category: res.category, Terminal: res.terminal, Reason: reason, ReasonTruncated: truncated,
		MatchedBy: "default", HealthAction: "degrade", ResponseContentType: strings.SplitN(res.contentType, ";", 2)[0], ResponseBytes: len(res.body)}
	if res.upstreamStatus != 0 {
		us := res.upstreamStatus
		el.UpstreamStatus = &us
	}
	if res.terminal != "upstream_http" && res.terminal != "upstream_envelope" {
		el.Reason = res.terminal + " error"
		if res.category == "proxy" {
			el.Reason = "proxy connection failed"
		}
	}
	ev := evaluation{}
	sample := true
	switch res.terminal {
	case "upstream_http", "upstream_envelope":
		st := res.status
		if st >= 500 {
			if ws := wrappedStatus(matchText); ws != 0 {
				res.status, res.wrapped, res.category = ws, true, "request"
				el.Status, el.MatchedBy, el.HealthAction, el.Category = ws, "wrapped-request-error", "none", "request"
				s.logs.Append("errors", el.TS, el)
				return ev
			}
		}
		retry := rules.MatchRetry(cfg.RetryRules, st, matchText, res.header)
		if retry != nil {
			el.RetryRuleID, el.RetryDecision = retry.ID, retry.Decision
		}
		rule := rules.MatchError(cfg.ErrorRules, st, redacted, res.header)
		if c, r := membershipLimits(matchText); c > 0 || r > 0 {
			s.pool.Learn(acct.ID, c, r)
		}
		if rule == nil {
			if st >= 400 && st < 500 && st != 401 && st != 402 && st != 403 && st != 429 {
				sample, el.HealthAction = false, "none" // the request's fault, not the account's
				res.category = "request"
				el.Category = "request"
			}
		} else {
			el.MatchedBy, el.RuleID, el.RuleScope, el.RuleAction = "rule", rule.ID, rule.Scope, rule.Action
			ev.action, ev.ruleID = rule.Action, rule.ID
			res.downstreamStatus, res.ruleID = rule.DownstreamStatus, rule.ID
			switch rule.Action {
			case rules.ActionIgnore:
				sample, el.HealthAction = false, "none"
			case rules.ActionCooldown:
				// what the cooldown covers: the account, one model, or a funding class
				target, class := "account", ""
				switch {
				case rule.Scope == rules.ScopeFunding && model != "":
					class = s.pool.Class(cfg, model)
					switch class {
					case "plan":
						target = "account" // windows, 套餐模型额度 and wallet all empty
					case "package":
						target = "class"
					default:
						target = "model"
					}
				case rule.Scope == rules.ScopeModel && model != "":
					target = "model"
				}
				strikes := 1
				if rule.Reset != nil && rule.Reset.Escalate {
					key := "account"
					switch target {
					case "class":
						key = class
					case "model":
						key = "model:" + model
					default:
						if class == "plan" {
							key = "plan"
						}
					}
					strikes = s.pool.Strike(acct.ID, key)
				}
				d := rules.CooldownAfter(rule, res.header, redacted, now, strikes)
				ms := d.Milliseconds()
				el.RetryAfterMs = &ms
				el.RuleScope = rule.Scope + ":" + target
				res.retryAfter = d
				switch target {
				case "model":
					s.pool.Cooldown(acct.ID, model, now.Add(d), st, rule.ID, rule.ID)
				case "class":
					s.pool.ClassCooldown(acct.ID, class, now.Add(d), st, rule.ID, rule.ID)
				default:
					s.pool.Cooldown(acct.ID, "", now.Add(d), st, rule.ID, rule.ID)
					res.accountScoped = true
				}
				ev.switchAccount = true
			case rules.ActionHardQuarantine:
				s.pool.Quarantine(acct.ID, st, rule.ID, rule.ID)
				res.accountScoped = true
				ev.switchAccount = true
			}
			if rule.RefreshQuota {
				s.quota.Refresh(acct.ID)
			}
		}
		if st == 401 {
			ev.switchAccount = true
			res.accountScoped = true
		}
		// an error a rule pins on the account (cooldown, quarantine) is not the
		// request's fault, so a retry rule for its status doesn't stop the switch
		if retry != nil && !(rule != nil && (rule.Action == rules.ActionCooldown || rule.Action == rules.ActionHardQuarantine)) {
			ev.switchAccount = false
		} else if retry != nil {
			el.RetryDecision = "overridden-by-error-rule"
		}
	case "proxy", "network":
		// never sent: another account can take it without double billing
		ev.switchAccount = !res.sent
	}
	if sample {
		s.pool.Sample(acct.ID, false)
	}
	if res.retryAfter == 0 && res.header != nil {
		if v := res.header.Get("Retry-After"); v != "" {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				res.retryAfter = time.Duration(n) * time.Second
			}
		}
	}
	el.Switched = ev.switchAccount
	s.logs.Append("errors", el.TS, el)
	return ev
}

// writeFailure answers the client for a request whose last attempt failed
// before writing anything. Account problems never reach the client as
// 401/402/403: a relay such as new-api would disable the whole channel.
func (s *Server) writeFailure(w http.ResponseWriter, cfg *config.Config, proto string, res *attemptResult, attempts int) int {
	w.Header().Set("X-Attempts", strconv.Itoa(attempts))
	st := res.status
	switch {
	case res.terminal == "timeout":
		writeLocalError(w, proto, http.StatusGatewayTimeout, "api_error", "upstream_timeout", "upstream timed out", 0)
		return http.StatusGatewayTimeout
	case res.terminal == "network" || res.terminal == "proxy":
		writeLocalError(w, proto, http.StatusBadGateway, "api_error", "upstream_connection_failed", "upstream connection failed", 0)
		return http.StatusBadGateway
	case res.wrapped:
		// pass the deterministic request error through
	case res.downstreamStatus != 0:
		ds := res.downstreamStatus
		ra, typ, msg := 0, "api_error", "upstream account unavailable for this request"
		switch {
		case ds == http.StatusTooManyRequests:
			ra, typ, msg = ceilSeconds(res.retryAfter), "rate_limit_error", "upstream account limit reached, retry later"
		case ds == http.StatusBadGateway:
			msg = "upstream service unavailable"
		}
		writeLocalError(w, proto, ds, typ, strings.ReplaceAll(res.ruleID, "-", "_"), msg, ra)
		return ds
	case st == 401 || st == 403:
		writeLocalError(w, proto, http.StatusBadGateway, "api_error", "upstream_account_auth_failed", "upstream account authentication failed", 0)
		return http.StatusBadGateway
	case st == 429:
		ra := ceilSeconds(res.retryAfter)
		writeLocalError(w, proto, http.StatusTooManyRequests, "rate_limit_error", "upstream_rate_limited", "upstream is rate limited, retry later", ra)
		return http.StatusTooManyRequests
	case st == 402 || (res.accountScoped && st >= 400 && st < 500):
		writeLocalError(w, proto, http.StatusServiceUnavailable, "api_error", "upstream_account_unavailable", "upstream account temporarily unavailable", 0)
		return http.StatusServiceUnavailable
	}
	ct := res.contentType
	if ct == "" || res.terminal == "upstream_envelope" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(st)
	_, _ = w.Write(res.body)
	return st
}

// ---- upstream attempt ----

var skipRequestHeaders = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true, "proxy-connection": true,
	"te": true, "trailer": true, "transfer-encoding": true, "upgrade": true, "host": true, "content-length": true,
	"authorization": true, "x-api-key": true, "api-key": true, "x-goog-api-key": true, "cookie": true,
	"accept-encoding": true, "forwarded": true, "x-real-ip": true, "true-client-ip": true, "x-request-id": true,
}

func copyRequestHeaders(dst, src http.Header) {
	for k, vs := range src {
		lk := strings.ToLower(k)
		if skipRequestHeaders[lk] || strings.HasPrefix(lk, "x-forwarded-") || strings.HasPrefix(lk, "cf-") {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func isProxyError(err error, proxyURL string) bool {
	if proxyURL == "" {
		return false
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "proxyconnect" {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "proxyconnect") || strings.Contains(msg, "socks")
}

func (s *Server) attempt(w http.ResponseWriter, r *http.Request, cfg *config.Config, lease *pool.Lease, body []byte, proto string, rec *requestLog, attemptNo int, start time.Time, group string) *attemptResult {
	acct := lease.Account
	res := &attemptResult{}
	t0 := time.Now()
	defer func() { res.ms = time.Since(t0).Milliseconds() }()

	actx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var timedOut atomic.Bool
	limit := time.Duration(cfg.Routing.RequestSec) * time.Second
	if rec.Stream {
		limit = time.Duration(cfg.Routing.FirstEventSec) * time.Second
	}
	timer := time.AfterFunc(limit, func() { timedOut.Store(true); cancel() })
	defer timer.Stop()

	var sentOnce sync.Once
	markSent := func() { sentOnce.Do(func() { res.sent = true; lease.MarkSent() }) }
	trace := &httptrace.ClientTrace{WroteRequest: func(i httptrace.WroteRequestInfo) {
		if i.Err == nil {
			markSent()
		}
	}}
	target := cfg.RuntimeBase(acct) + strings.TrimPrefix(r.URL.Path, "/v1")
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(actx, trace), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		res.kind, res.status, res.terminal, res.category = outFailed, http.StatusBadGateway, "network", "network"
		return res
	}
	copyRequestHeaders(req.Header, r.Header)
	req.Header.Set("Authorization", "Bearer "+acct.RuntimeKey)
	if proto == "anthropic" {
		req.Header.Set("X-Api-Key", acct.RuntimeKey)
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.ContentLength = int64(len(body))

	resp, err := s.httpClient(acct.ProxyURL).Do(req)
	if err != nil {
		res.kind = outFailed
		switch {
		case r.Context().Err() != nil:
			res.kind = outCancelled
		case timedOut.Load():
			res.status, res.terminal, res.category = http.StatusGatewayTimeout, "timeout", "timeout"
		case isProxyError(err, acct.ProxyURL):
			res.status, res.terminal, res.category = http.StatusBadGateway, "proxy", "proxy"
		default:
			res.status, res.terminal, res.category = http.StatusBadGateway, "network", "network"
		}
		return res
	}
	defer resp.Body.Close()
	markSent()
	res.upstreamStatus = resp.StatusCode
	res.header = resp.Header
	ct := resp.Header.Get("Content-Type")
	res.contentType = ct

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if r.Context().Err() != nil {
			res.kind = outCancelled
			return res
		}
		res.kind, res.status, res.body, res.terminal, res.category = outFailed, resp.StatusCode, b, "upstream_http", "upstream"
		return res
	}
	isResponses := strings.HasPrefix(r.URL.Path, "/v1/responses")
	if strings.HasPrefix(strings.ToLower(ct), "text/event-stream") {
		return s.relayStream(w, r, cfg, lease, resp, res, cancel, timer, &timedOut, attemptNo, start, isResponses, group)
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		res.kind = outFailed
		switch {
		case r.Context().Err() != nil:
			res.kind = outCancelled
		case timedOut.Load():
			res.status, res.terminal, res.category = http.StatusGatewayTimeout, "timeout", "timeout"
		default:
			res.status, res.terminal, res.category = http.StatusBadGateway, "network", "network"
		}
		return res
	}
	var m map[string]any
	if json.Unmarshal(b, &m) == nil && m != nil {
		if isEnvelopeError(m) {
			res.kind, res.status, res.body, res.terminal, res.category = outFailed, errorStatus(m, b), b, "upstream_envelope", "upstream"
			return res
		}
		scanUsage(m, &res.usage)
		if isResponses {
			if id, _ := m["id"].(string); id != "" {
				s.pool.Bind(cfg, s.responseFingerprint(group, id), acct.ID)
			}
		}
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("X-Attempts", strconv.Itoa(attemptNo))
	setUpstreamRequestID(h, resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(b)
	res.kind, res.result, res.status, res.terminal = outDelivered, "success", resp.StatusCode, "success"
	return res
}

func isEnvelopeError(m map[string]any) bool {
	if m["error"] == nil {
		return m["type"] == "error"
	}
	return m["choices"] == nil && m["output"] == nil && m["content"] == nil
}

// errorStatus reads a status out of an error envelope or SSE error event.
func errorStatus(m map[string]any, raw []byte) int {
	num := func(v any) int {
		switch x := v.(type) {
		case float64:
			if x >= 400 && x <= 599 {
				return int(x)
			}
		case string:
			if n, err := strconv.Atoi(x); err == nil && n >= 400 && n <= 599 {
				return n
			}
		}
		return 0
	}
	if n := num(m["status"]); n != 0 {
		return n
	}
	if n := num(m["status_code"]); n != 0 {
		return n
	}
	for _, k := range []string{"error", "response"} {
		if e, ok := m[k].(map[string]any); ok {
			for _, f := range []string{"status", "status_code", "code"} {
				if n := num(e[f]); n != 0 {
					return n
				}
			}
			if inner, ok := e["error"].(map[string]any); ok {
				if n := num(inner["code"]); n != 0 {
					return n
				}
			}
		}
	}
	lower := strings.ToLower(string(raw))
	switch {
	case strings.Contains(lower, "insufficient_balance"):
		return 402
	case strings.Contains(lower, "model_limit_exceeded"), strings.Contains(lower, "rate_limit"):
		return 429
	case strings.Contains(lower, "invalid_api_key"), strings.Contains(lower, "authentication_error"):
		return 401
	case strings.Contains(lower, "tier_model_not_entitled"), strings.Contains(lower, "permission"):
		return 403
	case strings.Contains(lower, "invalid_request"), strings.Contains(lower, "invalid_model"):
		return 400
	}
	return 502
}

func intField(m map[string]any, k string) *int64 {
	if f, ok := m[k].(float64); ok {
		v := int64(f)
		return &v
	}
	return nil
}

// scanUsage reads OpenAI Chat, Responses and Anthropic usage shapes; a
// later event overrides only the fields it carries.
func scanUsage(m map[string]any, u *usage) {
	var us map[string]any
	if x, ok := m["usage"].(map[string]any); ok {
		us = x
	} else if r, ok := m["response"].(map[string]any); ok {
		us, _ = r["usage"].(map[string]any)
	} else if msg, ok := m["message"].(map[string]any); ok {
		us, _ = msg["usage"].(map[string]any)
	}
	if us == nil {
		return
	}
	set := func(dst **int64, v *int64) {
		if v != nil {
			*dst = v
		}
	}
	set(&u.in, intField(us, "prompt_tokens"))
	set(&u.in, intField(us, "input_tokens"))
	set(&u.out, intField(us, "completion_tokens"))
	set(&u.out, intField(us, "output_tokens"))
	set(&u.cached, intField(us, "cache_read_input_tokens"))
	for _, k := range []string{"prompt_tokens_details", "input_tokens_details"} {
		if d, ok := us[k].(map[string]any); ok {
			set(&u.cached, intField(d, "cached_tokens"))
		}
	}
}

// ---- SSE ----

const maxEventBytes = 32 << 20

var errEventTooLarge = errors.New("SSE event too large")

// readEvent reads one SSE event: lines up to a blank line, raw.
func readEvent(rd *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		line, err := rd.ReadSlice('\n')
		buf = append(buf, line...)
		if len(buf) > maxEventBytes {
			return nil, errEventTooLarge
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return buf, err
		}
		if len(line) == 1 || (len(line) == 2 && line[0] == '\r') {
			if len(buf) == len(line) {
				buf = buf[:0] // a stray blank line
				continue
			}
			return buf, nil
		}
	}
}

func sseFields(ev []byte) (event string, data []byte, real bool) {
	for _, line := range bytes.Split(ev, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			event = strings.TrimSpace(string(line[6:]))
			real = true
		case bytes.HasPrefix(line, []byte("data:")):
			d := line[5:]
			if len(d) > 0 && d[0] == ' ' {
				d = d[1:]
			}
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, d...)
			real = true
		case len(line) > 0 && line[0] != ':':
			real = true
		}
	}
	return
}

// sseError says whether an event (its sseFields) is an error event, with
// its status and JSON payload. Only an event that names an error is decoded.
func sseError(event string, data []byte) (bool, int, []byte) {
	isErr := event == "error" || event == "response.failed"
	if !isErr && !bytes.Contains(data, []byte(`"error"`)) && !bytes.Contains(data, []byte("response.failed")) {
		return false, 0, nil
	}
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	if m != nil {
		if m["error"] != nil && m["choices"] == nil {
			isErr = true
		}
		if t, _ := m["type"].(string); t == "error" || t == "response.failed" {
			isErr = true
		}
	}
	if !isErr {
		return false, 0, nil
	}
	return true, errorStatus(m, data), data
}

// hasUsage says whether data has a "usage" member that isn't null: OpenAI
// chunks streamed with include_usage carry "usage":null until the last one.
func hasUsage(data []byte) bool {
	key := []byte(`"usage"`)
	for rest := data; ; {
		i := bytes.Index(rest, key)
		if i < 0 {
			return false
		}
		rest = rest[i+len(key):]
		v := bytes.TrimLeft(rest, " \t\r\n")
		if len(v) == 0 || v[0] != ':' {
			continue
		}
		if !bytes.HasPrefix(bytes.TrimLeft(v[1:], " \t\r\n"), []byte("null")) {
			return true
		}
	}
}

// isTerminalEvent says whether an SSE event ends a stream: OpenAI's
// [DONE], Anthropic's message_stop, the Responses API's response.completed.
func isTerminalEvent(event string, data []byte) bool {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return true
	}
	return event == "message_stop" || event == "response.completed"
}

type evMsg struct {
	ev  []byte
	err error
}

func (s *Server) relayStream(w http.ResponseWriter, r *http.Request, cfg *config.Config, lease *pool.Lease, resp *http.Response, res *attemptResult,
	cancel context.CancelFunc, timer *time.Timer, timedOut *atomic.Bool, attemptNo int, start time.Time, isResponses bool, group string) *attemptResult {
	rd := bufio.NewReaderSize(resp.Body, 64<<10)
	events := make(chan evMsg, 16)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			ev, err := readEvent(rd)
			select {
			case events <- evMsg{ev, err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	readFailure := func(err error) *attemptResult {
		res.kind = outFailed
		switch {
		case r.Context().Err() != nil:
			res.kind = outCancelled
		case timedOut.Load():
			res.status, res.terminal, res.category = http.StatusGatewayTimeout, "timeout", "timeout"
		default:
			res.status, res.terminal, res.category = http.StatusBadGateway, "network", "network"
		}
		return res
	}

	// hold everything until the first complete event proves not to be an error
	var first, firstData []byte
	var firstEvent string
	var firstErr error
	for first == nil {
		m := <-events
		if event, data, real := sseFields(m.ev); real && (m.err == nil || m.err == io.EOF) {
			first, firstEvent, firstData, firstErr = m.ev, event, data, m.err
			break
		}
		if m.err != nil {
			return readFailure(m.err)
		}
	}
	timer.Stop()
	if isErr, st, payload := sseError(firstEvent, firstData); isErr {
		res.kind, res.status, res.body, res.terminal, res.category = outFailed, st, payload, "upstream_envelope", "upstream"
		res.contentType = "application/json"
		return res
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Attempts", strconv.Itoa(attemptNo))
	setUpstreamRequestID(h, resp.Header)
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	ttft := time.Since(start).Milliseconds()
	res.ttft = &ttft
	res.kind, res.status, res.result, res.terminal = outDelivered, http.StatusOK, "success", "success"

	clientGone := func() *attemptResult {
		cancel()
		res.kind, res.status, res.result = outCancelled, 499, "client_cancelled"
		return res
	}
	scan := func(event string, data []byte) {
		if !hasUsage(data) && !(isResponses && event == "response.created") && !bytes.Contains(data, []byte(`"response.created"`)) {
			return
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			return
		}
		scanUsage(m, &res.usage)
		if isResponses {
			if t, _ := m["type"].(string); t == "response.created" || event == "response.created" {
				if rr, ok := m["response"].(map[string]any); ok {
					if id, _ := rr["id"].(string); id != "" {
						s.pool.Bind(cfg, s.responseFingerprint(group, id), lease.Account.ID)
					}
				}
			}
		}
	}
	write := func(b []byte) bool {
		if _, err := w.Write(b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !write(first) {
		return clientGone()
	}
	scan(firstEvent, firstData)
	if firstErr == io.EOF {
		return res // the whole stream was one event
	}

	idleDur := time.Duration(cfg.Routing.IdleSec) * time.Second
	idle := time.NewTimer(idleDur)
	defer idle.Stop()
	hb := time.NewTicker(time.Duration(cfg.Routing.HeartbeatSec) * time.Second)
	defer hb.Stop()
	for {
		select {
		case m := <-events:
			if len(m.ev) > 0 {
				event, data, _ := sseFields(m.ev)
				done := isTerminalEvent(event, data)
				if !write(m.ev) {
					if done {
						return res // the client had what it needs and hung up first
					}
					return clientGone()
				}
				scan(event, data)
				if isErr, _, _ := sseError(event, data); isErr {
					res.result, res.category = "failed", "upstream"
				}
				if done {
					// Clients close as soon as they see the last event; don't wait
					// for the upstream EOF, or that close reads as a cancellation.
					return res
				}
				idle.Reset(idleDur)
			}
			if m.err == io.EOF {
				return res
			}
			if m.err != nil {
				if r.Context().Err() != nil {
					return clientGone()
				}
				res.result, res.terminal = "failed", "network"
				res.category = "network"
				if timedOut.Load() {
					res.terminal, res.category = "timeout", "timeout"
				}
				return res
			}
		case <-hb.C:
			if !write([]byte(": PING\n\n")) {
				return clientGone()
			}
		case <-idle.C:
			timedOut.Store(true)
			cancel()
		case <-r.Context().Done():
			return clientGone()
		}
	}
}

// ---- /v1/models ----

func sumCounts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// handleModels lists the upstream catalogue, narrowed to the models some
// account of the caller's group may call (each key's own catalogue), so a
// relay such as new-api only offers what this pool can serve.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Get()
	ck, ok := s.authClient(cfg, r)
	if !ok {
		writeLocalError(w, "openai", http.StatusUnauthorized, "authentication_error", "invalid_api_key", "invalid API key", 0)
		return
	}
	body := s.upstreamModels(r.Context(), cfg, ck.Group)
	if body == nil {
		writeLocalError(w, "openai", http.StatusBadGateway, "api_error", "models_unavailable", "could not list upstream models", 0)
		return
	}
	allowed, known := map[string]bool{}, false
	if !cfg.Routing.IgnoreCatalog {
		now := time.Now()
		for _, a := range cfg.Accounts {
			if a.Group != ck.Group || !a.Enabled {
				continue
			}
			if ms := s.quota.View(a.ID).Models(now); ms != nil {
				known = true
				for _, m := range ms {
					allowed[m] = true
				}
			}
		}
	}
	if known {
		var doc map[string]any
		if json.Unmarshal(body, &doc) == nil {
			if data, ok := doc["data"].([]any); ok {
				kept := make([]any, 0, len(data))
				for _, it := range data {
					if m, ok := it.(map[string]any); ok {
						if id, _ := m["id"].(string); allowed[id] {
							kept = append(kept, it)
						}
					}
				}
				doc["data"] = kept
				if b, err := json.Marshal(doc); err == nil {
					body = b
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

type modelsEntry struct {
	body []byte
	at   time.Time
}

// modelsCall is one in-flight upstream read that concurrent misses share.
type modelsCall struct {
	done chan struct{}
	body []byte
}

// resetModels drops the cached catalogues (the upstream settings changed);
// a read already in flight then isn't cached.
func (s *Server) resetModels() {
	s.models.Lock()
	s.models.byGroup = nil
	s.models.gen++
	s.models.Unlock()
}

// upstreamModels is the raw /v1/models of an account of group, cached per
// group; concurrent misses wait for one upstream read.
func (s *Server) upstreamModels(ctx context.Context, cfg *config.Config, group string) []byte {
	s.models.Lock()
	if e, ok := s.models.byGroup[group]; ok && time.Since(e.at) < time.Duration(cfg.Routing.ModelsCacheSec)*time.Second {
		s.models.Unlock()
		return e.body
	}
	if c := s.models.inflight[group]; c != nil {
		s.models.Unlock()
		select {
		case <-c.done:
			return c.body
		case <-ctx.Done():
			return nil
		}
	}
	c := &modelsCall{done: make(chan struct{})}
	if s.models.inflight == nil {
		s.models.inflight = map[string]*modelsCall{}
	}
	s.models.inflight[group] = c
	gen := s.models.gen
	s.models.Unlock()

	// the read outlives a caller that leaves: others may be waiting on it
	c.body = s.fetchModels(context.WithoutCancel(ctx), cfg, group)
	s.models.Lock()
	delete(s.models.inflight, group)
	if c.body != nil && gen == s.models.gen {
		if s.models.byGroup == nil {
			s.models.byGroup = map[string]modelsEntry{}
		}
		s.models.byGroup[group] = modelsEntry{body: c.body, at: time.Now()}
	}
	s.models.Unlock()
	close(c.done)
	return c.body
}

func (s *Server) fetchModels(ctx context.Context, cfg *config.Config, group string) []byte {
	order, _ := s.pool.Preview(cfg, group, "")
	for _, id := range order {
		a, ok := cfg.Account(id)
		if !ok {
			continue
		}
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, _ := http.NewRequestWithContext(c, http.MethodGet, cfg.RuntimeBase(a)+"/models", nil)
		req.Header.Set("Authorization", "Bearer "+a.RuntimeKey)
		req.Header.Set("Accept", "application/json")
		resp, err := s.httpClient(a.ProxyURL).Do(req)
		if err != nil {
			cancel()
			continue
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		cancel()
		if err != nil || resp.StatusCode != http.StatusOK || !json.Valid(b) {
			continue
		}
		return b
	}
	return nil
}

var (
	concLimitRe = regexp.MustCompile(`concurrency limit exceeded\s*(\d+)\s*/\s*(\d+)`)
	rpmLimitRe  = regexp.MustCompile(`\brpm(?: limit exceeded)?\s*:?\s*(\d+)\s*/\s*(\d+)`)
)

// membershipLimits reads the limits a tier_limit_exceeded refusal names:
// "membership concurrency limit exceeded 6/5" → concurrency 5, "rpm 61/60"
// → 60. lower is the lowercased body.
func membershipLimits(lower string) (conc, rpm int) {
	if !strings.Contains(lower, "tier_limit_exceeded") && !strings.Contains(lower, "limit exceeded") {
		return 0, 0
	}
	if m := concLimitRe.FindStringSubmatch(lower); m != nil {
		conc, _ = strconv.Atoi(m[2])
	}
	if m := rpmLimitRe.FindStringSubmatch(lower); m != nil {
		rpm, _ = strconv.Atoi(m[2])
	}
	return conc, rpm
}

// egressLabel names an account's outbound path without its credentials.
func egressLabel(proxyURL string) string {
	if proxyURL == "" {
		return "direct"
	}
	if u, err := url.Parse(proxyURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "proxy"
}
