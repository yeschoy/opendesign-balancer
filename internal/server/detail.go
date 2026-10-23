package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"opendesign-balancer/internal/config"
)

// protocolOf names the inbound API protocol by request path.
func protocolOf(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1/messages"):
		return "anthropic-messages"
	case strings.HasPrefix(path, "/v1/responses"):
		return "openai-responses"
	case strings.HasPrefix(path, "/v1/chat/completions"):
		return "openai-chat"
	case strings.HasPrefix(path, "/v1/completions"):
		return "openai-completions"
	case strings.HasPrefix(path, "/v1/embeddings"):
		return "openai-embeddings"
	}
	return "openai"
}

// detailLog is one request's wire-level record (logs.detail). Credentials
// are masked; bodies are cut to logs.detailBodyKB.
type detailLog struct {
	TS                    time.Time           `json:"ts"`
	RequestID             string              `json:"requestId"`
	Protocol              string              `json:"protocol"`
	Method                string              `json:"method"`
	Path                  string              `json:"path"`
	Model                 string              `json:"model"`
	AccountID             string              `json:"accountId,omitempty"`
	Status                int                 `json:"status"`
	RequestHeaders        map[string][]string `json:"requestHeaders"`
	RequestBody           string              `json:"requestBody"`
	RequestBodyBytes      int                 `json:"requestBodyBytes"`
	RequestBodyTruncated  bool                `json:"requestBodyTruncated"`
	ResponseHeaders       map[string][]string `json:"responseHeaders"`
	ResponseBody          string              `json:"responseBody"`
	ResponseBodyBytes     int                 `json:"responseBodyBytes"`
	ResponseBodyTruncated bool                `json:"responseBodyTruncated"`
}

func (d *detailLog) LogKeys() map[string]string {
	return map[string]string{"requestId": d.RequestID, "accountId": d.AccountID, "status": strconv.Itoa(d.Status),
		"model": d.Model, "protocol": d.Protocol}
}

var secretHeaders = map[string]bool{"authorization": true, "x-api-key": true, "api-key": true, "x-goog-api-key": true,
	"cookie": true, "set-cookie": true, "proxy-authorization": true}

func maskHeaders(cfg *config.Config, h http.Header) map[string][]string {
	out := make(map[string][]string, len(h))
	for k, vs := range h {
		cp := make([]string, len(vs))
		for i, v := range vs {
			if secretHeaders[strings.ToLower(k)] {
				cp[i] = "[REDACTED]"
			} else {
				cp[i] = redactSecrets(cfg, v)
			}
		}
		out[k] = cp
	}
	return out
}

// clipMiddle keeps the first and last max/2 bytes of b: a long chat's end
// (the latest turn and the answer) matters as much as its start.
func clipMiddle(b []byte, max int) ([]byte, bool) {
	if len(b) <= max {
		return b, false
	}
	h := max / 2
	out := append([]byte(nil), b[:h]...)
	out = append(out, fmt.Sprintf("\n…[省略 %d 字节]…\n", len(b)-2*h)...)
	return append(out, b[len(b)-h:]...), true
}

func maskBody(cfg *config.Config, b []byte, max int) (string, bool) {
	b, cut := clipMiddle(b, max)
	return redactSecrets(cfg, strings.ToValidUTF8(string(b), "\uFFFD")), cut
}

// captureWriter keeps the status and the first and last max/2 bytes of what goes to the client.
type captureWriter struct {
	http.ResponseWriter
	max    int
	status int
	head   []byte
	tail   []byte
	n      int
}

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	h := c.max / 2
	rest := b
	if room := h - len(c.head); room > 0 {
		k := min(room, len(rest))
		c.head, rest = append(c.head, rest[:k]...), rest[k:]
	}
	if len(rest) > 0 {
		c.tail = append(c.tail, rest...)
		if len(c.tail) > 2*h {
			c.tail = append(c.tail[:0], c.tail[len(c.tail)-h:]...)
		}
	}
	n, err := c.ResponseWriter.Write(b)
	c.n += n
	return n, err
}

// body is what was captured, with the omitted middle marked.
func (c *captureWriter) body() ([]byte, bool) {
	h := c.max / 2
	tail := c.tail
	if len(tail) > h {
		tail = tail[len(tail)-h:]
	}
	if len(c.head)+len(tail) >= c.n {
		return append(append([]byte(nil), c.head...), tail...), false
	}
	out := append([]byte(nil), c.head...)
	out = append(out, fmt.Sprintf("\n…[省略 %d 字节]…\n", c.n-len(c.head)-len(tail))...)
	return append(out, tail...), true
}

// Unwrap lets http.NewResponseController reach the real writer to flush.
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
