package gin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	forgeops "github.com/Luke-Popwell/forge-ops-tracker-go"
)

func TestRecoveryReportsThenSends500(t *testing.T) {
	var received int32
	trackerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&received, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer trackerServer.Close()

	forgeops.Init(func(c *forgeops.Configuration) {
		c.DSN = "http://key@" + trackerServer.Listener.Addr().String() + "/events"
		c.Environment = "production"
		c.Timeout = time.Second
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Recovery())
	router.GET("/orders", func(c *gin.Context) {
		panic("boom")
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/orders", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&received) < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&received); got != 1 {
		t.Fatalf("tracker server received %d events, want 1", got)
	}
}

func TestRecoveryPassesThroughANonPanickingHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(Recovery())
	router.GET("/", func(c *gin.Context) {
		c.Status(http.StatusTeapot)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
}

// Every scenario below that needs Timing (and so triggers RecordPerformance, and so starts
// forgeops' own package-level PerformanceFlusher's background goroutine) shares this single
// trackerServer and single forgeops.Init call: see integrations/nethttp's own
// TestBreadcrumbsAndTiming for the real, go test -race-confirmed reason a second Init call here,
// racing that goroutine's own unsynchronized reads of Configuration, would be a genuine, if
// pre-existing and unrelated to breadcrumbs itself, data race.
func TestBreadcrumbsAndTiming(t *testing.T) {
	eventCh := make(chan map[string]any, 2)
	var samplesMu sync.Mutex
	// Every transaction_name ever seen across every flush, not just the latest: a flush batches
	// every bucket accumulated since the last one (see PerformanceFlusher.Flush), so two subtests
	// below whose own requests happen to land in the same 20ms window arrive together, in a
	// randomized order (Go's own map iteration order, which Flush ranges over directly with no
	// sorting). Reading only samples[0] here (this test's own first draft) intermittently missed
	// whichever transaction didn't happen to land first in that batch: a real, reproducible flake
	// this exact rollout's own -race runs caught directly, not a hypothetical.
	seenTransactionNames := map[string]bool{}
	trackerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["message"]; ok {
			eventCh <- body
		}
		if samples, ok := body["samples"].([]any); ok {
			samplesMu.Lock()
			for _, sample := range samples {
				name, _ := sample.(map[string]any)["transaction_name"].(string)
				if name != "" {
					seenTransactionNames[name] = true
				}
			}
			samplesMu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer trackerServer.Close()

	forgeops.Init(func(c *forgeops.Configuration) {
		c.DSN = "http://key@" + trackerServer.Listener.Addr().String() + "/events"
		c.Environment = "production"
		c.Timeout = time.Second
		c.PerformanceFlushInterval = 20 * time.Millisecond
	})

	t.Run("Timing's own automatic controller breadcrumb", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		router := gin.New()
		// Recovery, then Timing, matching this package's own doc comment on installing both:
		// Recovery is what actually calls forgeops.WithBreadcrumbs, so Timing's own AddBreadcrumb
		// call needs it to have already run first.
		router.Use(Recovery(), Timing())
		router.GET("/orders/:id", func(c *gin.Context) {
			panic("boom")
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/orders/42", nil)
		router.ServeHTTP(rec, req)

		select {
		case event := <-eventCh:
			crumbs, _ := event["breadcrumbs"].([]any)
			if len(crumbs) != 1 {
				t.Fatalf("breadcrumbs = %+v, want exactly one automatic controller entry", event["breadcrumbs"])
			}
			crumb, _ := crumbs[0].(map[string]any)
			// The route pattern, not "GET /orders/42": the same reason the performance subtest
			// below cares about c.FullPath() over the raw path.
			if crumb["category"] != "controller" || crumb["message"] != "GET /orders/:id" {
				t.Errorf("breadcrumb = %+v, want category controller, message %q", crumb, "GET /orders/:id")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("tracker server never received the reported event")
		}
	})

	t.Run("a manually-added breadcrumb", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		router := gin.New()
		router.Use(Recovery())
		router.GET("/checkout", func(c *gin.Context) {
			forgeops.AddBreadcrumb(c.Request.Context(), "charged card", "custom", "info", map[string]any{"order_id": 42})
			panic("boom")
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/checkout", nil)
		router.ServeHTTP(rec, req)

		select {
		case event := <-eventCh:
			crumbs, _ := event["breadcrumbs"].([]any)
			if len(crumbs) != 1 {
				t.Fatalf("breadcrumbs = %+v, want exactly the one manually-added entry", event["breadcrumbs"])
			}
			crumb, _ := crumbs[0].(map[string]any)
			if crumb["message"] != "charged card" {
				t.Errorf("breadcrumb message = %v, want %q", crumb["message"], "charged card")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("tracker server never received the reported event")
		}
	})

	t.Run("Timing uses the matched route pattern, not the raw path", func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		router := gin.New()
		router.Use(Timing())
		router.GET("/users/:id", func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/users/42", nil)
		router.ServeHTTP(rec, req)

		deadline := time.Now().Add(2 * time.Second)
		for {
			samplesMu.Lock()
			seen := seenTransactionNames["GET /users/:id"]
			samplesMu.Unlock()
			if seen {
				// The route pattern, not "GET /users/42": a distinct user id must not explode
				// into its own separate transaction the way the literal path would.
				return
			}
			if time.Now().After(deadline) {
				samplesMu.Lock()
				snapshot := fmt.Sprintf("%v", seenTransactionNames)
				samplesMu.Unlock()
				t.Fatalf("performance sample for GET /users/:id was not delivered in time (seen so far: %s)", snapshot)
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}
