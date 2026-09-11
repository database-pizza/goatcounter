// Package pizzasql provides a database/sql driver for the database.pizza
// managed PizzaSQL service.
//
// The driver speaks the PostgreSQL simple-query protocol (through pgx's
// pgconn) but emits SQLite-dialect SQL, which is what PizzaSQL executes. A
// PizzaSQL connection never opens a local SQLite file: every connection goes to
// a PizzaSQL tenant, normally through the app.database.pizza PostgreSQL proxy on
// a trusted or loopback hop. The zdb adapter in zdb.go also delegates ordinary
// `sqlite+...` connects that reach it to the go-sqlite3 driver, so registering
// it cannot hijack a local SQLite connection.
//
// Simple query protocol has no bind parameters, so ? placeholders are inlined
// as SQLite literals. Parameter binding is quote- and comment-aware; see
// bind.go. Values are decoded on the way back so time, NULL, and bytea columns
// round-trip through database/sql. The driver deliberately does not emulate
// RETURNING or rewrite statements: the engine sees exactly what the caller
// wrote, and CheckCapabilities reports when it cannot satisfy GoatCounter.
package pizzasql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// DriverName is the database/sql driver name registered by this package.
const DriverName = "pizzasql"

// Connection lifecycle timeouts. They bound dialing and teardown so a hung
// engine cannot hold a pooled connection or a caller forever.
const (
	connectTimeout = 10 * time.Second
	closeTimeout   = 5 * time.Second
	finishTimeout  = 30 * time.Second
)

// PostgreSQL data type OIDs used to convert wire values into Go values. The
// engine reports a full OID for the types GoatCounter relies on; anything else
// stays a string for database/sql to convert.
const (
	oidBytea       = 17
	oidDate        = 1082
	oidTimestamp   = 1114
	oidTimestamptz = 1184
	oidTime        = 1266
)

func init() {
	sql.Register(DriverName, &pizzaDriver{})
}

var (
	_ driver.Driver           = (*pizzaDriver)(nil)
	_ driver.DriverContext    = (*pizzaDriver)(nil)
	_ driver.Connector        = (*connector)(nil)
	_ driver.Conn             = (*connection)(nil)
	_ driver.ConnBeginTx      = (*connection)(nil)
	_ driver.ExecerContext    = (*connection)(nil)
	_ driver.QueryerContext   = (*connection)(nil)
	_ driver.Pinger           = (*connection)(nil)
	_ driver.SessionResetter  = (*connection)(nil)
	_ driver.Validator        = (*connection)(nil)
	_ driver.Stmt             = (*statement)(nil)
	_ driver.StmtExecContext  = (*statement)(nil)
	_ driver.StmtQueryContext = (*statement)(nil)
	_ driver.Tx               = (*transaction)(nil)
	_ driver.Result           = result{}
	_ driver.Rows             = (*rows)(nil)
)

type pizzaDriver struct{}

type connector struct {
	config *pgconn.Config
}

type connection struct {
	pg *pgconn.PgConn
}

type statement struct {
	c     *connection
	query string
}

type transaction struct {
	c *connection
}

type result struct {
	id, affected int64
}

type rows struct {
	fields []pgconn.FieldDescription
	values [][][]byte
	pos    int
}

func (d *pizzaDriver) Open(dsn string) (driver.Conn, error) {
	c, err := d.OpenConnector(dsn)
	if err != nil {
		return nil, err
	}
	return c.Connect(context.Background())
}

func (*pizzaDriver) OpenConnector(dsn string) (driver.Connector, error) {
	config, err := pgconn.ParseConfig(dsn)
	if err != nil {
		// ParseConfigError carries the raw connection string and redaction can
		// leak the suffix of a password containing an escaped quote, so never
		// surface the original error.
		return nil, errors.New("pizzasql: invalid connection configuration")
	}
	return &connector{config: config}, nil
}

func (*connector) Driver() driver.Driver { return &pizzaDriver{} }

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	// database/sql does not impose a dial deadline, so apply a bounded one.
	// A caller-supplied deadline is respected as-is.
	connectCtx := ctx
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		connectCtx, cancel = context.WithTimeout(ctx, connectTimeout)
		defer cancel()
	}

	pg, err := pgconn.ConnectConfig(connectCtx, c.config.Copy())
	if err != nil {
		return nil, sanitizeConnectError(err)
	}

	conn := &connection{pg: pg}
	if err := conn.checkCapability(connectCtx); err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), closeTimeout)
		defer cancel()
		_ = pg.Close(closeCtx)
		return nil, err
	}
	return conn, nil
}

