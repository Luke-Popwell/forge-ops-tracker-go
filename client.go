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

// DeliverSpans posts one whole captured trace: {"trace_id": ..., "spans": [...]}, unlike
// DeliverPerformanceSamples' batch over a time window. One trace, one POST.
func (c *Client) DeliverSpans(trace map[string]any) bool {
	return c.post(c.configuration.SpansURI(), trace)
}

// DeliverMetrics posts a batch of individual CaptureMetric entries as {"metrics": [...]}.
func (c *Client) DeliverMetrics(entries []map[string]any) bool {
	return c.post(c.configuration.CustomMetricsURI(), map[string]any{"metrics": entries})
}

// DeliverInfrastructureMetrics posts a batch of infrastructure readings the same way.
func (c *Client) DeliverInfrastructureMetrics(entries []map[string]any) bool {
	return c.post(c.configuration.InfrastructureMetricsURI(), map[string]any{"metrics": entries})
}

// DeliverChange posts one RecordChange payload to "/changes".
func (c *Client) DeliverChange(change map[string]any) bool {
	return c.post(c.configuration.ChangesURI(), change)
}

// DeliverChangeSnapshot posts the startup change snapshot to "/change_snapshots".
func (c *Client) DeliverChangeSnapshot(snapshot map[string]any) bool {
	return c.post(c.configuration.ChangeSnapshotsURI(), snapshot)
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
