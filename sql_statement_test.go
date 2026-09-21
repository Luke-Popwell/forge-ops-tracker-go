package forgeops

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type fakeDatabaseError struct{ statement string }

func (e *fakeDatabaseError) Error() string        { return "boom" }
func (e *fakeDatabaseError) SQLStatement() string { return e.statement }

func TestFindSQLReadsWithSQLAndCustomTypesAndFollowsTheWrappingChain(t *testing.T) {
	if got := findSQL(WithSQL(errors.New("boom"), "SELECT 1")); got != "SELECT 1" {
		t.Errorf("WithSQL: got %q", got)
	}
	if got := findSQL(&fakeDatabaseError{statement: "SELECT 2"}); got != "SELECT 2" {
		t.Errorf("custom type: got %q", got)
	}
	wrapped := fmt.Errorf("refund failed: %w", WithSQL(errors.New("db"), "CALL refund_order(1)"))
	if got := findSQL(wrapped); got != "CALL refund_order(1)" {
		t.Errorf("wrapped: got %q", got)
	}
	joined := errors.Join(errors.New("a"), WithSQL(errors.New("db"), "SELECT 3"))
	if got := findSQL(joined); got != "SELECT 3" {
		t.Errorf("joined: got %q", got)
	}
	if got := findSQL(errors.New("nope")); got != "" {
		t.Errorf("no SQL: got %q", got)
	}
	if got := findSQL(WithSQL(errors.New("blank"), "  ")); got != "" {
		t.Errorf("blank: got %q", got)
	}
}

func TestWithSQLKeepsTheOriginalErrorReachableAndPassesNilThrough(t *testing.T) {
	original := errors.New("boom")
	wrapped := WithSQL(original, "SELECT 1")
	if !errors.Is(wrapped, original) || wrapped.Error() != "boom" {
		t.Errorf("original error not reachable through WithSQL: %v", wrapped)
	}
	if WithSQL(nil, "SELECT 1") != nil {
		t.Error("WithSQL(nil, ...) should be nil")
	}
}