// sanitizeConnectError preserves an actionable SQLSTATE while dropping every
// DSN component (user, database, password) that identifies the tenant.
func sanitizeConnectError(err error) error {
	var connErr *pgconn.ConnectError
	if !errors.As(err, &connErr) {
		return errors.New("pizzasql: connection failed")
	}

	var pgErr *pgconn.PgError
	if errors.As(connErr, &pgErr) {
		return errors.New("pizzasql: connection failed (SQLSTATE " + pgErr.Code + ")")
	}
	return errors.New("pizzasql: connection failed; check address, TLS settings, and credentials")
}

// checkCapability verifies the session-local insert ID function on every new
// connection. It is cheap and catches engines that would otherwise return a
// stale or shared ID.
func (c *connection) checkCapability(ctx context.Context) error {
	results, err := c.execAll(ctx, "SELECT last_insert_rowid()", nil, true)
	if err != nil {
		return err
	}
	if len(results) != 1 || len(results[0].Rows) != 1 || len(results[0].Rows[0]) != 1 || results[0].Rows[0][0] == nil {
		return errors.New("pizzasql: engine does not support a session-local last_insert_rowid")
	}
	if _, err := strconv.ParseInt(string(results[0].Rows[0][0]), 10, 64); err != nil {
		return errors.New("pizzasql: engine returned a non-integer last_insert_rowid")
	}
	return nil
}

func (c *connection) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	return c.pg.Close(ctx)
}

func (c *connection) IsValid() bool {
	return !c.pg.IsClosed() && c.pg.TxStatus() == 'I'
}

func (c *connection) ResetSession(context.Context) error {
	if !c.IsValid() {
		return driver.ErrBadConn
	}
	return nil
}

func (c *connection) Ping(ctx context.Context) error {
	return c.checkCapability(ctx)
}

func (c *connection) Prepare(query string) (driver.Stmt, error) {
	return &statement{c: c, query: query}, nil
}

func (s *statement) Close() error  { return nil }
func (s *statement) NumInput() int { return -1 }

func named(args []driver.Value) []driver.NamedValue {
	values := make([]driver.NamedValue, len(args))
	for i, v := range args {
		values[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return values
}

func (s *statement) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), named(args))
}

func (s *statement) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), named(args))
}

func (s *statement) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.c.ExecContext(ctx, s.query, args)
}

func (s *statement) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.c.QueryContext(ctx, s.query, args)
}

func (c *connection) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *connection) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if opts.Isolation != 0 || opts.ReadOnly {
		return nil, errors.New("pizzasql: explicit transaction isolation and read-only transactions are unsupported")
	}
	if _, err := c.execAll(ctx, "BEGIN", nil, true); err != nil {
		return nil, err
	}
	return &transaction{c: c}, nil
}

func (t *transaction) finish(query string) error {
	ctx, cancel := context.WithTimeout(context.Background(), finishTimeout)
	defer cancel()
	_, err := t.c.execAll(ctx, query, nil, false)
	return err
}

func (t *transaction) Commit() error   { return t.finish("COMMIT") }
func (t *transaction) Rollback() error { return t.finish("ROLLBACK") }

// execAll runs one simple-query batch and returns every statement result. The
// simple protocol is used because PizzaSQL accepts SQLite-dialect SQL there,
// and because zdb runs multi-statement schema batches through Exec.
//
// When retryable is false no error is mapped to driver.ErrBadConn, so a caller
// that has already acknowledged a write never triggers a replay.
func (c *connection) execAll(ctx context.Context, query string, args []driver.NamedValue, retryable bool) ([]*pgconn.Result, error) {
	if c.pg.IsClosed() {
		if retryable {
			return nil, driver.ErrBadConn
		}
		return nil, errors.New("pizzasql: connection is closed")
	}

	sqlText, err := bind(query, args)
	if err != nil {
		return nil, err
	}

	mrr := c.pg.Exec(ctx, sqlText)
	var results []*pgconn.Result
	for mrr.NextResult() {
		rr := mrr.ResultReader()
		// pgconn.ResultReader.Read only copies field descriptions after the
		// first data row. Snapshot them first so an empty SELECT still exposes
		// its columns through database/sql.
		fields := append([]pgconn.FieldDescription(nil), rr.FieldDescriptions()...)
		result := rr.Read()
		if len(result.FieldDescriptions) == 0 && len(fields) > 0 {
			result.FieldDescriptions = fields
		}
		results = append(results, result)
	}
	err = mrr.Close()
	if err != nil {
		if retryable && pgconn.SafeToRetry(err) {
			return nil, driver.ErrBadConn
		}
		return nil, err
	}
	for _, r := range results {
		if r.Err != nil {
			return nil, r.Err
		}
	}
	return results, nil
}

