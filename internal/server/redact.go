package server

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"opendesign-balancer/internal/config"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`sk-amr-[A-Za-z0-9_-]+`),
	regexp.MustCompile(`vela_ctrl_[A-Za-z0-9_-]+`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`(?i)(socks5h?|https?)://[^\s"'/@]+:[^\s"'/@]+@[^\s"']+`),
	regexp.MustCompile(`__Secure-better-auth\.session_token=[^;\s"]+`),
}

// redactSecrets replaces every known account key and proxy URL and every
// credential-looking token in text.
func redactSecrets(cfg *config.Config, text string) string {
	if text == "" {
		return text
	}
	var pairs []string
	for _, a := range cfg.Accounts {
		for _, v := range []string{a.RuntimeKey, a.ControlKey, a.ProxyURL} {
			if len(v) >= 8 {
				pairs = append(pairs, v, "[REDACTED]")
			}
		}
	}
	if len(pairs) > 0 {
		text = strings.NewReplacer(pairs...).Replace(text)
	}
	for _, re := range secretPatterns {
		text = re.ReplaceAllString(text, "[REDACTED]")
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
