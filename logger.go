package forgeops

import "log"

// Logger is this client's own internal debug channel -- never the host app's error/exception
// path, since a broken tracker must never be able to make noise there. Defaults to a no-op; set
// Configuration.Logger to StdLogger{log.Default()} (or your own implementation) to see it.
type Logger interface {
	Debugf(format string, args ...any)
}

type noopLogger struct{}

func (noopLogger) Debugf(string, ...any) {}

// StdLogger adapts the standard library's *log.Logger to the Logger interface, prefixing every
// line so it's identifiable among a host app's other log output.
type StdLogger struct {
	*log.Logger
}

func (l StdLogger) Debugf(format string, args ...any) {
	l.Printf("[forgeops] "+format, args...)
}
