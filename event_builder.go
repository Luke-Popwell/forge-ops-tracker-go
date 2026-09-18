package forgeops

import (
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"time"
)

// MaxFrames caps how many backtrace frames a single event carries, the same limit every other
// client in this repo applies.
const MaxFrames = 500

// ContextLines is how many lines of source to grab on either side of the culprit line (see
// EventBuilder.attachSourceContext), and MaxContextLineLength is the longest a single captured
// line is allowed to be before getting truncated: guards against one pathological
// minified/generated line ballooning the payload. ForgeOps itself re-truncates on arrival too, the
// same "don't just trust the SDK" posture MaxFrames already gets on the server side.
const (
	ContextLines         = 5
	MaxContextLineLength = 500
)

const modulePathPrefix = "github.com/Luke-Popwell/forge-ops-tracker-go"

// sdkName identifies this client to the server's auto language-detection on the project the
// event lands in (see Project#note_sdk_platform server-side); matches this repo's own sdks/go
// directory name, the same convention every other language's client follows.
const sdkName = "go"

// readFile is a var, not a direct os.ReadFile call, purely so a test can swap it out to assert
// attachSourceContext never even attempts a read when CaptureSourceContext is disabled.
var readFile = os.ReadFile

// Frame is one entry in an event's backtrace. ContextLine/PreContext/PostContext are pointers
// (rather than plain string/[]string) so that omitempty on a *nil* pointer omits the key entirely
// on the wire when no source context was attached, while still letting a non-nil pointer to a
// genuinely empty slice (a culprit line at the very start or end of a file) marshal as "[]" rather
// than also being treated as empty: omitempty only ever looks at pointer nil-ness, never at what
// a non-nil pointer points to.
type Frame struct {
	File        string    `json:"file"`
	Line        int       `json:"line"`
	Method      string    `json:"method"`
	InApp       bool      `json:"in_app"`
	ContextLine *string   `json:"context_line,omitempty"`
	PreContext  *[]string `json:"pre_context,omitempty"`
	PostContext *[]string `json:"post_context,omitempty"`
}

// EventBuilder turns a reported error into the payload shape the ingestion API expects. Ported
// from gems/forge_ops_tracker/lib/forge_ops_tracker/event_builder.rb: backtrace frames come from
// runtime.Callers rather than regex-parsing MRI backtrace lines, but the resulting shape
// (file/line/method/in_app) is the same.
type EventBuilder struct {
	configuration *Configuration
}

func NewEventBuilder(configuration *Configuration) *EventBuilder {
	return &EventBuilder{configuration: configuration}
}

// Build turns err into an event payload. pcs is the raw program counters from runtime.Callers,
// captured at the point CaptureError/Recover was called: this client captures the stack at the
// call site rather than from the error value itself, since a plain Go error carries no stack of
// its own, unlike Python's traceback or Java's Throwable, which travel with the exception.
func (b *EventBuilder) Build(err error, context map[string]any, user map[string]any, pcs []uintptr, breadcrumbs []Breadcrumb) map[string]any {
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
		"sdk_name":        sdkName,
	}
	if len(user) > 0 {
		payload["user"] = copyContext(user)
	}
	// Omitted entirely rather than an empty slice: matches gems/forge_ops_tracker's own
	// `if breadcrumbs && !breadcrumbs.empty?`, and every other optional field this map already
	// leaves out rather than sending as a deliberately-empty placeholder.
	if len(breadcrumbs) > 0 {
		payload["breadcrumbs"] = breadcrumbs
	}

	if b.configuration.ScrubPII {
		payload = b.scrub(payload)
	}
	return payload
}

// exception_class/occurred_at/environment/release/server_name/sdk_name/user are left alone:
// structured fields this client or the host app sets deliberately, not free text an error or its
// context could accidentally spill sensitive data into. user specifically is a deliberate
// exemption, not an oversight: ScrubValue's own email pattern would otherwise redact the exact
// thing this field exists to carry (scrub below never touches the "user" key at all).
func (b *EventBuilder) scrub(payload map[string]any) map[string]any {
	payload["message"] = ScrubString(payload["message"].(string))

	frames := payload["backtrace"].([]Frame)
	scrubbed := make([]Frame, len(frames))
	for i, frame := range frames {
		scrubbed[i] = scrubFrame(frame)
	}
	payload["backtrace"] = scrubbed

	payload["context"] = ScrubValue(payload["context"], "")
	payload["tags"] = ScrubValue(payload["tags"], "")

	// category/level/timestamp are left alone, the same "structured fields this client sets
	// deliberately, not free text" exemption exception_class/environment/etc. already get above:
	// only message (arbitrary text) and data (arbitrary caller-supplied values, the same shape
	// context already is) can carry anything worth scrubbing.
	if crumbs, ok := payload["breadcrumbs"].([]Breadcrumb); ok {
		scrubbedCrumbs := make([]Breadcrumb, len(crumbs))
		for i, crumb := range crumbs {
			scrubbedCrumbs[i] = Breadcrumb{
				Category:  crumb.Category,
				Message:   ScrubString(crumb.Message),
				Level:     crumb.Level,
				Timestamp: crumb.Timestamp,
				Data:      ScrubValue(crumb.Data, "").(map[string]any),
			}
		}
		payload["breadcrumbs"] = scrubbedCrumbs
	}
	return payload
}

