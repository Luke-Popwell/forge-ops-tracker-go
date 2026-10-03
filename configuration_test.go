package forgeops

import (
	"bytes"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

const developmentWarning = `Not sending: this environment is "development", and only production, staging are enabled. ` +
	`Set FORGE_OPS_ENVIRONMENT=production (or add "development" to the enabled environments) to send from here.`

func TestResolveEnvironment(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"nothing set", map[string]string{}, "production"},
		{"blank", map[string]string{"FORGE_OPS_ENVIRONMENT": "  "}, "production"},
		{"FORGE_OPS_ENVIRONMENT", map[string]string{"FORGE_OPS_ENVIRONMENT": "development"}, "development"},
		{"GO_ENV is not consulted", map[string]string{"GO_ENV": "development"}, "production"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveEnvironment(func(key string) string { return tc.env[key] })
			if got != tc.want {
				t.Errorf("resolveEnvironment = %q, want %q", got, tc.want)
			}
		})
	}
}

func developmentConfiguration() *Configuration {
	return &Configuration{
		DSN:                 "https://abc123@example.com/api/v1/events",
		Environment:         "development",
		EnabledEnvironments: map[string]bool{"staging": true, "production": true},
		Logger:              noopLogger{},
	}
}

func TestNotSendingWarningExplainsADisabledEnvironment(t *testing.T) {
	if got := developmentConfiguration().NotSendingWarning(); got != developmentWarning {
		t.Errorf("NotSendingWarning() = %q, want %q", got, developmentWarning)
	}
}

func TestNoNotSendingWarningWhenEnabledOrUnconfigured(t *testing.T) {
	enabled := developmentConfiguration()
	enabled.Environment = "staging"
	noDSN := developmentConfiguration()
	noDSN.DSN = ""
	added := developmentConfiguration()
	added.EnabledEnvironments["development"] = true

	for name, c := range map[string]*Configuration{"enabled": enabled, "no DSN": noDSN, "added": added} {
		if got := c.NotSendingWarning(); got != "" {
			t.Errorf("%s: NotSendingWarning() = %q, want empty", name, got)
		}
	}
}

func TestWarnIfNotSendingPrintsToStderrOnce(t *testing.T) {
	var stderr bytes.Buffer
	var warned atomic.Bool

	warnIfNotSending(developmentConfiguration(), &warned, &stderr)
	warnIfNotSending(developmentConfiguration(), &warned, &stderr)

	if want := "[ForgeOps] " + developmentWarning + "\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}

type recordingLogger struct{ lines []string }

func (l *recordingLogger) Debugf(format string, args ...any) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func TestWarnIfNotSendingUsesTheConfiguredLogger(t *testing.T) {
	var stderr bytes.Buffer
	var warned atomic.Bool
	logger := &recordingLogger{}
	c := developmentConfiguration()
	c.Logger = logger

	warnIfNotSending(c, &warned, &stderr)

	if len(logger.lines) != 1 || logger.lines[0] != developmentWarning {
		t.Errorf("logged %q, want just the warning", logger.lines)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestWarnIfNotSendingIsSilentWithoutADSN(t *testing.T) {
	var stderr bytes.Buffer
	var warned atomic.Bool
	c := developmentConfiguration()
	c.DSN = ""

	warnIfNotSending(c, &warned, &stderr)

	if stderr.Len() != 0 || warned.Load() {
		t.Errorf("stderr = %q, warned = %v; want silence", stderr.String(), warned.Load())
	}
}

func TestNewConfigurationDefaults(t *testing.T) {
	t.Setenv("FORGE_OPS_DSN", "")
	t.Setenv("FORGE_OPS_ENVIRONMENT", "")
	t.Setenv("FORGE_OPS_RELEASE", "")
	os.Unsetenv("FORGE_OPS_ENVIRONMENT")

	c := NewConfiguration()

	if c.Environment != "production" {
		t.Errorf("Environment = %q, want %q", c.Environment, "production")
	}
	if c.QueueSize != 1000 {
		t.Errorf("QueueSize = %d, want 1000", c.QueueSize)
	}
	if c.Timeout != 2*time.Second {
		t.Errorf("Timeout = %v, want 2s", c.Timeout)
	}
	if !c.ScrubPII {
		t.Error("ScrubPII = false, want true")
	}
	if !c.CaptureSourceContext {
		t.Error("CaptureSourceContext = false, want true")
	}
	if !c.EnabledEnvironments["production"] || !c.EnabledEnvironments["staging"] {
		t.Error("expected production and staging enabled by default")
	}
	if !c.TrackPerformance {
		t.Error("TrackPerformance = false, want true")
	}
	if c.PerformanceFlushInterval != 60*time.Second {
		t.Errorf("PerformanceFlushInterval = %v, want 60s", c.PerformanceFlushInterval)
	}
}

func TestConfigurationPerformanceSamplesURI(t *testing.T) {
	c := &Configuration{DSN: "https://abc123@forgeops.example/api/v1/events"}

	if got := c.PerformanceSamplesURI(); got != "https://forgeops.example/api/v1/performance_samples" {
		t.Errorf("PerformanceSamplesURI() = %q, want swapped /events for /performance_samples", got)
	}
}

func TestConfigurationPerformanceSamplesURIEmptyWithNoDSN(t *testing.T) {
	c := &Configuration{}

	if got := c.PerformanceSamplesURI(); got != "" {
		t.Errorf("PerformanceSamplesURI() = %q, want empty", got)
	}
}

func TestConfigurationAPIKeyAndIngestionURI(t *testing.T) {
	c := &Configuration{DSN: "https://abc123@forgeops.example/api/v1/events"}

	if got := c.APIKey(); got != "abc123" {
		t.Errorf("APIKey() = %q, want %q", got, "abc123")
	}
	if got := c.IngestionURI(); got != "https://forgeops.example/api/v1/events" {
		t.Errorf("IngestionURI() = %q, want no credentials", got)
	}
}

func TestConfigurationAPIKeyURLDecodesUsername(t *testing.T) {
	c := &Configuration{DSN: "https://ab%2Fc@forgeops.example/api/v1/events"}

	if got := c.APIKey(); got != "ab/c" {
		t.Errorf("APIKey() = %q, want %q", got, "ab/c")
	}
}

func TestConfigurationEmptyOrMalformedDSN(t *testing.T) {
	for _, dsn := range []string{"", "not-a-url", "://broken"} {
		c := &Configuration{DSN: dsn}
		if got := c.APIKey(); got != "" {
			t.Errorf("APIKey() for DSN %q = %q, want empty", dsn, got)
		}
		if got := c.IngestionURI(); got != "" {
			t.Errorf("IngestionURI() for DSN %q = %q, want empty", dsn, got)
		}
	}
}

func TestConfigurationIsEnabled(t *testing.T) {
	cases := []struct {
		name        string
		dsn         string
		environment string
		want        bool
	}{
		{"valid dsn in production", "https://key@host/path", "production", true},
		{"valid dsn in staging", "https://key@host/path", "staging", true},
		{"valid dsn in development", "https://key@host/path", "development", false},
		{"no dsn", "", "production", false},
		{"dsn with no api key", "https://host/path", "production", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Configuration{
				DSN:                 tc.dsn,
				Environment:         tc.environment,
				EnabledEnvironments: map[string]bool{"production": true, "staging": true},
			}
			if got := c.IsEnabled(); got != tc.want {
				t.Errorf("IsEnabled() = %v, want %v", got, tc.want)
			}
		})
	}
}
