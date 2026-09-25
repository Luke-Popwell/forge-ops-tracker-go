package forgeops

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestADatabaseSpanCarriesItsStatementMaskedAndItsSystem(t *testing.T) {
	server := newSpansServer(t)
	initTracing(t, server, nil)

	start := time.Now()
	ctx := WithTrace(context.Background())
	queryCtx, end := StartDatabaseSpan(ctx, "load orders", "SELECT * FROM orders WHERE email = 'jane@example.com' AND total > 4200", "PostgreSQL")
	RecordDatabaseSpan(queryCtx, "load user", "SELECT name FROM users WHERE id = 9911", "", time.Now(), time.Millisecond)
	end()
	FinishTrace(ctx, "GET /orders", start, 1500*time.Millisecond)

	trace := server.waitForTraces(t, 1)[0]
	orders := spanNamed(t, trace, "load orders")
	user := spanNamed(t, trace, "load user")

	if orders["kind"] != "database" {
		t.Errorf("kind = %v, want database", orders["kind"])
	}
	data := orders["data"].(map[string]any)
	if data["db.statement"] != "SELECT * FROM orders WHERE email = ? AND total > ?" {
		t.Errorf("db.statement = %v", data["db.statement"])
	}
	if data["db.system"] != "postgresql" {
		t.Errorf("db.system = %v, want postgresql", data["db.system"])
	}
	userData := user["data"].(map[string]any)
	if userData["db.statement"] != "SELECT name FROM users WHERE id = ?" {
		t.Errorf("db.statement = %v", userData["db.statement"])
	}
	if _, ok := userData["db.system"]; ok {
		t.Errorf("db.system should be left out when empty, got %v", userData["db.system"])
	}
	if user["parent_span_id"] != orders["span_id"] {
		t.Errorf("load user parent = %v, want load orders", user["parent_span_id"])
	}

	payload, _ := json.Marshal(trace)
	for _, literal := range []string{"jane@example.com", "4200", "9911"} {
		if strings.Contains(string(payload), literal) {
			t.Errorf("payload contains the literal %q: %s", literal, payload)
		}
	}
}

func TestAStatementPutInDataByHandIsMaskedOnADatabaseSpanOnly(t *testing.T) {
	server := newSpansServer(t)
	initTracing(t, server, nil)

	start := time.Now()
	ctx := WithTrace(context.Background())
	handMade := map[string]any{"db.statement": "DELETE FROM carts WHERE token = 'abc123'", "rows": 3}
	RecordSpan(ctx, "clear cart", "database", time.Now(), time.Millisecond, handMade)
	RecordSpan(ctx, "not a string", "database", time.Now(), time.Millisecond, map[string]any{"db.statement": 42})
	FinishTrace(ctx, "POST /checkout", start, 1500*time.Millisecond)

	trace := server.waitForTraces(t, 1)[0]
	data := spanNamed(t, trace, "clear cart")["data"].(map[string]any)
	if data["db.statement"] != "DELETE FROM carts WHERE token = ?" {
		t.Errorf("db.statement = %v", data["db.statement"])
	}
	if data["rows"] != float64(3) {
		t.Errorf("other data keys must be kept, got %v", data)
	}
	if handMade["db.statement"] != "DELETE FROM carts WHERE token = 'abc123'" {
		t.Errorf("the caller's own map was modified: %v", handMade)
	}
	if _, ok := spanNamed(t, trace, "not a string")["data"].(map[string]any)["db.statement"]; ok {
		t.Error("a statement that isn't a string should be dropped")
	}
}

func TestALongStatementIsTruncated(t *testing.T) {
	server := newSpansServer(t)
	initTracing(t, server, nil)

	start := time.Now()
	ctx := WithTrace(context.Background())
	query := "SELECT " + strings.Repeat("column_name, ", 500) + "id FROM orders"
	RecordDatabaseSpan(ctx, "wide select", query, "mysql", time.Now(), time.Millisecond)
	FinishTrace(ctx, "GET /orders", start, 1500*time.Millisecond)

	trace := server.waitForTraces(t, 1)[0]
	statement := spanNamed(t, trace, "wide select")["data"].(map[string]any)["db.statement"].(string)
	if len(statement) != 4003 || !strings.HasSuffix(statement, "...") {
		t.Errorf("len = %d, want 4000 characters plus ...", len(statement))
	}
}

func TestDatabaseSpanDataLeavesOutBlankValues(t *testing.T) {
	if data := DatabaseSpanData("  ", ""); len(data) != 0 {
		t.Errorf("DatabaseSpanData(blank) = %v, want empty", data)
	}
	if data := DatabaseSpanData("SELECT 1", " SQLite "); data["db.system"] != "sqlite" || data["db.statement"] != "SELECT 1" {
		t.Errorf("DatabaseSpanData = %v", data)
	}
}

func TestDatabaseSpansAreNoOpsWithoutATrace(t *testing.T) {
	ctx, end := StartDatabaseSpan(context.Background(), "no trace", "SELECT 1", "postgresql")
	end()
	RecordDatabaseSpan(ctx, "no trace", "SELECT 1", "postgresql", time.Now(), time.Millisecond)
	if traceFromContext(ctx) != nil {
		t.Error("a database span must never fabricate a trace")
	}
}
