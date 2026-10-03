package forgeops

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
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

	if mode == "setup-page" {
		// Exactly what the setup page shows: FORGE_OPS_DSN in the environment (set by the parent)
		// and a bare Init. Called twice to show the "not sending" warning still prints only once.
		Init(nil)
		Init(nil)
		CaptureError(errors.New("first test error"), nil, nil)
		Flush(2 * time.Second)
		os.Exit(0)
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
		// Only errors count: a child left at its defaults also sends a change snapshot.
		if r.URL.Path == "/events" {
			atomic.AddInt32(&received, 1)
		}
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

// runSetupPageChild runs the "setup-page" child with every FORGE_OPS_* variable from this
// process removed (so the machine running the tests can't change the result) and env added,
// returning its stderr.
func runSetupPageChild(t *testing.T, env ...string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestExitFlushHelperProcess$")
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "FORGE_OPS_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, exitFlushModeEnv+"=setup-page")
	cmd.Env = append(cmd.Env, env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("child process failed: %v\n%s", err, stderr.String())
	}
	return stderr.String()
}

func TestADSNAloneDeliversTheFirstError(t *testing.T) {
	server, received := slowServer(t)

	stderr := runSetupPageChild(t, "FORGE_OPS_DSN=http://key@"+server.Listener.Addr().String()+"/events")

	if got := atomic.LoadInt32(received); got != 1 {
		t.Fatalf("server received %d events from a child with only a DSN set, want 1", got)
	}
	if strings.Contains(stderr, "Not sending") {
		t.Errorf("unexpected warning: %s", stderr)
	}
}

func TestADevelopmentEnvironmentSendsNothingAndSaysSoOnce(t *testing.T) {
	server, received := slowServer(t)

	stderr := runSetupPageChild(t,
		"FORGE_OPS_DSN=http://key@"+server.Listener.Addr().String()+"/events",
		"FORGE_OPS_ENVIRONMENT=development",
	)

	if got := atomic.LoadInt32(received); got != 0 {
		t.Fatalf("server received %d events from a development child, want 0", got)
	}
	if want := "[ForgeOps] " + developmentWarning + "\n"; !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr, want)
	}
	if n := strings.Count(stderr, "Not sending"); n != 1 {
		t.Errorf("warning printed %d times, want once: %s", n, stderr)
	}
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
