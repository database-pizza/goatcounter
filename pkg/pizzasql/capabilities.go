package pizzasql

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Capabilities describes the scalar engine features GoatCounter requires.
// Every probe is a read-only statement, so checking them never mutates the
// tenant and never needs write or alter_table scope.
type Capabilities struct {
	// SQLiteVersion is the raw string returned by sqlite_version().
	SQLiteVersion string
	// VersionAtLeast335 reports whether SQLiteVersion parsed as >= 3.35.
	VersionAtLeast335 bool
	// LastInsertRowID is a session-local last_insert_rowid().
	LastInsertRowID bool
	// JSON supports the json()/json_extract() family used by GoatCounter.
	JSON bool
	// PercentDiff provides the percent_diff() function.
	PercentDiff bool
}

// Missing returns a human-readable list of the scalar capabilities that are
// required but unavailable.
func (c Capabilities) Missing() []string {
	var m []string
	if !c.VersionAtLeast335 {
		m = append(m, "sqlite_version >= 3.35 (engine reported "+strconv.Quote(c.SQLiteVersion)+")")
	}
	if !c.LastInsertRowID {
		m = append(m, "session-local last_insert_rowid()")
	}
	if !c.JSON {
		m = append(m, "JSON functions")
	}
	if !c.PercentDiff {
		m = append(m, "percent_diff()")
	}
	return m
}

// OK reports whether every scalar capability is available.
func (c Capabilities) OK() bool { return len(c.Missing()) == 0 }

// UnsupportedError reports engine capabilities that are missing.
type UnsupportedError struct {
	Missing []string
}

func (e *UnsupportedError) Error() string {
	return "pizzasql: engine is missing required capabilities: " + strings.Join(e.Missing, ", ")
}

// CheckCapabilities probes db for the scalar features GoatCounter needs and
// returns the observed capabilities. It only runs read-only statements
// (`select sqlite_version()`, `last_insert_rowid()`, JSON functions, and
// `percent_diff`), so it is safe on every pooled connection and startup and
// does not require write or alter_table scope.
//
// Behavioural features such as RETURNING and stored generated columns cannot
// be probed without executing DDL/DML, so they are verified separately by
// VerifyBehavior from explicit integration tests.
func CheckCapabilities(ctx context.Context, db *sql.DB) (Capabilities, error) {
	var caps Capabilities

	conn, err := db.Conn(ctx)
	if err != nil {
		return caps, err
	}
	defer conn.Close()

	caps.SQLiteVersion, err = queryString(ctx, conn, "select sqlite_version()")
	if err != nil {
		return caps, errors.New("pizzasql: probing sqlite_version(): " + err.Error())
	}
	caps.VersionAtLeast335 = versionAtLeast(caps.SQLiteVersion, 3, 35)

	var last int64
	caps.LastInsertRowID = conn.QueryRowContext(ctx, "select last_insert_rowid()").Scan(&last) == nil

	caps.JSON = probeJSON(ctx, conn)
	caps.PercentDiff = probePercentDiff(ctx, conn)

	if missing := caps.Missing(); len(missing) > 0 {
		return caps, &UnsupportedError{Missing: missing}
	}
	return caps, nil
}

func queryString(ctx context.Context, conn *sql.Conn, query string) (string, error) {
	var s sql.NullString
	if err := conn.QueryRowContext(ctx, query).Scan(&s); err != nil {
		return "", err
	}
	return s.String, nil
}

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)`)

func versionAtLeast(v string, major, minor int) bool {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return false
	}
	vmaj, _ := strconv.Atoi(m[1])
	vmin, _ := strconv.Atoi(m[2])
	if vmaj != major {
		return vmaj > major
	}
	return vmin >= minor
}

func probeJSON(ctx context.Context, conn *sql.Conn) bool {
	var doc, extracted string
	err := conn.QueryRowContext(ctx,
		`select json('[1,2]'), json_extract('{"a":1}', '$.a')`).Scan(&doc, &extracted)
	return err == nil && doc == "[1,2]" && extracted == "1"
}

func probePercentDiff(ctx context.Context, conn *sql.Conn) bool {
	var diff sql.NullFloat64
	err := conn.QueryRowContext(ctx, "select percent_diff(1, 2)").Scan(&diff)
	return err == nil && diff.Valid
}

// BehaviorCapabilities are engine features that can only be verified by
// creating a table, so they are never probed at connect time. Callers that
// want the guarantee (integration tests, release verification) run
// VerifyBehavior explicitly against a disposable database.
type BehaviorCapabilities struct {
	// Returning passes through INSERT/UPDATE/DELETE ... RETURNING.
	Returning bool
	// GeneratedColumns supports `generated always as (...) stored`.
	GeneratedColumns bool
}

// OK reports whether every behavioural feature is available.
func (b BehaviorCapabilities) OK() bool { return b.Returning && b.GeneratedColumns }

// Missing returns the behavioural features that are unavailable.
func (b BehaviorCapabilities) Missing() []string {
	var m []string
	if !b.Returning {
		m = append(m, "RETURNING")
	}
	if !b.GeneratedColumns {
		m = append(m, "stored generated columns")
	}
	return m
}

// VerifyBehavior checks the features that need DDL/DML by creating and
// dropping uniquely named tables. It mutates the database, so it is intended
// only for explicit integration tests against a disposable database and is
// never called at connect time.
func VerifyBehavior(ctx context.Context, db *sql.DB) (BehaviorCapabilities, error) {
	var caps BehaviorCapabilities

	conn, err := db.Conn(ctx)
	if err != nil {
		return caps, err
	}
	defer conn.Close()

	name := "pizzasql_behavior_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	caps.Returning = probeReturning(ctx, conn, name+"_r")
	caps.GeneratedColumns = probeGenerated(ctx, conn, name+"_g")

	if missing := caps.Missing(); len(missing) > 0 {
		return caps, &UnsupportedError{Missing: missing}
	}
	return caps, nil
}

func probeReturning(ctx context.Context, conn *sql.Conn, table string) bool {
	defer func() { _, _ = conn.ExecContext(context.Background(), "drop table if exists "+table) }()

	_, err := conn.ExecContext(ctx,
		"create table "+table+" (id integer primary key autoincrement, v text)")
	if err != nil {
		return false
	}

	var id int64
	err = conn.QueryRowContext(ctx,
		"insert into "+table+" (v) values ('probe') returning id").Scan(&id)
	return err == nil && id != 0
}

func probeGenerated(ctx context.Context, conn *sql.Conn, table string) bool {
	defer func() { _, _ = conn.ExecContext(context.Background(), "drop table if exists "+table) }()

	_, err := conn.ExecContext(ctx,
		"create table "+table+" (a integer, b integer generated always as (a + 1) stored)")
	if err != nil {
		return false
	}
	_, err = conn.ExecContext(ctx, "insert into "+table+" (a) values (1)")
	if err != nil {
		return false
	}

	var b int64
	err = conn.QueryRowContext(ctx, "select b from "+table).Scan(&b)
	return err == nil && b == 2
}