func (c *connection) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	results, err := c.execAll(ctx, query, args, true)
	if err != nil {
		return nil, err
	}

	res := result{}
	if len(results) == 0 {
		return res, nil
	}

	last := results[len(results)-1]
	res.affected = last.CommandTag.RowsAffected()
	if last.CommandTag.Insert() && res.affected > 0 {
		// The insert has already been acknowledged, so a failure to read the
		// last insert ID must never surface driver.ErrBadConn: database/sql
		// would interpret it as retryable and replay the whole insert.
		id, err := c.lastInsertID(ctx)
		if err != nil {
			return nil, err
		}
		res.id = id
	}
	return res, nil
}

func (c *connection) lastInsertID(ctx context.Context) (int64, error) {
	results, err := c.execAll(ctx, "SELECT last_insert_rowid()", nil, false)
	if err != nil {
		return 0, errors.New("pizzasql: insert acknowledged but last insert ID could not be retrieved; the row state is uncertain: " + err.Error())
	}
	if len(results) != 1 || len(results[0].Rows) != 1 || len(results[0].Rows[0]) != 1 || results[0].Rows[0][0] == nil {
		return 0, errors.New("pizzasql: invalid last insert ID result")
	}
	id, err := strconv.ParseInt(string(results[0].Rows[0][0]), 10, 64)
	if err != nil {
		return 0, errors.New("pizzasql: server did not return an integer insert ID")
	}
	return id, nil
}

func (r result) LastInsertId() (int64, error) { return r.id, nil }
func (r result) RowsAffected() (int64, error) { return r.affected, nil }

func (c *connection) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	results, err := c.execAll(ctx, query, args, true)
	if err != nil {
		return nil, err
	}

	var res *pgconn.Result
	for _, r := range results {
		if len(r.FieldDescriptions) == 0 {
			continue
		}
		if res != nil {
			return nil, errors.New("pizzasql: multiple result sets are unsupported")
		}
		res = r
	}
	if res == nil {
		return &rows{}, nil
	}
	return &rows{fields: res.FieldDescriptions, values: res.Rows}, nil
}

func (r *rows) Columns() []string {
	names := make([]string, len(r.fields))
	for i, f := range r.fields {
		names[i] = f.Name
	}
	return names
}

func (r *rows) Close() error {
	r.values = nil
	return nil
}

func (r *rows) Next(dest []driver.Value) error {
	if r.pos >= len(r.values) {
		return io.EOF
	}

	row := r.values[r.pos]
	for i, v := range row {
		if v == nil {
			dest[i] = nil
			continue
		}

		switch r.fields[i].DataTypeOID {
		case oidBytea:
			dest[i] = decodeBytea(v)
		case oidDate, oidTimestamp, oidTimestamptz, oidTime:
			t, err := parseTime(string(v))
			if err != nil {
				return err
			}
			dest[i] = t
		default:
			// Only decode time from a reliable type OID. A TEXT column whose
			// value merely looks like a date is left as a string so
			// database/sql can scan it without corrupting the value.
			dest[i] = string(v)
		}
	}
	r.pos++
	return nil
}

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"15:04:05.999999999",
	"15:04:05",
}

func parseTime(s string) (time.Time, error) {
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("pizzasql: invalid timestamp value " + strconv.Quote(s))
}

// decodeBytea converts a bytea wire value into bytes. The engine sends raw
// bytes, while a stricter PostgreSQL server sends the \x hex form; both are
// handled, and Go's fmt "%v" decimal list is recognized as a last resort.
func decodeBytea(v []byte) []byte {
	if len(v) >= 2 && v[0] == '\\' && v[1] == 'x' {
		if b, err := hex.DecodeString(string(v[2:])); err == nil {
			return b
		}
	}
	if len(v) >= 2 && v[0] == '[' && v[len(v)-1] == ']' {
		if b, ok := decodeDecimalByteList(string(v[1 : len(v)-1])); ok {
			return b
		}
	}
	return v
}

func decodeDecimalByteList(s string) ([]byte, bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil, false
	}
	out := make([]byte, len(fields))
	for i, f := range fields {
		n, err := strconv.ParseUint(f, 10, 8)
		if err != nil {
			return nil, false
		}
		out[i] = byte(n)
	}
	return out, true
}
