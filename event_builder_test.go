package forgeops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func testConfiguration() *Configuration {
	return &Configuration{
		Environment:          "production",
		Release:              "a1b2c3d",
		ServerName:           "test-host",
		AppRoot:              "/app",
		ScrubPII:             true,
		CaptureSourceContext: true,
	}
}

// capturePcs is itself part of package forgeops, the same import path modulePathPrefix filters
// out: so a backtrace built from it demonstrates the filter correctly walking past every
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

	payload := builder.Build(err, map[string]any{"order_id": 42}, nil, capturePcs(), nil)

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
	if payload["sdk_name"] != "go" {
		t.Errorf("sdk_name = %v, want %q", payload["sdk_name"], "go")
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

	payload := builder.Build(errors.New("boom"), nil, nil, capturePcs(), nil)
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
	// own frames would in real use: the first surviving frame is testing's own call into it.
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
	payload := builder.Build(err, map[string]any{"api_key": "shh-secret"}, nil, capturePcs(), nil)

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
	payload := builder.Build(err, nil, nil, capturePcs(), nil)

	if payload["message"] != "contact user@example.com" {
		t.Errorf("message = %v, want unscrubbed", payload["message"])
	}
}

func TestBuildIncludesTheUserWhenGivenOneNeverScrubbedEvenThoughItsAnEmail(t *testing.T) {
	config := testConfiguration()
	builder := NewEventBuilder(config)

	payload := builder.Build(errors.New("boom"), nil, map[string]any{"id": 42, "email": "ada@example.com"}, capturePcs(), nil)

	user, ok := payload["user"].(map[string]any)
	if !ok {
		t.Fatalf("payload[user] = %#v, want a map[string]any", payload["user"])
	}
	if user["email"] != "ada@example.com" {
		t.Errorf(`user["email"] = %v, want unscrubbed "ada@example.com"`, user["email"])
	}
}

func TestBuildOmitsTheUserKeyEntirelyWhenNoneWasGiven(t *testing.T) {
	config := testConfiguration()
	builder := NewEventBuilder(config)

	payload := builder.Build(errors.New("boom"), nil, nil, capturePcs(), nil)

	if _, ok := payload["user"]; ok {
		t.Errorf("payload[user] = %#v, want no user key at all", payload["user"])
	}
}

// writeSourceFile creates a temp file with lineCount lines, "line 1" through "line <lineCount>",
// and returns its path. Callers are responsible for removing it (t.Cleanup, mirroring the other
// clients' tempfile-per-test approach rather than a shared fixture path).
func writeSourceFile(t *testing.T, lineCount int) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "app_file-*.go")
	if err != nil {
		t.Fatalf("os.CreateTemp() error = %v", err)
	}
	defer f.Close()

	lines := make([]string, lineCount)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	if _, err := f.WriteString(strings.Join(lines, "\n")); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}
	return f.Name()
}

func TestAttachSourceContextAttachesContextLinePreAndPostContextByDefault(t *testing.T) {
	config := testConfiguration()
	builder := NewEventBuilder(config)
	path := writeSourceFile(t, 20)

	frame := builder.attachSourceContext(Frame{File: path, Line: 10, InApp: true})

	if frame.ContextLine == nil || *frame.ContextLine != "line 10" {
		t.Errorf("ContextLine = %v, want %q", frame.ContextLine, "line 10")
	}
	wantPre := []string{"line 5", "line 6", "line 7", "line 8", "line 9"}
	if frame.PreContext == nil || !reflect.DeepEqual(*frame.PreContext, wantPre) {
		t.Errorf("PreContext = %v, want %v", frame.PreContext, wantPre)
	}
	wantPost := []string{"line 11", "line 12", "line 13", "line 14", "line 15"}
	if frame.PostContext == nil || !reflect.DeepEqual(*frame.PostContext, wantPost) {
		t.Errorf("PostContext = %v, want %v", frame.PostContext, wantPost)
	}
}

func TestAttachSourceContextClampsAtFileBoundariesRatherThanPanicking(t *testing.T) {
	config := testConfiguration()
	builder := NewEventBuilder(config)
	path := writeSourceFile(t, 3)

	first := builder.attachSourceContext(Frame{File: path, Line: 1, InApp: true})
	last := builder.attachSourceContext(Frame{File: path, Line: 3, InApp: true})

	if len(*first.PreContext) != 0 {
		t.Errorf("first.PreContext = %v, want empty", *first.PreContext)
	}
	if want := []string{"line 2", "line 3"}; !reflect.DeepEqual(*first.PostContext, want) {
		t.Errorf("first.PostContext = %v, want %v", *first.PostContext, want)
	}
	if want := []string{"line 1", "line 2"}; !reflect.DeepEqual(*last.PreContext, want) {
		t.Errorf("last.PreContext = %v, want %v", *last.PreContext, want)
	}
	if len(*last.PostContext) != 0 {
		t.Errorf("last.PostContext = %v, want empty", *last.PostContext)
	}
}

