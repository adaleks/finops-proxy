package wrapper

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ProxyClient is a minimal stdlib client for the proxy's control-plane route
// DELETE /v1/agent/{id}/history.
type ProxyClient struct {
	baseURL    string
	httpc      *http.Client
	maxRetries int
	backoff    time.Duration
}

// NewProxyClient builds a ProxyClient for the given proxy base URL, HTTP
// timeout, and retry count (negative retries are clamped to zero).
func NewProxyClient(proxyURL string, timeout time.Duration, maxRetries int) *ProxyClient {
	if maxRetries < 0 {
		maxRetries = 0
	}
	return &ProxyClient{
		baseURL:    proxyURL,
		httpc:      &http.Client{Timeout: timeout},
		maxRetries: maxRetries,
		backoff:    500 * time.Millisecond,
	}
}

// buildURL returns {base}/v1/agent/{PathEscape(id)}/history.
func (c *ProxyClient) buildURL(agentID string) string {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return c.baseURL + "/v1/agent/" + url.PathEscape(agentID) + "/history"
	}
	u.Path = "/v1/agent/" + url.PathEscape(agentID) + "/history"
	u.RawQuery = ""
	return u.String()
}

// DeleteHistory clears one agent's loop history on the proxy. It is idempotent:
// 2xx and 404 (already empty) both mean success. Transient failures (network,
// 5xx) are retried with backoff; after exhausting retries it returns the last
// error so the caller can log it and continue (fail-open).
func (c *ProxyClient) DeleteHistory(ctx context.Context, agentID string) error {
	target := c.buildURL(agentID)
	var lastErr error

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, attempt); err != nil {
				return err
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, nil)
		if err != nil {
			return err
		}
		resp, err := c.httpc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()

		switch {
		case resp.StatusCode < 300 || resp.StatusCode == http.StatusNotFound:
			return nil
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("proxy returned status %d", resp.StatusCode)
			continue
		default:
			return fmt.Errorf("proxy returned status %d (unexpected)", resp.StatusCode)
		}
	}
	return lastErr
}

func (c *ProxyClient) sleep(ctx context.Context, attempt int) error {
	delay := c.backoff << uint(attempt-1)
	if delay > 8*time.Second {
		delay = 8 * time.Second
	}
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
