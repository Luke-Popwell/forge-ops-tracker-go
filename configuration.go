package forgeops

import (
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Configuration holds a single ForgeOps DSN plus everything else the client needs to build and
// deliver events. Mirrors gems/forge_ops_tracker's Configuration: a single DSN
// string carries both the ingestion URL and the project's API key:
// "https://<api_key>@host/api/v1/events".
type Configuration struct {
	DSN         string
	Environment string
	Release     string
	ServerName  string

	// AppRoot decides whether a backtrace frame is "in_app": a frame's file path is compared
	// against this root, the same file-path matching the Ruby gem does against Rails.root and the
	// Python client does against os.getcwd(). A Go binary built without -trimpath embeds the real
	// build-time source paths, so the same approach works here too. Defaults to the current
	// working directory; set it explicitly if that doesn't match your app's actual layout (a
	// binary started from a different directory than its module root, for instance).
	AppRoot string

	EnabledEnvironments map[string]bool
	QueueSize           int
	Timeout             time.Duration
	ScrubPII            bool

	// CaptureSourceContext controls whether EventBuilder reads a few lines of source off disk
	// around each in-app frame's culprit line (see EventBuilder.attachSourceContext). Defaults to
	// true so a snippet shows up with no extra setup, but this field by itself isn't what actually
	// keeps proprietary source code from ending up somewhere it shouldn't: ForgeOps' own
	// per-project setting is the durable, server-enforced off switch, since it applies no matter
	// what this field happens to be set to on any given deployment, and can't quietly drift back
	// on the way a local config value could. Set this to false too if this host app should never
	// even attempt that disk read in the first place.
	CaptureSourceContext bool

	// CaptureSQLObjects, when an error carries the SQL behind a failed database call (attached with
	// WithSQL, or exposed by an error type implementing SQLStatement() string), sends the names of
	// the stored procedure, table and view that SQL touched, so an issue says where to start
	// looking. Names are identifiers, never values, which is why this defaults on.
	// CaptureSQLStatement is the separate, opt-in step of also sending the statement itself, with
	// every string and number replaced by "?"; off by default because even a masked statement
	// describes your schema, and ForgeOps' own per-project setting is what durably governs whether
	// the server stores it. See sql_statement.go.
	CaptureSQLObjects   bool
	CaptureSQLStatement bool

	Logger Logger

	// TrackPerformance controls whether the net/http/Gin integrations time every request and
	// periodically report an aggregate per transaction (see PerformanceFlusher). On by default,
	// the same "on unless you turn it off" posture error tracking itself already has.
	TrackPerformance bool
	// MetricFlushInterval and InfrastructureMetricFlushInterval are the time between flushes of the
	// buffered CaptureMetric / CaptureInfrastructureMetric entries (see MetricBuffer). There is no
	// TrackMetrics flag the way TrackPerformance has one: these are explicit calls the host app's
	// own code makes, not automatic instrumentation, so there is nothing to turn off that simply
	// not calling them doesn't already do.
	MetricFlushInterval               time.Duration
	InfrastructureMetricFlushInterval time.Duration
	// PerformanceFlushInterval is the time between aggregate performance reports; requests are
	// timed in-process and flushed as one small batch on this interval, not one network call
	// per request.
	PerformanceFlushInterval time.Duration

	// TrackBreadcrumbs controls whether WithBreadcrumbs actually attaches a trail to a context at
	// all (a false value makes it a no-op, returning ctx unchanged), and whether the net/http/Gin
	// integrations record their own automatic "controller" entry. On by default, the same "on
	// unless you turn it off" posture every other independent tracking mechanism in this client
	// already has. AddBreadcrumb itself works regardless of this flag, same as every other client
	// in this repo: it only gates whether a trail exists to add to in the first place, never a
	// second check once one already does.
	TrackBreadcrumbs bool
	// MaxBreadcrumbs is the most recent entries a single trail keeps; the oldest is dropped once
	// full. Matches gems/forge_ops_tracker's own default exactly.
	MaxBreadcrumbs int

	// TrackTracing controls whether the net/http/Gin Timing middlewares open a trace per request
	// (see WithTrace) and whether StartSpan/RecordSpan/Transport record anything. On by default, the
	// same "on unless you turn it off" posture every other tracking mechanism here has; tracing is a
	// plan-gated feature, enforced server-side, so an org without it just gets a 403 it ignores.
	// Every request still gets a trace id with this off (see WithRequest): it goes on errors
	// reported during the request and on the traceparent header Transport adds (see
	// PropagateTraces), since that id is also what links an error here to one in another service.
	TrackTracing bool
	// TraceCaptureThreshold is how long a request's root span must run before its whole trace is
	// sent at all: the entire point of the feature, not a sampling knob (a fast request costs
	// nothing over the wire). 1 second, matching gems/forge_ops_tracker's own default. A request
	// that errored sends its trace however fast it was.
	TraceCaptureThreshold time.Duration

	// PropagateTraces controls whether Transport adds a W3C traceparent header to outbound requests
	// made inside a request or trace, so the service being called continues this trace. On by
	// default, matching gems/forge_ops_tracker: the header carries the trace id that links an error
	// here to an error there, which is useful with or without spans, so it goes out even with
	// TrackTracing off. A traceparent the request already has is never replaced.
	PropagateTraces bool
	// TracePropagationTargets limits which hosts get that header. nil (the default) means every
	// host. Otherwise each entry is a host string, matching that host and its subdomains on a dot
	// boundary ("example.com" matches "api.example.com", never "badexample.com"; case and a leading
	// dot are ignored), or a *regexp.Regexp searched for anywhere in the lowercased host, so anchor
	// it yourself. Anything else matches nothing, so an empty, non-nil slice propagates nowhere.
	// Useful for a third-party API that rejects unknown headers, or that shouldn't learn your trace
	// ids at all.
	TracePropagationTargets []any

	// DetectChanges controls whether Init sends one change snapshot per process (the Go version and
	// the module versions compiled into this binary; see change_tracking.go) so ForgeOps can record
	// what changed since the last deploy. On by default, the same "on unless you turn it off"
	// posture as every other tracking mechanism here. RecordChange works regardless of this flag.
	DetectChanges bool
	// TrackEnvVarNames adds the NAMES (never the values) of this process's environment variables to
	// that snapshot, minus host-specific noise like HOSTNAME or PATH, so a variable added or removed
	// between deploys shows up as a change. Off by default: even a variable's name can say more
	// about your setup than you want to send.
	TrackEnvVarNames bool
}

// NewConfiguration returns a Configuration seeded from FORGE_OPS_DSN/FORGE_OPS_ENVIRONMENT/
// FORGE_OPS_RELEASE and sensible defaults for everything else: the same env vars and defaults
// every other client in this repo reads.
func NewConfiguration() *Configuration {
	cwd, _ := os.Getwd()
	return &Configuration{
		DSN:                               os.Getenv("FORGE_OPS_DSN"),
		Environment:                       envOrDefault("FORGE_OPS_ENVIRONMENT", "development"),
		Release:                           os.Getenv("FORGE_OPS_RELEASE"),
		ServerName:                        safeHostname(),
		AppRoot:                           cwd,
		EnabledEnvironments:               map[string]bool{"production": true, "staging": true},
		QueueSize:                         1000,
		Timeout:                           2 * time.Second,
		ScrubPII:                          true,
		CaptureSourceContext:              true,
		CaptureSQLObjects:                 true,
		Logger:                            noopLogger{},
		TrackPerformance:                  true,
		MetricFlushInterval:               60 * time.Second,
		InfrastructureMetricFlushInterval: 60 * time.Second,
		PerformanceFlushInterval:          60 * time.Second,
		TrackBreadcrumbs:                  true,
		MaxBreadcrumbs:                    30,
		TrackTracing:                      true,
		TraceCaptureThreshold:             time.Second,
		PropagateTraces:                   true,
		DetectChanges:                     true,
	}
}

// ShouldPropagateTrace reports whether an outbound request to host (a hostname, no port) should
// carry a traceparent header, per PropagateTraces and TracePropagationTargets. Case-insensitive,
// since hostnames are.
func (c *Configuration) ShouldPropagateTrace(host string) bool {
	if !c.PropagateTraces {
		return false
	}
	if c.TracePropagationTargets == nil {
		return true
	}
	host = strings.ToLower(host)
	for _, target := range c.TracePropagationTargets {
		switch t := target.(type) {
		case *regexp.Regexp:
			if t != nil && t.MatchString(host) {
				return true
			}
		case string:
			domain := strings.TrimPrefix(strings.ToLower(t), ".")
			if domain != "" && (host == domain || strings.HasSuffix(host, "."+domain)) {
				return true
			}
		}
	}
	return false
}

func envOrDefault(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

func safeHostname() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
}

// APIKey returns the DSN's userinfo component, URL-decoded: empty if the DSN is unset or
// malformed.
func (c *Configuration) APIKey() string {
	parsed := c.parsedDSN()
	if parsed == nil || parsed.User == nil {
		return ""
	}
	return parsed.User.Username()
}

// IngestionURI returns the ingestion URL with credentials stripped out: they travel as the
// Authorization header instead, never embedded in the request URI.
func (c *Configuration) IngestionURI() string {
	parsed := c.parsedDSN()
	if parsed == nil {
		return ""
	}
	stripped := *parsed
	stripped.User = nil
	return stripped.String()
}

// PerformanceSamplesURI returns the same ingestion URL with the trailing "/events" swapped for
// "/performance_samples": one DSN, two endpoints, matching every other client's own
// sessionCheckinsUri()/SessionCheckinsUri()/session_checkins_uri() derivation for its own sibling
// endpoint.
func (c *Configuration) PerformanceSamplesURI() string {
	uri := c.IngestionURI()
	if uri == "" {
		return ""
	}
	const suffix = "/events"
	if strings.HasSuffix(uri, suffix) {
		return strings.TrimSuffix(uri, suffix) + "/performance_samples"
	}
	return uri
}

// CustomMetricsURI returns the same ingestion URL with the trailing "/events" swapped for
// "/custom_metrics".
func (c *Configuration) CustomMetricsURI() string {
	return c.swapEventsSuffix("/custom_metrics")
}

// InfrastructureMetricsURI returns the same ingestion URL with the trailing "/events" swapped for
// "/infrastructure_metrics".
func (c *Configuration) InfrastructureMetricsURI() string {
	return c.swapEventsSuffix("/infrastructure_metrics")
}

func (c *Configuration) swapEventsSuffix(replacement string) string {
	uri := c.IngestionURI()
	if uri == "" {
		return ""
	}
	const suffix = "/events"
	if strings.HasSuffix(uri, suffix) {
		return strings.TrimSuffix(uri, suffix) + replacement
	}
	return uri
}

// ChangesURI returns the same ingestion URL with the trailing "/events" swapped for "/changes".
func (c *Configuration) ChangesURI() string {
	return c.swapEventsSuffix("/changes")
}

// ChangeSnapshotsURI returns the same ingestion URL with the trailing "/events" swapped for
// "/change_snapshots".
func (c *Configuration) ChangeSnapshotsURI() string {
	return c.swapEventsSuffix("/change_snapshots")
}

// SpansURI returns the same ingestion URL with the trailing "/events" swapped for "/spans": one
// captured trace, one POST, matching gems/forge_ops_tracker's own Configuration#spans_uri.
func (c *Configuration) SpansURI() string {
	uri := c.IngestionURI()
	if uri == "" {
		return ""
	}
	const suffix = "/events"
	if strings.HasSuffix(uri, suffix) {
		return strings.TrimSuffix(uri, suffix) + "/spans"
	}
	return uri
}

func (c *Configuration) IsEnabled() bool {
	return c.DSN != "" && c.APIKey() != "" && c.EnabledEnvironments[c.Environment]
}

func (c *Configuration) parsedDSN() *url.URL {
	if c.DSN == "" {
		return nil
	}
	// url.Parse rarely errors on garbage input: it just produces an incomplete *url.URL, which
	// the empty Scheme/Host check below catches the same way Python's urlsplit-then-check-scheme
	// approach does.
	parsed, err := url.Parse(c.DSN)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil
	}
	return parsed
}
