package pizzasql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// fakeResponse is one statement result the fake server sends back.
type fakeResponse struct {
	fields []pgproto3.FieldDescription
	rows   [][][]byte
	tag    string
	err    string
}

func fieldText(name string) pgproto3.FieldDescription {
	return pgproto3.FieldDescription{Name: []byte(name), DataTypeOID: 25, DataTypeSize: -1}
}

func fieldOID(name string, oid uint32) pgproto3.FieldDescription {
	return pgproto3.FieldDescription{Name: []byte(name), DataTypeOID: oid, DataTypeSize: -1}
}

type fakeServer struct {
	t   *testing.T
	ln  net.Listener
	dsn string

	fn func(query string) []fakeResponse

	mu      sync.Mutex
	queries []string
}

func startFakeServer(t *testing.T, fn func(query string) []fakeResponse) *fakeServer {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	s := &fakeServer{
		t:  t,
		ln: ln,
		dsn: fmt.Sprintf("host=127.0.0.1 port=%d user=u dbname=d sslmode=disable",
			ln.Addr().(*net.TCPAddr).Port),
		fn: fn,
	}
	go s.serve()

	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeServer) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.queries...)
}

func (s *fakeServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeServer) handle(nc net.Conn) {
	defer nc.Close()

	backend := pgproto3.NewBackend(nc, nc)
	if _, err := backend.ReceiveStartupMessage(); err != nil {
		return
	}
	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "14.0"})
	backend.Send(&pgproto3.BackendKeyData{ProcessID: 1, SecretKey: []byte{0, 0, 0, 2}})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := backend.Flush(); err != nil {
		return
	}

	for {
		msg, err := backend.Receive()
		if err != nil {
			return
		}

		switch m := msg.(type) {
		case *pgproto3.Query:
			s.mu.Lock()
			s.queries = append(s.queries, m.String)
			s.mu.Unlock()

			responses := s.fn(m.String)
			for _, r := range responses {
				if r.err != "" {
					backend.Send(&pgproto3.ErrorResponse{
						Severity: "ERROR",
						Code:     "XX000",
						Message:  r.err,
					})
					continue
				}
				if len(r.fields) > 0 {
					backend.Send(&pgproto3.RowDescription{Fields: r.fields})
					for _, row := range r.rows {
						backend.Send(&pgproto3.DataRow{Values: row})
					}
				}
				tag := r.tag
				if tag == "" {
					tag = "SELECT 1"
				}
				backend.Send(&pgproto3.CommandComplete{CommandTag: []byte(tag)})
			}
			backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if err := backend.Flush(); err != nil {
				return
			}
		case *pgproto3.Terminate:
			return
		}
	}
}

// lastRowID returns a handler that answers the connect-time capability probe
// with the given values, in order, and everything else with the provided
// responses.
func lastRowID(values ...string) func(string) []fakeResponse {
	var i int
	return func(query string) []fakeResponse {
		if strings.EqualFold(strings.TrimSpace(query), "SELECT last_insert_rowid()") {
			v := values[len(values)-1]
			if i < len(values) {
				v = values[i]
			}
			i++
			return []fakeResponse{{
				fields: []pgproto3.FieldDescription{fieldOID("last_insert_rowid", 20)},
				rows:   [][][]byte{{[]byte(v)}},
			}}
		}
		return nil
	}
}

func TestConnectorCapabilityCheck(t *testing.T) {
	srv := startFakeServer(t, lastRowID("7"))

	db, err := sql.Open(DriverName, srv.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}

	found := false
	for _, q := range srv.recorded() {
		if strings.EqualFold(strings.TrimSpace(q), "SELECT last_insert_rowid()") {
			found = true
		}
	}
	if !found {
		t.Fatal("connect must probe last_insert_rowid")
	}
}

