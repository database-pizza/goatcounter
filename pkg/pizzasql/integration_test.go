package pizzasql_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"zgo.at/goatcounter/v2/pkg/pizzasql"
)

// openIntegration connects to the engine named by PIZZASQL_TEST_DSN and skips
// the test when the engine cannot satisfy GoatCounter's required capabilities.
// The DSN must point at a disposable database: the test creates and drops its
// own uniquely named table but never touches other tables.
func openIntegration(t *testing.T) *sql.DB {
	t.Helper()

	dsn := os.Getenv("PIZZASQL_TEST_DSN")
	if dsn == "" {
		t.Skip("PIZZASQL_TEST_DSN not set; skipping PizzaSQL integration test")
	}

	db, err := sql.Open(pizzasql.DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Ping(); err != nil {
		t.Fatalf("connect to PizzaSQL: %s", err)
	}
	if _, err := pizzasql.CheckCapabilities(context.Background(), db); err != nil {
		var unsup *pizzasql.UnsupportedError
		if errors.As(err, &unsup) {
			t.Skipf("engine cannot run GoatCounter: %s", err)
		}
		t.Fatalf("capability probe: %s", err)
	}
	// RETURNING and stored generated columns can only be checked by executing
	// DDL/DML, so verify them here rather than at connect time.
	if _, err := pizzasql.VerifyBehavior(context.Background(), db); err != nil {
		var unsup *pizzasql.UnsupportedError
		if errors.As(err, &unsup) {
			t.Skipf("engine cannot run GoatCounter: %s", err)
		}
		t.Fatalf("behaviour probe: %s", err)
	}
	return db
}

func TestIntegrationDriverWorkflow(t *testing.T) {
	db := openIntegration(t)
	table := fmt.Sprintf("pizzasql_it_%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = db.Exec("drop table if exists " + table) })

	// Multi-statement schema Exec, as zdb runs db/schema.gotxt.
	schema := "create table " + table + ` (
		id      integer primary key autoincrement,
		name    text,
		created timestamp,
		raw     blob,
		stored  integer generated always as (id + 1) stored
	);
	create index ` + table + `_name on ` + table + ` (name);`
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("multi-statement schema: %s", err)
	}

	now := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	res, err := db.Exec("insert into "+table+" (name, created, raw) values (?, ?, ?) returning id",
		"alpha", now, []byte{0x00, 0xff, 0x10})
	if err != nil {
		t.Fatalf("insert returning: %s", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	var (
		gotName string
		gotTime time.Time
		gotRaw  []byte
		gotGen  int64
	)
	err = db.QueryRow("select name, created, raw, stored from "+table+" where id = ?", id).
		Scan(&gotName, &gotTime, &gotRaw, &gotGen)
	if err != nil {
		t.Fatalf("select: %s", err)
	}
	if gotName != "alpha" {
		t.Errorf("name = %q", gotName)
	}
	if !gotTime.Equal(now) {
		t.Errorf("created = %v; want %v", gotTime, now)
	}
	if string(gotRaw) != string([]byte{0x00, 0xff, 0x10}) {
		t.Errorf("raw = %x", gotRaw)
	}
	if gotGen != id+1 {
		t.Errorf("stored = %d; want %d", gotGen, id+1)
	}

	rows, err := db.Query("select id, name from " + table + " where id = -1")
	if err != nil {
		t.Fatalf("empty select: %s", err)
	}
	columns, err := rows.Columns()
	_ = rows.Close()
	if err != nil {
		t.Fatalf("empty select columns: %s", err)
	}
	if len(columns) != 2 || columns[0] != "id" || columns[1] != "name" {
		t.Errorf("empty select columns = %v; want [id name]", columns)
	}

	// Plain insert must recover the session-local last insert ID.
	res, err = db.Exec("insert into "+table+" (name) values (?)", "beta")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if id2 == 0 || id2 == id {
		t.Errorf("last insert ID = %d; want a new non-zero id", id2)
	}

	// JSON functions and percent_diff used by GoatCounter.
	var (
		doc string
		ext string
		pct float64
	)
	if err := db.QueryRow(`select json('[1,2]'), json_extract('{"a":1}', '$.a'), percent_diff(1, 2)`).
		Scan(&doc, &ext, &pct); err != nil {
		t.Fatalf("json/percent_diff: %s", err)
	}
	if doc != "[1,2]" || ext != "1" || pct != 100 {
		t.Errorf("json/percent_diff = %q, %q, %v", doc, ext, pct)
	}

	// Transactions.
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("insert into "+table+" (name) values (?)", "rolled-back"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("select count(*) from "+table+" where name = ?", "rolled-back").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("rolled-back row count = %d; want 0", count)
	}
}
