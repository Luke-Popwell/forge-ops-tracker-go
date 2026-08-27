package forgeops

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"time"
)

// MaxFrames caps how many backtrace frames a single event carries, the same limit every other
// client in this repo applies.
const MaxFrames = 500

const modulePathPrefix = "github.com/Luke-Popwell/forge-ops-tracker-go"

// Frame is one entry in an event's backtrace.
type Frame struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Method string `json:"method"`
	InApp  bool   `json:"in_app"`
}

// EventBuilder turns a reported error into the payload shape the ingestion API expects. Ported
// from gems/forge_ops_tracker/lib/forge_ops_tracker/event_builder.rb -- backtrace frames come from
// runtime.Callers rather than regex-parsing MRI backtrace lines, but the resulting shape
// (file/line/method/in_app) is the same.
type EventBuilder struct {
	configuration *Configuration
}

func NewEventBuilder(configuration *Configuration) *EventBuilder {
	return &EventBuilder{configuration: configuration}
}

// Build turns err into an event payload. pcs is the raw program counters from runtime.Callers,
// captured at the point CaptureError/Recover was called -- this client captures the stack at the
// call site rather than from the error value itself, since a plain Go error carries no stack of
// its own, unlike Python's traceback or Java's Throwable, which travel with the exception.
func (b *EventBuilder) Build(err error, context map[string]any, pcs []uintptr) map[string]any {
	payload := map[string]any{
		"exception_class": exceptionClassName(err),
		"message":         err.Error(),
		"backtrace":       b.backtrace(pcs),
		"occurred_at":     time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		"environment":     b.configuration.Environment,
		"release":         b.configuration.Release,
		"server_name":     b.configuration.ServerName,
		"context":         copyContext(context),
		"tags":            map[string]any{},
	}

	if b.configuration.ScrubPII {
		payload = b.scrub(payload)
	}
	return payload
}

// exception_class/occurred_at/environment/release/server_name are left alone -- structured fields
// this client or the host app sets deliberately, not free text an error or its context could
// accidentally spill sensitive data into.
func (b *EventBuilder) scrub(payload map[string]any) map[string]any {
	payload["message"] = ScrubString(payload["message"].(string))

	frames := payload["backtrace"].([]Frame)
	scrubbed := make([]Frame, len(frames))
	for i, frame := range frames {
		scrubbed[i] = Frame{
			File:   ScrubString(frame.File),
			Line:   frame.Line,
			Method: ScrubString(frame.Method),
			InApp:  frame.InApp,
		}
	}
	payload["backtrace"] = scrubbed

	payload["context"] = ScrubValue(payload["context"], "")
	payload["tags"] = ScrubValue(payload["tags"], "")
	return payload
}

func (b *EventBuilder) backtrace(pcs []uintptr) []Frame {
	callerFrames := runtime.CallersFrames(pcs)
	frames := make([]Frame, 0, len(pcs))
	seenAppFrame := false
	for {
		frame, more := callerFrames.Next()

		// Skip this SDK's own frames -- CaptureError/Recover/Report's own call chain adds no
		// diagnostic value, the same reason runtime-internal frames never show up in a Python
		// traceback. Only skipped until real caller code is reached, so a host app that happens to
		// call into this package from deeper in its own call chain still gets an accurate trace for
		// its own frames.
		if !seenAppFrame && strings.HasPrefix(frame.Function, modulePathPrefix) {
			if !more {
				break
			}
			continue
		}
		seenAppFrame = true

		frames = append(frames, Frame{
			File:   frame.File,
			Line:   frame.Line,
			Method: frame.Function,
			InApp:  b.isInApp(frame.File),
		})
		if len(frames) >= MaxFrames || !more {
			break
		}
	}
	return frames
}

func (b *EventBuilder) isInApp(file string) bool {
	root := b.configuration.AppRoot
	if root == "" || file == "" {
		return false
	}
	if !strings.HasPrefix(file, root) {
		return false
	}
	if goroot := runtime.GOROOT(); goroot != "" && strings.HasPrefix(file, goroot) {
		return false
	}
	// The module cache under GOPATH/pkg/mod holds every third-party dependency's source, the same
	// role site-packages/dist-packages plays for Python -- never in_app regardless of AppRoot.
	return !strings.Contains(file, "/pkg/mod/")
}

func copyContext(context map[string]any) map[string]any {
	if context == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(context))
	for k, v := range context {
		out[k] = v
	}
	return out
}

// exceptionClassName mirrors Python's module.ClassName -- the error value's package-qualified
// type name, e.g. "invoicing.InvoiceSyncError" for a custom type, or the bare stdlib name
// ("*errors.errorString", "*fmt.wrapError") for a plain errors.New/fmt.Errorf value. A recovered
// panic that wasn't already an error (panicValueError, see forgeops.go) reports the panicked
// value's own type instead, e.g. "panic: string" for panic("boom"), since reporting
// "forgeops.panicValueError" for every non-error panic would tell a reader nothing.
func exceptionClassName(err error) string {
	if p, ok := err.(panicValueError); ok {
		return fmt.Sprintf("panic: %s", reflect.TypeOf(p.value))
	}
	t := reflect.TypeOf(err)
	if t == nil {
		return "error"
	}
	return t.String()
}