func TestQueryDecodesValues(t *testing.T) {
	handler := lastRowID("0")
	base := handler
	srv := startFakeServer(t, func(query string) []fakeResponse {
		if strings.EqualFold(strings.TrimSpace(query), "SELECT last_insert_rowid()") {
			return base(query)
		}
		return []fakeResponse{{
			fields: []pgproto3.FieldDescription{
				fieldText("s"),
				fieldOID("ts", oidTimestamptz),
				fieldOID("d", oidDate),
				fieldText("text_date"),
				fieldOID("b", oidBytea),
				fieldText("n"),
			},
			rows: [][][]byte{{
				[]byte("hello"),
				[]byte("2024-05-06 07:08:09"),
				[]byte("2024-05-06"),
				[]byte("2024-05-06"),
				[]byte{0x00, 0xff, 0x10},
				nil,
			}},
		}}
	})

	db, err := sql.Open(DriverName, srv.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var (
		s        string
		ts       time.Time
		d        time.Time
		textDate string
		b        []byte
		n        sql.NullString
	)
	err = db.QueryRow("select s, ts, d, text_date, b, n from t").Scan(&s, &ts, &d, &textDate, &b, &n)
	if err != nil {
		t.Fatal(err)
	}
	if s != "hello" {
		t.Errorf("s = %q", s)
	}
	if want := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC); !ts.Equal(want) {
		t.Errorf("ts = %v; want %v", ts, want)
	}
	// A real DATE OID decodes to time.Time.
	if want := time.Date(2024, 5, 6, 0, 0, 0, 0, time.UTC); !d.Equal(want) {
		t.Errorf("d = %v; want %v", d, want)
	}
	// A TEXT OID that merely looks like a date must stay an exact string.
	if textDate != "2024-05-06" {
		t.Errorf("text_date = %q; want %q", textDate, "2024-05-06")
	}
	if want := []byte{0x00, 0xff, 0x10}; string(b) != string(want) {
		t.Errorf("b = %x; want %x", b, want)
	}
	if n.Valid {
		t.Errorf("n = %v; want NULL", n)
	}
}

