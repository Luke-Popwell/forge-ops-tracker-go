package gin

import (
	"net/http"
	"net/http/httptest"
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
