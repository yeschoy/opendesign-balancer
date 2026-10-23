package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"opendesign-balancer/internal/config"
)

// Identity is the conversation a request belongs to. Only the enum fields
// are ever logged; the raw value and the fingerprint never are.
type Identity struct {
	Source      string // header | body | message_hmac | none
	KeyType     string // session_id, parent_session, prompt_cache_key, previous_response_id, …
	Confidence  string // explicit | fallback | none
	Fingerprint string
}

// Headers new-api passes through, in priority order: parents first so a
// parent agent and its sub-agents land on one account.
var parentHeaders = []string{"X-Parent-Session-Id", "X-Parent-Session-Affinity", "X-Codex-Parent-Thread-Id", "X-Claude-Code-Parent-Agent-Id"}

var sessionHeaders = []struct{ name, keyType string }{
	{"X-Claude-Code-Session-Id", "session_id"},
	{"Session-Id", "session_id"},
	{"Session_id", "session_id"},
	{"X-Session-Id", "session_id"},
	{"X-Session-Affinity", "session_id"},
	{"Thread-Id", "thread_id"},
	{"X-Thread-Id", "thread_id"},
	{"Conversation-Id", "conversation_id"},
	{"Conversation_id", "conversation_id"},
	{"X-Conversation-Id", "conversation_id"},
}

var bodyKeys = []struct{ path, keyType string }{
	{"prompt_cache_key", "prompt_cache_key"},
	{"session_id", "session_id"},
	{"conversation_id", "conversation_id"},
	{"thread_id", "thread_id"},
	{"metadata.session_id", "session_id"},
	{"metadata.conversation_id", "conversation_id"},
	{"metadata.thread_id", "thread_id"},
}

func validValue(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" || len(v) > 512 {
		return "", false
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", false
		}
	}
	return v, true
}

// headerValue reads a header by its exact (non-canonical) name too:
// Go canonicalizes "Session_id" differently from what clients send.
func headerValue(h http.Header, name string) string {
	if v := h.Get(name); v != "" {
		return v
	}
	if v, ok := h[name]; ok && len(v) > 0 {
		return v[0]
	}
	lower := strings.ToLower(name)
	for k, v := range h {
		if strings.ToLower(k) == lower && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

func lookup(obj map[string]any, path string) string {
	cur := any(obj)
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[part]
	}
	s, _ := cur.(string)
	return s
}

var claudeSessionRe = regexp.MustCompile(`_session_([0-9a-fA-F-]{8,})`)

// claudeUserSession pulls the session out of Claude Code's metadata.user_id,
// either "user_<h>_account_<u>_session_<s>" or a JSON object string.
func claudeUserSession(uid string) string {
	if uid == "" {
		return ""
	}
	if strings.HasPrefix(uid, "{") {
		var m map[string]any
		if json.Unmarshal([]byte(uid), &m) == nil {
			if s, ok := m["session_id"].(string); ok {
				return s
			}
		}
		return ""
	}
	if m := claudeSessionRe.FindStringSubmatch(uid); m != nil {
		return m[1]
	}
	return ""
}

func (s *Server) fingerprint(group, kind, value string) string {
	mac := hmac.New(sha256.New, s.secret)
	mac.Write([]byte(kind + "\x00" + group + "\x00" + value))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Server) responseFingerprint(group, responseID string) string {
	return s.fingerprint(group, "response", responseID)
}

// identity finds the conversation of a request: a Responses
// previous_response_id first (the upstream keeps it on one account), then
// configured and parent headers, body fields, session headers, and as a
// fallback an HMAC of the first system and user messages.
func (s *Server) identity(cfg *config.Config, group string, h http.Header, body map[string]any) Identity {
	if v, ok := validValue(lookup(body, "previous_response_id")); ok {
		return Identity{Source: "body", KeyType: "previous_response_id", Confidence: "explicit", Fingerprint: s.responseFingerprint(group, v)}
	}
	explicit := func(source, keyType, v string) Identity {
		return Identity{Source: source, KeyType: keyType, Confidence: "explicit", Fingerprint: s.fingerprint(group, "session", v)}
	}
	for _, name := range cfg.Routing.SessionHeaders {
		if v, ok := validValue(headerValue(h, name)); ok {
			return explicit("header", "custom_header", v)
		}
	}
	for _, name := range parentHeaders {
		if v, ok := validValue(headerValue(h, name)); ok {
			return explicit("header", "parent_session", v)
		}
	}
	for _, k := range bodyKeys {
		if v, ok := validValue(lookup(body, k.path)); ok {
			return explicit("body", k.keyType, v)
		}
	}
	if v, ok := validValue(claudeUserSession(lookup(body, "metadata.user_id"))); ok {
		return explicit("body", "claude_user_session", v)
	}
	for _, sh := range sessionHeaders {
		if v, ok := validValue(headerValue(h, sh.name)); ok {
			return explicit("header", sh.keyType, v)
		}
	}
	if cfg.Routing.FallbackHash {
		if stable := stableConversation(body); stable != "" {
			return Identity{Source: "message_hmac", KeyType: "message_hmac", Confidence: "fallback", Fingerprint: s.fingerprint(group, "messages", stable)}
		}
	}
	return Identity{Source: "none", KeyType: "none", Confidence: "none"}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// contentText flattens a message content (string or blocks) to its text.
func contentText(c any) string {
	switch x := c.(type) {
	case string:
		return x
	case []any:
		var b strings.Builder
		for _, part := range x {
			if m, ok := part.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					b.WriteString(t)
					b.WriteByte('\n')
				}
			}
		}
		return b.String()
	}
	return ""
}

