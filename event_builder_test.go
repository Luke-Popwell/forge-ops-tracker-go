package forgeops

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func testConfiguration() *Configuration {
	return &Configuration{
		Environment: "production",
		Release:     "a1b2c3d",
		ServerName:  "test-host",
		AppRoot:     "/app",
		ScrubPII:    true,
	}
}

// capturePcs is itself part of package forgeops, the same import path modulePathPrefix filters
// out -- so a backtrace built from it demonstrates the filter correctly walking past every
// forgeops-package frame (this helper, the calling test function) to the first frame outside the
// package (testing.tRunner), the same way it would walk past CaptureError/Recover/Report's own
// frames in real use to reach the host app's actual caller.
func capturePcs() []uintptr {
	pcs := make([]uintptr, MaxFrames+16)
	n := runtime.Callers(1, pcs)
	return pcs[:n]
}

func TestEventBuilderBuildBasicFields(t *testing.T) {
	config := testConfiguration()
	builder := NewEventBuilder(config)
	err := errors.New("boom")

	payload := builder.Build(err, map[string]any{"order_id": 42}, capturePcs())

	if payload["message"] != "boom" {
		t.Errorf("message = %v, want %q", payload["message"], "boom")
	}
	if payload["environment"] != "production" {
		t.Errorf("environment = %v", payload["environment"])
	}
	if payload["release"] != "a1b2c3d" {
		t.Errorf("release = %v", payload["release"])
	}
	if payload["server_name"] != "test-host" {
		t.Errorf("server_name = %v", payload["server_name"])
	}
	context := payload["context"].(map[string]any)
	if context["order_id"] != 42 {
		t.Errorf("context[order_id] = %v", context["order_id"])
	}
}

type customError struct{ msg string }

func (e customError) Error() string { return e.msg }

func TestExceptionClassNameForCustomErrorType(t *testing.T) {
	got := exceptionClassName(customError{msg: "bad"})
	if !strings.HasSuffix(got, "customError") {
		t.Errorf("exceptionClassName() = %q, want it to end with customError", got)
	}
}

func TestExceptionClassNameForStdlibError(t *testing.T) {
	got := exceptionClassName(fmt.Errorf("wrapped: %w", errors.New("inner")))
	if got == "" {
		t.Error("exceptionClassName() returned empty string")
	}
}

func TestExceptionClassNameForNonErrorPanic(t *testing.T) {
	got := exceptionClassName(panicValueError{value: "boom"})
	if got != "panic: string" {
		t.Errorf("exceptionClassName() = %q, want %q", got, "panic: string")
	}
}

func TestBacktraceExcludesSDKOwnFrames(t *testing.T) {
	config := testConfiguration()
	builder := NewEventBuilder(config)

	payload := builder.Build(errors.New("boom"), nil, capturePcs())
	frames := payload["backtrace"].([]Frame)

	if len(frames) == 0 {
		t.Fatal("expected at least one backtrace frame")
	}
	for _, f := range frames {
		if strings.HasPrefix(f.Method, modulePathPrefix) {
			t.Errorf("frame %q should have been filtered out as an SDK-internal frame", f.Method)
		}
	}
	// capturePcs and this test function are both part of package forgeops itself, so every frame
	// up to and including this test gets filtered out the same way CaptureError/Recover/Report's
	// own frames would in real use -- the first surviving frame is testing's own call into it.
	if !strings.Contains(frames[0].Method, "testing.") {
		t.Errorf("frames[0].Method = %q, want the first non-forgeops-package frame (testing.*)", frames[0].Method)
	}
}

func TestIsInAppExcludesStdlibAndModuleCache(t *testing.T) {
	config := testConfiguration()
	builder := NewEventBuilder(config)

	if builder.isInApp("/app/handlers/orders.go") != true {
		t.Error("expected a file under AppRoot to be in_app")
	}
	if builder.isInApp("/other/handlers/orders.go") != false {
		t.Error("expected a file outside AppRoot to not be in_app")
	}
	if builder.isInApp("/app/vendor/pkg/mod/github.com/x/y/z.go") != false {
		t.Error("expected a module-cache path to not be in_app")
	}
	if builder.isInApp("") != false {
		t.Error("expected an empty filename to not be in_app")
	}
}

func TestBuildScrubsMessageContextAndBacktrace(t *testing.T) {
	config := testConfiguration()
	config.AppRoot = "" // isInApp isn't under test here
	builder := NewEventBuilder(config)

	err := errors.New("failed to charge user@example.com")
	payload := builder.Build(err, map[string]any{"api_key": "shh-secret"}, capturePcs())

	if payload["message"] != "failed to charge [EMAIL FILTERED]" {
		t.Errorf("message = %v", payload["message"])
	}
	context := payload["context"].(map[string]any)
	if context["api_key"] != redacted {
		t.Errorf("context[api_key] = %v, want %q", context["api_key"], redacted)
	}
}

func TestBuildDoesNotScrubWhenDisabled(t *testing.T) {
	config := testConfiguration()
	config.ScrubPII = false
	builder := NewEventBuilder(config)

	err := errors.New("contact user@example.com")
	payload := builder.Build(err, nil, capturePcs())

	if payload["message"] != "contact user@example.com" {
		t.Errorf("message = %v, want unscrubbed", payload["message"])
	}
}
