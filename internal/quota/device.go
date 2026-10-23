package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// DeviceStart is a new device authorization. DeviceSecret stays in the
// process and is never written to disk, logs or the admin UI.
type DeviceStart struct {
	DeviceID            string    `json:"deviceId"`
	DeviceSecret        string    `json:"deviceSecret"`
	UserCode            string    `json:"userCode"`
	ActivationURL       string    `json:"activationUrl"`
	PollIntervalSeconds int       `json:"pollIntervalSeconds"`
	ExpiresAt           time.Time `json:"expiresAt"`
	Status              string    `json:"status"`
}

type DeviceToken struct {
	Status     string `json:"status"`
	ControlKey string `json:"controlKey"`
	RuntimeKey string `json:"runtimeKey"`
	User       *struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("network error")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error any `json:"error"`
		}
		code := ""
		if json.Unmarshal(data, &e) == nil {
			if s, ok := e.Error.(string); ok {
				code = codeRe(s)
			}
		}
		return &HTTPError{Status: resp.StatusCode, Code: code}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("invalid JSON")
	}
	return nil
}

func (c *Client) StartDevice(ctx context.Context, host string) (DeviceStart, error) {
	var d DeviceStart
	err := c.post(ctx, "/api/v1/cli/device-authorizations",
		map[string]string{"cliVersion": "0.1.4", "host": host, "profile": "prod"}, &d)
	if err == nil && (d.DeviceID == "" || d.DeviceSecret == "") {
		err = fmt.Errorf("device authorization incomplete")
	}
	return d, err
}

func (c *Client) PollDevice(ctx context.Context, deviceID, secret string) (DeviceToken, error) {
	var t DeviceToken
	err := c.post(ctx, "/api/v1/cli/device-authorizations/"+url.PathEscape(deviceID)+"/token",
		map[string]string{"deviceSecret": secret}, &t)
	return t, err
}
