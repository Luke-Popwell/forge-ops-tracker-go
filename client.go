package forgeops

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// Client delivers one payload over HTTP. Every failure mode: DNS, connection, timeout, TLS, a
// non-2xx response: is caught here and turned into a false return rather than a propagated
// error, since a broken or unreachable tracker must never be able to break the host app. Ported
// from gems/forge_ops_tracker/lib/forge_ops_tracker/client.rb.
//
// Uses only net/http, not a third-party HTTP client: same reason the Ruby gem uses plain
// Net::HTTP rather than a gem dependency: this has to work in any host app without adding a
// dependency of its own.
type Client struct {
	configuration *Configuration
	httpClient    *http.Client
}

func NewClient(configuration *Configuration) *Client {
	return &Client{
		configuration: configuration,
		httpClient:    &http.Client{Timeout: configuration.Timeout},
	}
}

func (c *Client) Deliver(payload map[string]any) bool {
	return c.post(c.configuration.IngestionURI(), payload)
}

// DeliverPerformanceSamples posts a batch of aggregate performance samples (one entry per
// distinct transaction a flush interval saw, see PerformanceFlusher), not a single payload the
// way Deliver's own is.
func (c *Client) DeliverPerformanceSamples(samples []map[string]any) bool {
	return c.post(c.configuration.PerformanceSamplesURI(), map[string]any{"samples": samples})
}

func (c *Client) post(uri string, payload map[string]any) bool {
	if uri == "" {
		return false
	}

	body, err := json.Marshal(payload)
	if err != nil {
		c.configuration.Logger.Debugf("delivery failed: %v", err)
		return false
	}

	request, err := http.NewRequest(http.MethodPost, uri, bytes.NewReader(body))
	if err != nil {
		c.configuration.Logger.Debugf("delivery failed: %v", err)
		return false
	}
	request.Header.Set("Authorization", "Bearer "+c.configuration.APIKey())
	request.Header.Set("Content-Type", "application/json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		c.configuration.Logger.Debugf("delivery failed: %v", err)
		return false
	}
	defer response.Body.Close()
	// Drain the body so the underlying connection can be reused by the next delivery rather than
	// forcing http.Client to open a fresh one every time.
	_, _ = io.Copy(io.Discard, response.Body)

	return response.StatusCode >= 200 && response.StatusCode < 300
}
