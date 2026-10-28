package server

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"

	"opendesign-balancer/internal/config"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`sk-amr-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`vela_ctrl_[A-Za-z0-9_-]+`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`\brv_[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`(?i)(socks5h?|https?)://[^\s"'/@]+:[^\s"'/@]+@[^\s"']+`),
	regexp.MustCompile(`__Secure-better-auth\.session_token=[^;\s"]+`),
}

// secretHints are cheap substrings: a text with none of them (case-folded)
// can't match any secretPatterns entry, so the regexes are skipped.
var secretHints = []string{"bearer", "sk-", "rv_", "vela_ctrl_", "://", "__secure-better-auth"}

type replacerCache struct {
	cfg *config.Config
	r   *strings.Replacer // nil: no account secrets to replace
}

var lastReplacer atomic.Pointer[replacerCache]

// accountReplacer is the replacer for cfg's account secrets, rebuilt only
// when the config snapshot changes.
func accountReplacer(cfg *config.Config) *strings.Replacer {
	if c := lastReplacer.Load(); c != nil && c.cfg == cfg {
		return c.r
	}
	var pairs []string
	for _, a := range cfg.Accounts {
		for _, v := range []string{a.RuntimeKey, a.ControlKey, a.ProxyURL} {
			if len(v) >= 8 {
				pairs = append(pairs, v, "[REDACTED]")
			}
		}
	}
	c := &replacerCache{cfg: cfg}
	if len(pairs) > 0 {
		c.r = strings.NewReplacer(pairs...)
	}
	lastReplacer.Store(c)
	return c.r
}

// redactSecrets replaces every known account key and proxy URL and every
// credential-looking token in text.
func redactSecrets(cfg *config.Config, text string) string {
	if text == "" {
		return text
	}
	if r := accountReplacer(cfg); r != nil {
		text = r.Replace(text)
	}
	lower := strings.ToLower(text)
	for _, h := range secretHints {
		if strings.Contains(lower, h) {
			for _, re := range secretPatterns {
				text = re.ReplaceAllString(text, "[REDACTED]")
			}
			break
		}
	}
	return text
}

// errorReason is the redacted, bounded reason an error log keeps: a JSON
// body itself (16 KiB at most), anything else only its size.
func errorReason(cfg *config.Config, body []byte, contentType string) (reason string, truncated bool) {
	if len(body) == 0 {
		return "empty error body", false
	}
	trim := strings.TrimSpace(string(body))
	isJSON := json.Valid([]byte(trim))
	if !isJSON && (strings.HasPrefix(trim, "data:") || strings.HasPrefix(trim, "event:")) {
		isJSON = true // a failing SSE event: keep it, it is the provider's own error text
	}
	if !isJSON {
		return fmt.Sprintf("non-JSON error body (%s, %d bytes)", strings.SplitN(contentType, ";", 2)[0], len(body)), false
	}
	r := redactSecrets(cfg, trim)
	if len(r) > 16<<10 {
		return clip(r, 16<<10), true
	}
	return r, false
}