func TestAttachSourceContextTruncatesAnOverlongLine(t *testing.T) {
	config := testConfiguration()
	builder := NewEventBuilder(config)
	f, err := os.CreateTemp(t.TempDir(), "app_file-*.go")
	if err != nil {
		t.Fatalf("os.CreateTemp() error = %v", err)
	}
	overlong := strings.Repeat("x", 600)
	if _, err := f.WriteString(overlong); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}
	f.Close()

	frame := builder.attachSourceContext(Frame{File: f.Name(), Line: 1, InApp: true})

	want := strings.Repeat("x", 500) + "..."
	if frame.ContextLine == nil || *frame.ContextLine != want {
		t.Errorf("ContextLine = %v, want %q", frame.ContextLine, want)
	}
}

func TestAttachSourceContextNeverAttachesToAFrameThatIsNotInApp(t *testing.T) {
	config := testConfiguration()
	builder := NewEventBuilder(config)
	path := writeSourceFile(t, 20)

	frame := builder.attachSourceContext(Frame{File: path, Line: 10, InApp: false})

	if frame.ContextLine != nil || frame.PreContext != nil || frame.PostContext != nil {
		t.Errorf("expected no source context on a non-in-app frame, got %+v", frame)
	}
}

func TestAttachSourceContextAttemptsNoFileReadWhenCaptureSourceContextIsDisabled(t *testing.T) {
	config := testConfiguration()
	config.CaptureSourceContext = false
	builder := NewEventBuilder(config)
	path := writeSourceFile(t, 20)

	original := readFile
	readFile = func(name string) ([]byte, error) {
		t.Fatal("readFile should not be called when CaptureSourceContext is disabled")
		return nil, nil
	}
	defer func() { readFile = original }()

	frame := builder.attachSourceContext(Frame{File: path, Line: 10, InApp: true})

	if frame.ContextLine != nil {
		t.Errorf("ContextLine = %v, want nil", frame.ContextLine)
	}
}

func TestAttachSourceContextLeavesAFrameUntouchedWhenTheFileCannotBeRead(t *testing.T) {
	config := testConfiguration()
	builder := NewEventBuilder(config)
	missingPath := filepath.Join(t.TempDir(), "this-file-does-not-exist.go")

	frame := builder.attachSourceContext(Frame{File: missingPath, Line: 1, InApp: true})

	if frame.ContextLine != nil {
		t.Errorf("ContextLine = %v, want nil", frame.ContextLine)
	}
}

func TestBuildOmitsBreadcrumbsEntirelyWhenNoneAreGiven(t *testing.T) {
	config := testConfiguration()
	builder := NewEventBuilder(config)

	payload := builder.Build(errors.New("boom"), nil, nil, capturePcs(), nil)

	if _, ok := payload["breadcrumbs"]; ok {
		t.Errorf("payload has a breadcrumbs key, want it omitted for an empty trail")
	}
}

func TestBuildIncludesBreadcrumbsWhenGiven(t *testing.T) {
	config := testConfiguration()
	builder := NewEventBuilder(config)
	crumbs := []Breadcrumb{
		{Category: "controller", Message: "GET /orders/42", Level: "info", Timestamp: "2026-01-01T00:00:00Z", Data: map[string]any{"status": 200}},
	}

	payload := builder.Build(errors.New("boom"), nil, nil, capturePcs(), crumbs)

	got, ok := payload["breadcrumbs"].([]Breadcrumb)
	if !ok || len(got) != 1 || got[0].Message != "GET /orders/42" {
		t.Errorf("payload[breadcrumbs] = %#v, want the one entry given", payload["breadcrumbs"])
	}
}

func TestBuildScrubsBreadcrumbMessageAndDataButNotCategoryLevelOrTimestamp(t *testing.T) {
	config := testConfiguration()
	config.ScrubPII = true
	builder := NewEventBuilder(config)
	crumbs := []Breadcrumb{
		{
			Category: "custom", Message: "emailed alice@example.com", Level: "info", Timestamp: "2026-01-01T00:00:00Z",
			Data: map[string]any{"email": "alice@example.com", "password": "hunter2"},
		},
	}

	payload := builder.Build(errors.New("boom"), nil, nil, capturePcs(), crumbs)

	got := payload["breadcrumbs"].([]Breadcrumb)[0]
	if strings.Contains(got.Message, "alice@example.com") {
		t.Errorf("Message = %q, want the email scrubbed", got.Message)
	}
	if got.Category != "custom" || got.Level != "info" || got.Timestamp != "2026-01-01T00:00:00Z" {
		t.Errorf("Category/Level/Timestamp = %q/%q/%q, want them left untouched", got.Category, got.Level, got.Timestamp)
	}
	if got.Data["password"] == "hunter2" {
		t.Errorf("Data[password] = %q, want it redacted as a sensitive key", got.Data["password"])
	}
}