func TestMaskSQL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT * FROM orders2 WHERE email = 'a@b.co' AND id = 42 AND x = $1", "SELECT * FROM orders2 WHERE email = ? AND id = ? AND x = $1"},
		{"SELECT price * 1.5 FROM t WHERE a IN (1,2,3)", "SELECT price * ? FROM t WHERE a IN (?,?,?)"},
		{"EXEC sp_x @t = 'it''s'", "EXEC sp_x @t = ?"},
		{"SELECT 1 WHERE n = 'oops", "SELECT ? WHERE n = ?"},
		{"DO $b$ BEGIN PERFORM 1; END $b$", "DO ?"},
		{"SELECT 1.5x FROM t", "SELECT ?.5x FROM t"},
		{"SELECT a FROM sp_v2 WHERE b = 12abc", "SELECT a FROM sp_v2 WHERE b = 12abc"},
	}
	for _, c := range cases {
		if got := maskSQL(c.in); got != c.want {
			t.Errorf("maskSQL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMaskSQLIsIdempotentTruncatesAndReturnsEmptyForBlank(t *testing.T) {
	once := maskSQL("SELECT * FROM t WHERE a = 'x' AND b = 9")
	if maskSQL(once) != once {
		t.Errorf("not idempotent: %q", maskSQL(once))
	}
	if got := len([]rune(maskSQL("SELECT " + strings.Repeat("a, ", 3000) + " b"))); got != maxSQLLength+3 {
		t.Errorf("truncated length = %d", got)
	}
	if maskSQL("  ") != "" {
		t.Error("blank should mask to empty")
	}
}

func TestExtractSQLObjects(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want *SQLObjects
	}{
		{"exec with schema", "EXEC dbo.sp_refund_order @id = ?", &SQLObjects{Operation: "EXEC", Procedures: []string{"dbo.sp_refund_order"}, Relations: []string{}}},
		{"call", "CALL refund_order(?, ?)", &SQLObjects{Operation: "CALL", Procedures: []string{"refund_order"}, Relations: []string{}}},
		{"select of a function", "SELECT refund_order(?, ?)", &SQLObjects{Operation: "SELECT", Procedures: []string{"refund_order"}, Relations: []string{}}},
		{"view and joined table", "SELECT * FROM v_totals t JOIN public.customers c ON c.id = t.id", &SQLObjects{Operation: "SELECT", Procedures: []string{}, Relations: []string{"v_totals", "public.customers"}}},
		{"table function", "SELECT * FROM get_open_orders(?) o", &SQLObjects{Operation: "SELECT", Procedures: []string{"get_open_orders"}, Relations: []string{}}},
		{"insert column list", "INSERT INTO audit_log (a) VALUES (?)", &SQLObjects{Operation: "INSERT", Procedures: []string{}, Relations: []string{"audit_log"}}},
		{"builtin", "SELECT count(*) FROM orders", &SQLObjects{Operation: "SELECT", Procedures: []string{}, Relations: []string{"orders"}}},
		{"from inside extract", "SELECT 1 FROM orders WHERE extract(year FROM created_at) = ?", &SQLObjects{Operation: "SELECT", Procedures: []string{}, Relations: []string{"orders"}}},
		{"quoted identifiers", `UPDATE "Order Items" SET qty = ?`, &SQLObjects{Operation: "UPDATE", Procedures: []string{}, Relations: []string{`"Order Items"`}}},
		{"bracketed", "INSERT INTO [dbo].[audit_log] (a) VALUES (?)", &SQLObjects{Operation: "INSERT", Procedures: []string{}, Relations: []string{"[dbo].[audit_log]"}}},
		{"garbage", "garbage", nil},
	}
	for _, c := range cases {
		got := extractSQLObjects(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: extractSQLObjects(%q) = %+v, want %+v", c.name, c.in, got, c.want)
		}
	}
}

func TestEventBuilderSendsProcedureNameByDefaultAndStatementOnlyWhenOptedIn(t *testing.T) {
	err := WithSQL(errors.New("boom"), "EXEC dbo.sp_refund_order @order_id = 8814, @note = 'a@b.co'")
	config := testConfiguration()
	config.CaptureSQLObjects = true

	payload := NewEventBuilder(config).Build(err, nil, nil, nil, nil)
	objects, ok := payload["sql_objects"].(*SQLObjects)
	if !ok || !reflect.DeepEqual(objects.Procedures, []string{"dbo.sp_refund_order"}) {
		t.Fatalf("sql_objects = %#v", payload["sql_objects"])
	}
	if _, present := payload["sql_statement"]; present {
		t.Error("sql_statement should be absent by default")
	}

	config.CaptureSQLStatement = true
	payload = NewEventBuilder(config).Build(err, nil, nil, nil, nil)
	if payload["sql_statement"] != "EXEC dbo.sp_refund_order @order_id = ?, @note = ?" {
		t.Errorf("sql_statement = %#v", payload["sql_statement"])
	}

	config.CaptureSQLObjects = false
	config.CaptureSQLStatement = false
	payload = NewEventBuilder(config).Build(err, nil, nil, nil, nil)
	if _, present := payload["sql_objects"]; present {
		t.Error("sql_objects should be absent when both are off")
	}
	config.CaptureSQLObjects = true
	if _, present := NewEventBuilder(config).Build(errors.New("nope"), nil, nil, nil, nil)["sql_objects"]; present {
		t.Error("an error with no SQL should add nothing")
	}
}

func TestDefaultConfigurationCapturesObjectsButNotTheStatement(t *testing.T) {
	config := NewConfiguration()
	if !config.CaptureSQLObjects || config.CaptureSQLStatement {
		t.Errorf("defaults: objects=%v statement=%v", config.CaptureSQLObjects, config.CaptureSQLStatement)
	}
}
