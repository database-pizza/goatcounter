package pizzasql

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"zgo.at/zdb"
	"zgo.at/zdb/drivers"

	// The zdb go-sqlite3 driver is registered so a plain sqlite connect string
	// that reaches this adapter can be delegated without opening a local file
	// by accident.
	_ "zgo.at/zdb-drivers/go-sqlite3"
)

// Pool bounds for the managed service. The pgproxy enforces a per-organization
// connection quota, so a GoatCounter process keeps a deliberately small pool.
// GoatCounter passes these through its -dbconn flag, which defaults to 2,2 for
// PizzaSQL connections.
const (
	MaxOpenConns = 2
	MaxIdleConns = 2
)

// Register adds the PizzaSQL zdb driver. It is idempotent.
//
// Registering adds a second driver for the "sqlite" dialect, so it is only done
// when a connect string actually targets PizzaSQL. The registered driver still
// delegates ordinary `sqlite+...` connects to go-sqlite3, so a process that
// registered the driver can never have a local SQLite connection hijacked by
// it.
func Register() {
	registerOnce.Do(func() {
		drivers.RegisterDriver(zdbDriver{})
	})
}

var registerOnce sync.Once

// IsConnectString reports whether a zdb connect string targets PizzaSQL, for
// example "sqlite/pizzasql+host=127.0.0.1 ..." or "pizzasql+host=...".
func IsConnectString(connect string) bool {
	head, _, found := strings.Cut(connect, "+")
	if !found {
		head, _, _ = strings.Cut(connect, "://")
	}
	return head == DriverName || strings.HasSuffix(head, "/"+DriverName)
}

// zdbDriver adapts the database/sql driver above to zdb. It reports the SQLite
// dialect so zdb selects SQLite schema, migrations, and query files while the
// transport remains PostgreSQL.
type zdbDriver struct{}

var (
	_ drivers.Driver = zdbDriver{}
)

func (zdbDriver) Name() string { return DriverName }

func (zdbDriver) Dialect() string { return "sqlite" }

// Match recognizes the explicit "sqlite/pizzasql+" and "pizzasql+" forms. zdb
// appends the driver name to the connect string when this matches, so Connect
// strips exactly that suffix again.
func (zdbDriver) Match(dialect, driver string) bool {
	if driver != DriverName {
		return false
	}
	return strings.EqualFold(dialect, "sqlite") || strings.EqualFold(dialect, "(unknown)")
}

func (zdbDriver) Connect(ctx context.Context, connect string, create bool) (*sql.DB, any, error) {
	// Only strip the exact suffix zdb adds after a Match(); a "+++" that
	// happens to appear inside a DSN or file path is left alone.
	if dsn, ok := stripPizzaMarker(connect); ok {
		return connectPizzaSQL(ctx, dsn)
	}

	sqlite := findSQLiteDriver()
	if sqlite == nil {
		return nil, nil, errors.New("pizzasql: no go-sqlite3 driver is registered to handle a local SQLite connection")
	}
	return sqlite.Connect(ctx, connect, create)
}

// stripPizzaMarker removes the trailing "+++pizzasql" marker zdb appends after a
// Match(), and reports whether the connect string carried it. Only the suffix
// is considered, so a "+++" inside a DSN, password, or file path never changes
// the result.
func stripPizzaMarker(connect string) (string, bool) {
	return strings.CutSuffix(connect, "+++"+DriverName)
}

func connectPizzaSQL(ctx context.Context, dsn string) (*sql.DB, any, error) {
	db, err := sql.Open(DriverName, dsn)
	if err != nil {
		return nil, nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, nil, err
	}

	// Probe the scalar feature set once per pool. Every probe is read-only, so
	// startup never needs write or alter_table scope. Behavioural features
	// (RETURNING, generated columns) are verified only by integration tests.
	if _, err := CheckCapabilities(ctx, db); err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return db, nil, nil
}

func findSQLiteDriver() drivers.Driver {
	for _, d := range drivers.Drivers() {
		if d.Name() == "sqlite3" {
			return d
		}
	}
	return nil
}

// ErrUnique reports a PizzaSQL unique constraint violation. The engine returns
// SQLite's "UNIQUE constraint failed" text wrapped in a generic SQLSTATE. The
// match is deliberately narrow so unrelated errors that merely contain
// "duplicate" are not mistaken for unique violations.
func (zdbDriver) ErrUnique(err error) bool {
	if err == nil {
		return false
	}
	if strings.Contains(strings.ToLower(err.Error()), "unique constraint failed") {
		return true
	}
	if d := findSQLiteDriver(); d != nil {
		return d.ErrUnique(err)
	}
	return false
}

// StartTest starts a test database for zdb.RunTest. Without PIZZASQL_TEST_DSN
// the driver skips, so importing this package never turns local SQLite tests
// into remote ones.
func (zdbDriver) StartTest(t *testing.T, opt *drivers.TestOptions) context.Context {
	t.Helper()

	connect := os.Getenv("PIZZASQL_TEST_DSN")
	copt := zdb.ConnectOptions{
		Create:       true,
		MaxOpenConns: MaxOpenConns,
		MaxIdleConns: MaxIdleConns,
	}
	if opt != nil {
		if opt.Connect != "" {
			connect = opt.Connect
		}
		copt.Files = opt.Files
		copt.GoMigrations = opt.GoMigrations
	}
	if connect == "" {
		t.Skip("PIZZASQL_TEST_DSN not set; skipping PizzaSQL integration test")
	}
	copt.Connect = pizzaConnectString(connect)

	db, err := zdb.Connect(context.Background(), copt)
	if err != nil {
		var unsup *UnsupportedError
		if errors.As(err, &unsup) {
			t.Skipf("engine cannot run GoatCounter: %s", err)
		}
		t.Fatalf("pizzasql.StartTest: %s", RedactError(copt.Connect, err))
	}
	t.Cleanup(func() { _ = db.Close() })
	return zdb.WithDB(context.Background(), db)
}

// pizzaConnectString turns a raw DSN into a zdb connect string, and leaves an
// already-qualified string alone so it is never double-prefixed.
func pizzaConnectString(connect string) string {
	if IsConnectString(connect) {
		return connect
	}
	return "sqlite/" + DriverName + "+" + connect
}
