package server

import (
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

func maskBody(cfg *config.Config, b []byte, max int) (string, bool) {
	cut := len(b) > max
	if cut {
		b = b[:max]
	}
	return redactSecrets(cfg, strings.ToValidUTF8(string(b), "�")), cut
}

// captureWriter keeps the status and the first max bytes of what goes to the client.
type captureWriter struct {
	http.ResponseWriter
	max    int
	status int
	buf    []byte
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
	if room := c.max - len(c.buf); room > 0 {
		c.buf = append(c.buf, b[:min(room, len(b))]...)
	}
	n, err := c.ResponseWriter.Write(b)
	c.n += n
	return n, err
}

// Unwrap lets http.NewResponseController reach the real writer to flush.
func (c *captureWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
