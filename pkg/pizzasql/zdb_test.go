package pizzasql

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"zgo.at/zdb"
)

func TestIsConnectString(t *testing.T) {
	tests := map[string]bool{
		"sqlite/pizzasql+host=127.0.0.1":   true,
		"sqlite3/pizzasql+host=127.0.0.1":  true,
		"pizzasql+host=127.0.0.1":          true,
		"sqlite/pizzasql://host=127.0.0.1": true,
		"sqlite+./db.sqlite3":              false,
		"sqlite3+:memory:":                 false,
		"postgresql+dbname=x":              false,
	}
	for in, want := range tests {
		if got := IsConnectString(in); got != want {
			t.Errorf("IsConnectString(%q) = %v; want %v", in, got, want)
		}
	}
}

func TestZDBDriverMatch(t *testing.T) {
	d := zdbDriver{}
	if !d.Match("SQLite", "pizzasql") {
		t.Error("must match the explicit sqlite/pizzasql form")
	}
	if d.Match("SQLite", "") {
		t.Error("must not match a plain sqlite connect string")
	}
	if d.Match("SQLite", "sqlite3") {
		t.Error("must not match the go-sqlite3 driver")
	}
	if d.Match("PostgreSQL", "pizzasql") {
		t.Error("must not match a PostgreSQL dialect")
	}
}

func TestRegisterIdempotent(t *testing.T) {
	Register()
	Register()
}

func TestZDBCanResolveDriver(t *testing.T) {
	Register()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := zdb.Connect(ctx, zdb.ConnectOptions{
		Connect: "sqlite/pizzasql+host=127.0.0.1 port=1 user=u sslmode=disable",
	})
	if err == nil {
		t.Fatal("expected the connection to fail")
	}
	if strings.Contains(err.Error(), "no driver found") {
		t.Fatalf("zdb did not resolve the pizzasql driver: %v", err)
	}
}

// TestZDBCanResolveUnknownDialect checks the "pizzasql+..." form, which zdb
// resolves through Match because the dialect prefix is unknown.
func TestZDBCanResolveUnknownDialect(t *testing.T) {
	Register()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := zdb.Connect(ctx, zdb.ConnectOptions{
		Connect: "pizzasql+host=127.0.0.1 port=1 user=u sslmode=disable",
	})
	if err == nil {
		t.Fatal("expected the connection to fail")
	}
	if strings.Contains(err.Error(), "no driver found") {
		t.Fatalf("zdb did not resolve the pizzasql driver: %v", err)
	}
}

// TestZDBDriverDoesNotHijackPlainSQLite proves that once the PizzaSQL driver is
// registered, an ordinary sqlite+ connection still opens a real local SQLite
// database instead of being routed to PizzaSQL.
func TestZDBDriverDoesNotHijackPlainSQLite(t *testing.T) {
	Register()

	path := filepath.Join(t.TempDir(), "local.sqlite3")
	ctx := zdb.WithDB(context.Background(), mustConnectSQLite(t, path))

	if err := zdb.Exec(ctx, "create table t (id integer primary key, v text)"); err != nil {
		t.Fatal(err)
	}
	if err := zdb.Exec(ctx, "insert into t (v) values (?)", "local"); err != nil {
		t.Fatal(err)
	}
	var v string
	if err := zdb.Get(ctx, &v, "select v from t"); err != nil {
		t.Fatal(err)
	}
	if v != "local" {
		t.Fatalf("plain sqlite+ was hijacked: v = %q", v)
	}
}

func mustConnectSQLite(t *testing.T, path string) zdb.DB {
	t.Helper()
	db, err := zdb.Connect(context.Background(), zdb.ConnectOptions{
		Connect: "sqlite+" + path,
		Create:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestZDBDriverDelegatesPlainSQLite exercises the delegation directly so the
// guarantee does not depend on zdb's map iteration order.
func TestZDBDriverDelegatesPlainSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delegate.sqlite3")
	db, _, err := zdbDriver{}.Connect(context.Background(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if _, err := db.Exec("create table t (id integer primary key, v text)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("insert into t (v) values (?)", "x"); err != nil {
		t.Fatal(err)
	}
	var v string
	if err := db.QueryRow("select v from t").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != "x" {
		t.Fatalf("v = %q", v)
	}
}

// TestStripPizzaMarker checks that only a trailing +++pizzasql marker is
// stripped; the marker text earlier in a DSN or path is left alone.
func TestStripPizzaMarker(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"host=h password=p+++extra+++pizzasql", "host=h password=p+++extra", true},
		{"host=h password=p+++pizzasql", "host=h password=p", true},
		{"has+++pizzasql+++inside.sqlite3", "", false},
		{"plain.sqlite3", "", false},
		{"+++pizzasql", "", true},
	}
	for _, tt := range tests {
		got, ok := stripPizzaMarker(tt.in)
		if ok != tt.ok {
			t.Errorf("stripPizzaMarker(%q) ok = %v; want %v", tt.in, ok, tt.ok)
			continue
		}
		if ok && got != tt.want {
			t.Errorf("stripPizzaMarker(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

func TestPizzaConnectString(t *testing.T) {
	tests := map[string]string{
		"host=h password=p":           "sqlite/pizzasql+host=h password=p",
		"sqlite/pizzasql+host=h":      "sqlite/pizzasql+host=h",
		"sqlite3/pizzasql+host=h":     "sqlite3/pizzasql+host=h",
		"pizzasql+host=h":             "pizzasql+host=h",
		"postgres://u:p@127.0.0.1/db": "sqlite/pizzasql+postgres://u:p@127.0.0.1/db",
	}
	for in, want := range tests {
		if got := pizzaConnectString(in); got != want {
			t.Errorf("pizzaConnectString(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestErrUniqueNarrow(t *testing.T) {
	d := zdbDriver{}
	if !d.ErrUnique(errors.New("Execution error: UNIQUE constraint failed: users.email")) {
		t.Error("must match a unique constraint violation")
	}
	if d.ErrUnique(errors.New("duplicate column name: email")) {
		t.Error("must not match a duplicate-column error")
	}
	if d.ErrUnique(errors.New("some other error")) {
		t.Error("must not match an unrelated error")
	}
	if d.ErrUnique(nil) {
		t.Error("nil must not be a unique violation")
	}
}