// stableConversation is the first system/developer text and the first user
// text (each clipped to 4096 bytes) across Chat, Messages and Responses bodies.
func stableConversation(body map[string]any) string {
	var sys, user string
	if v, ok := body["system"]; ok {
		sys = contentText(v)
	}
	if v, ok := body["instructions"].(string); ok && sys == "" {
		sys = v
	}
	scan := func(items []any) {
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			switch role {
			case "system", "developer":
				if sys == "" {
					sys = contentText(m["content"])
				}
			case "user":
				if user == "" {
					user = contentText(m["content"])
				}
			}
			if sys != "" && user != "" {
				return
			}
		}
	}
	if msgs, ok := body["messages"].([]any); ok {
		scan(msgs)
	}
	switch in := body["input"].(type) {
	case string:
		if user == "" {
			user = in
		}
	case []any:
		scan(in)
	}
	if sys == "" && user == "" {
		return ""
	}
	out, _ := json.Marshal([2]string{clip(sys, 4096), clip(user, 4096)})
	return string(out)
}

// bodyKeysKept are the top-level fields identity and routing read; the rest
// of a request body (tools, the long message history) is never decoded.
var bodyKeysKept = map[string]bool{"model": true, "stream": true, "previous_response_id": true, "prompt_cache_key": true,
	"session_id": true, "conversation_id": true, "thread_id": true, "metadata": true, "system": true, "instructions": true}

// parseRequestBody decodes just what routing needs from a request body: the
// fields in bodyKeysKept, plus the leading messages/input items up to the
// first system and first user message (what stableConversation reads).
// The rest is only skipped over, not decoded, so a long history costs a scan
// and no allocations. Only the decoded parts are validated; a malformed
// remainder is left for the upstream to reject.
func parseRequestBody(b []byte) (map[string]any, error) {
	errBad := errors.New("not a JSON object")
	i := skipSpace(b, 0)
	if i >= len(b) || b[i] != '{' {
		return nil, errBad
	}
	obj := make(map[string]any, len(bodyKeysKept)+2)
	i = skipSpace(b, i+1)
	for i < len(b) && b[i] != '}' {
		if b[i] != '"' {
			return nil, errBad
		}
		ke := skipString(b, i)
		if ke < 0 {
			return nil, errBad
		}
		var key string
		if json.Unmarshal(b[i:ke], &key) != nil {
			return nil, errBad
		}
		i = skipSpace(b, ke)
		if i >= len(b) || b[i] != ':' {
			return nil, errBad
		}
		vs := skipSpace(b, i+1)
		ve := skipValue(b, vs)
		if ve < 0 {
			return nil, errBad
		}
		switch {
		case bodyKeysKept[key]:
			var x any
			if json.Unmarshal(b[vs:ve], &x) != nil {
				return nil, errBad
			}
			obj[key] = x
		case key == "messages" || key == "input":
			obj[key] = leadingItems(b[vs:ve])
		}
		i = skipSpace(b, ve)
		if i < len(b) && b[i] == ',' {
			i = skipSpace(b, i+1)
		} else if i >= len(b) || b[i] != '}' {
			return nil, errBad
		}
	}
	if i >= len(b) {
		return nil, errBad
	}
	return obj, nil
}

// leadingItems decodes a messages/input value: a string stays one, an array
// is decoded item by item until a system/developer and a user item are found.
func leadingItems(v []byte) any {
	if len(v) == 0 {
		return nil
	}
	if v[0] == '"' {
		var s string
		_ = json.Unmarshal(v, &s)
		return s
	}
	if v[0] != '[' {
		return nil
	}
	out := make([]any, 0, 4)
	var sys, user bool
	i := skipSpace(v, 1)
	for i < len(v) && v[i] != ']' {
		e := skipValue(v, i)
		if e < 0 {
			break
		}
		var x any
		if json.Unmarshal(v[i:e], &x) == nil {
			out = append(out, x)
			if m, ok := x.(map[string]any); ok {
				switch m["role"] {
				case "system", "developer":
					sys = sys || contentText(m["content"]) != ""
				case "user":
					user = user || contentText(m["content"]) != ""
				}
			}
		}
		if sys && user {
			break
		}
		i = skipSpace(v, e)
		if i < len(v) && v[i] == ',' {
			i = skipSpace(v, i+1)
		}
	}
	return out
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\n' || b[i] == '\t' || b[i] == '\r') {
		i++
	}
	return i
}

// skipString returns the index after the string starting at b[i] == '"', or -1.
func skipString(b []byte, i int) int {
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return -1
}

// skipValue returns the index after the JSON value starting at b[i], or -1.
func skipValue(b []byte, i int) int {
	if i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				if j = skipString(b, j) - 1; j < 0 {
					return -1
				}
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return j + 1
				}
			}
		}
		return -1
	}
	j := i
	for j < len(b) && b[j] != ',' && b[j] != '}' && b[j] != ']' && b[j] != ' ' && b[j] != '\n' && b[j] != '\t' && b[j] != '\r' {
		j++
	}
	if j == i {
		return -1
	}
	return j
}
