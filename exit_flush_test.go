package forgeops

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

// These run this test binary again as a child process (the helper below, selected by an
// environment variable), so what's checked is a real process exiting right after it reports
// something: the only way to show that nothing is left behind on the delivery goroutine, which
// dies with the process.

const exitFlushModeEnv = "FORGE_OPS_EXIT_FLUSH_MODE"
const exitFlushDSNEnv = "FORGE_OPS_EXIT_FLUSH_DSN"

func TestExitFlushHelperProcess(t *testing.T) {
	mode := os.Getenv(exitFlushModeEnv)
	if mode == "" {
		t.Skip("only runs as the child process of the exit flush tests")
	}

	Init(func(c *Configuration) {
		c.DetectChanges = false
		c.DSN = os.Getenv(exitFlushDSNEnv)
		c.Environment = "production"
		c.Timeout = 2 * time.Second
		c.Logger = noopLogger{}
	})

	switch mode {
	case "capture":
		// What a script's main looks like: report, flush, exit straight away.
		CaptureError(errors.New("reported just before exit"), nil, nil)
		Flush(2 * time.Second)
		os.Exit(0)
	case "panic":
		// A panic that crashes the process, reported by a deferred Recover alone.
		defer Recover(nil, nil)
		panic("crashed just after starting")
	}
}

// slowServer counts requests only once it has answered them, after a delay long enough that a
// child exiting without waiting would never be counted.
func slowServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var received int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		atomic.AddInt32(&received, 1)
	}))
	t.Cleanup(server.Close)
	return server, &received
}

func runExitFlushChild(t *testing.T, mode string, server *httptest.Server) error {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestExitFlushHelperProcess$")
	cmd.Env = append(os.Environ(),
		exitFlushModeEnv+"="+mode,
		exitFlushDSNEnv+"=http://key@"+server.Listener.Addr().String()+"/events",
	)
	return cmd.Run()
}

func TestAProcessThatFlushesBeforeExitingDeliversItsError(t *testing.T) {
	server, received := slowServer(t)

	if err := runExitFlushChild(t, "capture", server); err != nil {
		t.Fatalf("child process failed: %v", err)
	}

	if got := atomic.LoadInt32(received); got != 1 {
		t.Fatalf("server received %d events from the exited child, want 1", got)
	}
}

func TestAPanicThatCrashesTheProcessIsStillDelivered(t *testing.T) {
	server, received := slowServer(t)

	err := runExitFlushChild(t, "panic", server)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child process should have crashed with the panic, got %v", err)
	}

	if got := atomic.LoadInt32(received); got != 1 {
		t.Fatalf("server received %d events from the crashed child, want 1", got)
	}
}