// TestQueryTextDateFidelity checks that a date-shaped value in a TEXT column is
// never reinterpreted as a time.Time, which would change what a string or
// []byte destination receives.
func TestQueryTextDateFidelity(t *testing.T) {
	base := lastRowID("0")
	srv := startFakeServer(t, func(query string) []fakeResponse {
		if strings.EqualFold(strings.TrimSpace(query), "SELECT last_insert_rowid()") {
			return base(query)
		}
		return []fakeResponse{{
			fields: []pgproto3.FieldDescription{fieldText("v")},
			rows:   [][][]byte{{[]byte("2024-05-06")}},
		}}
	})

	db, err := sql.Open(DriverName, srv.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	var s string
	if err := db.QueryRow("select v from t").Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s != "2024-05-06" {
		t.Fatalf("TEXT date value was rewritten: %q", s)
	}

	var b []byte
	if err := db.QueryRow("select v from t").Scan(&b); err != nil {
		t.Fatal(err)
	}
	if string(b) != "2024-05-06" {
		t.Fatalf("TEXT date bytes were rewritten: %q", b)
	}
}

func TestExecInsertReadsLastInsertID(t *testing.T) {
	var mu sync.Mutex
	rowidCalls := 0

	srv := startFakeServer(t, func(query string) []fakeResponse {
		q := strings.TrimSpace(query)
		if strings.EqualFold(q, "SELECT last_insert_rowid()") {
			mu.Lock()
			defer mu.Unlock()
			rowidCalls++
			// The first call is the connect-time probe; the second is the
			// post-insert follow-up on the same session.
			if rowidCalls == 1 {
				return []fakeResponse{{fields: []pgproto3.FieldDescription{fieldOID("last_insert_rowid", 20)}, rows: [][][]byte{{[]byte("0")}}}}
			}
			return []fakeResponse{{fields: []pgproto3.FieldDescription{fieldOID("last_insert_rowid", 20)}, rows: [][][]byte{{[]byte("42")}}}}
		}
		return []fakeResponse{{tag: "INSERT 0 1"}}
	})

	db, err := sql.Open(DriverName, srv.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	res, err := db.Exec("insert into t (v) values (?)", "x")
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if id != 42 {
		t.Errorf("LastInsertId = %d; want 42", id)
	}
}

func TestMultiStatementExec(t *testing.T) {
	handler := lastRowID("0")
	base := handler

	srv := startFakeServer(t, func(query string) []fakeResponse {
		if strings.EqualFold(strings.TrimSpace(query), "SELECT last_insert_rowid()") {
			return base(query)
		}
		// One CommandComplete per statement in the batch.
		return []fakeResponse{
			{tag: "CREATE TABLE"},
			{tag: "CREATE INDEX"},
		}
	})

	db, err := sql.Open(DriverName, srv.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	schema := "create table t (id integer primary key);\ncreate index i on t (id);\n"
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}

	var found string
	for _, q := range srv.recorded() {
		if q == schema {
			found = q
		}
	}
	if found != schema {
		t.Fatalf("multi-statement batch was not passed through unchanged:\n%q", found)
	}
}

func TestExecDoesNotReplayInsertWhenFollowupFails(t *testing.T) {
	var mu sync.Mutex
	rowidCalls := 0

	srv := startFakeServer(t, func(query string) []fakeResponse {
		q := strings.TrimSpace(query)
		if strings.EqualFold(q, "SELECT last_insert_rowid()") {
			mu.Lock()
			defer mu.Unlock()
			rowidCalls++
			if rowidCalls == 1 {
				return []fakeResponse{{fields: []pgproto3.FieldDescription{fieldOID("last_insert_rowid", 20)}, rows: [][][]byte{{[]byte("0")}}}}
			}
			return []fakeResponse{{err: "admin shutdown"}}
		}
		return []fakeResponse{{tag: "INSERT 0 1"}}
	})

	db, err := sql.Open(DriverName, srv.dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	_, err = db.Exec("insert into t (v) values (?)", "x")
	if err == nil {
		t.Fatal("expected the follow-up failure to surface")
	}
	if errors.Is(err, driver.ErrBadConn) {
		t.Fatal("follow-up failure must not surface driver.ErrBadConn")
	}
	if !strings.Contains(err.Error(), "uncertain") {
		t.Errorf("error should explain the uncertain state: %v", err)
	}
}

func TestSanitizeConnectErrorDropsDSN(t *testing.T) {
	// A password containing an escaped quote used to leak through pgconn's
	// ParseConfigError; OpenConnector must not surface it.
	_, err := (&pizzaDriver{}).OpenConnector("postgres://user:secret@host:notaport/db")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatalf("connection error leaked the DSN: %v", err)
	}
}

func TestParseTimeLayouts(t *testing.T) {
	tests := map[string]time.Time{
		"2024-05-06 07:08:09":           time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC),
		"2024-05-06T07:08:09Z":          time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC),
		"2024-05-06 07:08:09.123456789": time.Date(2024, 5, 6, 7, 8, 9, 123456789, time.UTC),
		"2024-05-06":                    time.Date(2024, 5, 6, 0, 0, 0, 0, time.UTC),
	}
	for in, want := range tests {
		got, err := parseTime(in)
		if err != nil {
			t.Errorf("parseTime(%q): %s", in, err)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("parseTime(%q) = %v; want %v", in, got, want)
		}
	}
	if _, err := parseTime("not a time"); err == nil {
		t.Error("expected an error for an invalid timestamp")
	}
}

func TestDecodeBytea(t *testing.T) {
	if got := decodeBytea([]byte(`\x00ff`)); string(got) != string([]byte{0x00, 0xff}) {
		t.Errorf("hex decode = %x", got)
	}
	if got := decodeBytea([]byte{1, 2, 3}); string(got) != string([]byte{1, 2, 3}) {
		t.Errorf("raw decode = %x", got)
	}
	if got := decodeBytea([]byte("[104 105]")); string(got) != "hi" {
		t.Errorf("decimal decode = %q", got)
	}
}

func TestContextCancellation(t *testing.T) {
	srv := startFakeServer(t, lastRowID("0"))
	db, err := sql.Open(DriverName, srv.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// database/sql may return context.Canceled before reaching the driver; both
	// outcomes are acceptable here. The point is that it does not hang.
	_ = db.QueryRowContext(ctx, "select 1").Scan(new(int))
}
