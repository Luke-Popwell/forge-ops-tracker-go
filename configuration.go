package forgeops

import (
	"net/url"
	"os"
	"time"
)

// Configuration holds a single ForgeOps DSN plus everything else the client needs to build and
// deliver events. Mirrors gems/forge_ops_tracker's Configuration -- a single DSN
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

	Logger Logger
}

// NewConfiguration returns a Configuration seeded from FORGE_OPS_DSN/FORGE_OPS_ENVIRONMENT/
// FORGE_OPS_RELEASE and sensible defaults for everything else -- the same env vars and defaults
// every other client in this repo reads.
func NewConfiguration() *Configuration {
	cwd, _ := os.Getwd()
	return &Configuration{
		DSN:                  os.Getenv("FORGE_OPS_DSN"),
		Environment:          envOrDefault("FORGE_OPS_ENVIRONMENT", "development"),
		Release:              os.Getenv("FORGE_OPS_RELEASE"),
		ServerName:           safeHostname(),
		AppRoot:              cwd,
		EnabledEnvironments:  map[string]bool{"production": true, "staging": true},
		QueueSize:            1000,
		Timeout:              2 * time.Second,
		ScrubPII:             true,
		CaptureSourceContext: true,
		Logger:               noopLogger{},
	}
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

// APIKey returns the DSN's userinfo component, URL-decoded -- empty if the DSN is unset or
// malformed.
func (c *Configuration) APIKey() string {
	parsed := c.parsedDSN()
	if parsed == nil || parsed.User == nil {
		return ""
	}
	return parsed.User.Username()
}

// IngestionURI returns the ingestion URL with credentials stripped out -- they travel as the
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

func (c *Configuration) IsEnabled() bool {
	return c.DSN != "" && c.APIKey() != "" && c.EnabledEnvironments[c.Environment]
}

func (c *Configuration) parsedDSN() *url.URL {
	if c.DSN == "" {
		return nil
	}
	// url.Parse rarely errors on garbage input -- it just produces an incomplete *url.URL, which
	// the empty Scheme/Host check below catches the same way Python's urlsplit-then-check-scheme
	// approach does.
	parsed, err := url.Parse(c.DSN)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil
	}
	return parsed
}
