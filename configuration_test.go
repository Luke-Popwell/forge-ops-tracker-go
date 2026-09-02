package forgeops

import (
	"os"
	"testing"
	"time"
)

func TestNewConfigurationDefaults(t *testing.T) {
	t.Setenv("FORGE_OPS_DSN", "")
	t.Setenv("FORGE_OPS_ENVIRONMENT", "")
	t.Setenv("FORGE_OPS_RELEASE", "")
	os.Unsetenv("FORGE_OPS_ENVIRONMENT")

	c := NewConfiguration()

	if c.Environment != "development" {
		t.Errorf("Environment = %q, want %q", c.Environment, "development")
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
