package forgeops

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// Client delivers one payload over HTTP. Every failure mode: DNS, connection, timeout, TLS, a
// non-2xx response: is caught here and turned into a false return (or, for the batch senders, a
// deliveryOutcome) rather than a propagated error, since a broken or unreachable tracker must
// never be able to break the host app. Ported
// from gems/forge_ops_tracker/lib/forge_ops_tracker/client.rb.
//
// Uses only net/http, not a third-party HTTP client: same reason the Ruby gem uses plain
// Net::HTTP rather than a gem dependency: this has to work in any host app without adding a
// dependency of its own.
type Client struct {
	configuration *Configuration
	httpClient    *http.Client
}

// deliveryOutcome is what became of one POST, as far as whoever is holding the payload needs to
// know: whether to let go of it, and whether sending it again could ever help.
type deliveryOutcome int

const (
	// deliveryDelivered: any 2xx.
	deliveryDelivered deliveryOutcome = iota
	// deliveryRejected: a 4xx other than 408 and 429 (a bad payload, a revoked key, a plan without
	// the feature), or a payload that can't be encoded at all. The same payload will never be
	// accepted, so it must not be sent again.
	deliveryRejected
	// deliveryFailed: a network error, a timeout, a 5xx, a 408 or a 429. Sending the same payload
	// again later may well succeed.
	deliveryFailed
)

// maxFlushFailures is how many flushes in a row may fail before a batch sender (PerformanceFlusher,
// MetricBuffer) drops the batch it has been holding on to and starts clean, so an outage that never
// ends can't keep one ever-growing batch alive for as long as the process lives.
const maxFlushFailures = 5

func NewClient(configuration *Configuration) *Client {
	return &Client{
		configuration: configuration,
		httpClient:    &http.Client{Timeout: configuration.Timeout},
	}
}

func (c *Client) Deliver(payload map[string]any) bool {
	return c.post(c.configuration.IngestionURI(), payload) == deliveryDelivered
}

// DeliverPerformanceSamples posts a batch of aggregate performance samples (one entry per
// distinct transaction a flush interval saw, see PerformanceFlusher), not a single payload the
// way Deliver's own is.
func (c *Client) DeliverPerformanceSamples(samples []map[string]any) bool {
	return c.deliverPerformanceSamples(samples) == deliveryDelivered
}

// deliverPerformanceSamples is DeliverPerformanceSamples for PerformanceFlusher, which needs to
// tell a rejected batch from one that only failed for now.
func (c *Client) deliverPerformanceSamples(samples []map[string]any) deliveryOutcome {
	return c.post(c.configuration.PerformanceSamplesURI(), map[string]any{"samples": samples})
}

// DeliverSpans posts one whole captured trace: {"trace_id": ..., "spans": [...]}, unlike
// DeliverPerformanceSamples' batch over a time window. One trace, one POST.
func (c *Client) DeliverSpans(trace map[string]any) bool {
	return c.post(c.configuration.SpansURI(), trace) == deliveryDelivered
}

// DeliverMetrics posts a batch of individual CaptureMetric entries as {"metrics": [...]}.
func (c *Client) DeliverMetrics(entries []map[string]any) bool {
	return c.deliverMetrics(entries) == deliveryDelivered
}

// deliverMetrics is DeliverMetrics for MetricBuffer, for the same reason as
// deliverPerformanceSamples.
func (c *Client) deliverMetrics(entries []map[string]any) deliveryOutcome {
	return c.post(c.configuration.CustomMetricsURI(), map[string]any{"metrics": entries})
}

// DeliverInfrastructureMetrics posts a batch of infrastructure readings the same way.
func (c *Client) DeliverInfrastructureMetrics(entries []map[string]any) bool {
	return c.deliverInfrastructureMetrics(entries) == deliveryDelivered
}

func (c *Client) deliverInfrastructureMetrics(entries []map[string]any) deliveryOutcome {
	return c.post(c.configuration.InfrastructureMetricsURI(), map[string]any{"metrics": entries})
}

// DeliverChange posts one RecordChange payload to "/changes".
func (c *Client) DeliverChange(change map[string]any) bool {
	return c.post(c.configuration.ChangesURI(), change) == deliveryDelivered
}

// DeliverChangeSnapshot posts the startup change snapshot to "/change_snapshots".
func (c *Client) DeliverChangeSnapshot(snapshot map[string]any) bool {
	return c.post(c.configuration.ChangeSnapshotsURI(), snapshot) == deliveryDelivered
}

func (c *Client) post(uri string, payload map[string]any) deliveryOutcome {
	if uri == "" {
		return deliveryFailed
	}

	body, err := json.Marshal(payload)
	if err != nil {
		// Rejected, not failed: a payload that can't be encoded now can't be encoded on a retry.
		c.configuration.Logger.Debugf("delivery failed: %v", err)
		return deliveryRejected
	}

	request, err := http.NewRequest(http.MethodPost, uri, bytes.NewReader(body))
	if err != nil {
		c.configuration.Logger.Debugf("delivery failed: %v", err)
		return deliveryFailed
	}
	request.Header.Set("Authorization", "Bearer "+c.configuration.APIKey())
	request.Header.Set("Content-Type", "application/json")

	response, err := c.httpClient.Do(request)
	if err != nil {
		c.configuration.Logger.Debugf("delivery failed: %v", err)
		return deliveryFailed
	}
	defer response.Body.Close()
	// Drain the body so the underlying connection can be reused by the next delivery rather than
	// forcing http.Client to open a fresh one every time.
	_, _ = io.Copy(io.Discard, response.Body)

	outcome := outcomeForStatus(response.StatusCode)
	if outcome != deliveryDelivered {
		c.configuration.Logger.Debugf("delivery failed: the server answered %d", response.StatusCode)
	}
	return outcome
}

// outcomeForStatus sorts a response status into the three outcomes. 408 (the server gave up
// waiting for the request) and 429 (rate limited) are the two 4xx statuses that say nothing about
// the payload itself, so they count as failed for now. Anything else that isn't a 2xx or a 4xx
// (a 5xx, or a redirect net/http didn't follow) does too.
func outcomeForStatus(status int) deliveryOutcome {
	switch {
	case status >= 200 && status < 300:
		return deliveryDelivered
	case status == http.StatusRequestTimeout || status == http.StatusTooManyRequests:
		return deliveryFailed
	case status >= 400 && status < 500:
		return deliveryRejected
	default:
		return deliveryFailed
	}
}