func scrubFrame(frame Frame) Frame {
	scrubbed := Frame{
		File:   ScrubString(frame.File),
		Line:   frame.Line,
		Method: ScrubString(frame.Method),
		InApp:  frame.InApp,
	}
	if frame.ContextLine != nil {
		contextLine := ScrubString(*frame.ContextLine)
		preContext := scrubStrings(*frame.PreContext)
		postContext := scrubStrings(*frame.PostContext)
		scrubbed.ContextLine = &contextLine
		scrubbed.PreContext = &preContext
		scrubbed.PostContext = &postContext
	}
	return scrubbed
}

func scrubStrings(lines []string) []string {
	scrubbed := make([]string, len(lines))
	for i, line := range lines {
		scrubbed[i] = ScrubString(line)
	}
	return scrubbed
}

func (b *EventBuilder) backtrace(pcs []uintptr) []Frame {
	callerFrames := runtime.CallersFrames(pcs)
	frames := make([]Frame, 0, len(pcs))
	seenAppFrame := false
	for {
		frame, more := callerFrames.Next()

		// Skip this SDK's own frames: CaptureError/Recover/Report's own call chain adds no
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

		frames = append(frames, b.attachSourceContext(Frame{
			File:   frame.File,
			Line:   frame.Line,
			Method: frame.Function,
			InApp:  b.isInApp(frame.File),
		}))
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
	// role site-packages/dist-packages plays for Python: never in_app regardless of AppRoot.
	return !strings.Contains(file, "/pkg/mod/")
}

// attachSourceContext reads a few lines of source straight off disk around the culprit line, at
// capture-time, in the same running process the error came from. Gated on two things: the frame
// has to be in-app (never a module-cache/stdlib frame: there'd be nothing meaningful to show,
// and it's not the host app's own code to begin with), and configuration.CaptureSourceContext has
// to be true (see Configuration for why it defaults to true and why ForgeOps' own per-project
// setting, not this field, is the durable, protected way to turn it off). Best-effort: any file
// that can't be read (deleted, permission denied, a path that only ever existed on the machine
// that built the binary and isn't present on this one) just means this one frame gets no source
// context, never a panic of its own.
func (b *EventBuilder) attachSourceContext(frame Frame) Frame {
	if !b.configuration.CaptureSourceContext || !frame.InApp {
		return frame
	}

	data, err := readFile(frame.File)
	if err != nil {
		return frame
	}
	lines := splitLines(data)

	index := frame.Line - 1
	if index < 0 || index >= len(lines) {
		return frame
	}

	from := index - ContextLines
	if from < 0 {
		from = 0
	}
	to := index + ContextLines
	if to > len(lines)-1 {
		to = len(lines) - 1
	}

	contextLine := truncateContextLine(lines[index])
	preContext := append([]string{}, lines[from:index]...)
	for i, line := range preContext {
		preContext[i] = truncateContextLine(line)
	}
	postContext := append([]string{}, lines[index+1:to+1]...)
	for i, line := range postContext {
		postContext[i] = truncateContextLine(line)
	}

	frame.ContextLine = &contextLine
	frame.PreContext = &preContext
	frame.PostContext = &postContext
	return frame
}

// splitLines splits file content into lines the same way Ruby's File.readlines does: no phantom
// empty final element when the file ends with a trailing newline (a plain strings.Split would add
// one), and no trailing "\r" left over from a CRLF line ending.
func splitLines(data []byte) []string {
	text := string(data)
	lines := strings.Split(text, "\n")
	if strings.HasSuffix(text, "\n") {
		lines = lines[:len(lines)-1]
	}
	for i, line := range lines {
		lines[i] = strings.TrimSuffix(line, "\r")
	}
	return lines
}

func truncateContextLine(line string) string {
	if len(line) <= MaxContextLineLength {
		return line
	}
	return line[:MaxContextLineLength] + "..."
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

// exceptionClassName mirrors Python's module.ClassName: the error value's package-qualified
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
