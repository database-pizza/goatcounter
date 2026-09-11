package pizzasql

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

func TestVersionAtLeast(t *testing.T) {
	tests := map[string]bool{
		"3.45.1":    true,
		"3.35.0":    true,
		"3.35":      true,
		"3.34.9":    false,
		"2.9":       false,
		"4.0":       true,
		"build-abc": false,
		"":          false,
	}
	for in, want := range tests {
		if got := versionAtLeast(in, 3, 35); got != want {
			t.Errorf("versionAtLeast(%q) = %v; want %v", in, got, want)
		}
	}
}

func TestCapabilitiesMissing(t *testing.T) {
	var zero Capabilities
	missing := zero.Missing()
	for _, want := range []string{"3.35", "last_insert_rowid", "JSON", "percent_diff"} {
		found := false
		for _, m := range missing {
			if strings.Contains(m, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("missing list %v does not mention %q", missing, want)
		}
	}
	// Behavioural features are not part of the scalar connect-time check.
	for _, m := range missing {
		if strings.Contains(m, "RETURNING") || strings.Contains(m, "generated") {
			t.Errorf("scalar capability list must not include %q", m)
		}
	}

	full := Capabilities{
		SQLiteVersion:     "3.45.1",
		VersionAtLeast335: true,
		LastInsertRowID:   true,
		JSON:              true,
		PercentDiff:       true,
	}
	if !full.OK() {
		t.Errorf("expected full capabilities to be OK: %v", full.Missing())
	}
}

func capabilityHandler(jsonFn, percentDiff bool) func(string) []fakeResponse {
	return func(query string) []fakeResponse {
		q := strings.ToLower(strings.TrimSpace(query))
		switch {
		default:
			return nil
		case strings.HasPrefix(q, "select sqlite_version"):
			return []fakeResponse{{fields: []pgproto3.FieldDescription{fieldText("v")}, rows: [][][]byte{{[]byte("3.45.1")}}}}
		case q == "select last_insert_rowid()":
			return []fakeResponse{{fields: []pgproto3.FieldDescription{fieldOID("l", 20)}, rows: [][][]byte{{[]byte("0")}}}}
		case strings.HasPrefix(q, "select json("):
			if !jsonFn {
				return []fakeResponse{{err: "unknown function: JSON"}}
			}
			return []fakeResponse{{
				fields: []pgproto3.FieldDescription{fieldText("j"), fieldText("e")},
				rows:   [][][]byte{{[]byte("[1,2]"), []byte("1")}},
			}}
		case strings.HasPrefix(q, "select percent_diff("):
			if !percentDiff {
				return []fakeResponse{{err: "unknown function: PERCENT_DIFF"}}
			}
			return []fakeResponse{{fields: []pgproto3.FieldDescription{fieldOID("d", 701)}, rows: [][][]byte{{[]byte("100")}}}}
		}
	}
}

func TestCheckCapabilitiesAllPresent(t *testing.T) {
	srv := startFakeServer(t, capabilityHandler(true, true))
	db, err := sql.Open(DriverName, srv.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	caps, err := CheckCapabilities(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !caps.OK() {
		t.Fatalf("expected OK: %v", caps.Missing())
	}
	if !caps.JSON || !caps.PercentDiff || !caps.LastInsertRowID {
		t.Errorf("unexpected capabilities: %+v", caps)
	}
}

func TestCheckCapabilitiesMissing(t *testing.T) {
	srv := startFakeServer(t, capabilityHandler(false, false))
	db, err := sql.Open(DriverName, srv.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	caps, err := CheckCapabilities(context.Background(), db)
	if err == nil {
		t.Fatal("expected an UnsupportedError")
	}
	var unsup *UnsupportedError
	if !errors.As(err, &unsup) {
		t.Fatalf("expected *UnsupportedError; got %T: %v", err, err)
	}
	if caps.JSON || caps.PercentDiff {
		t.Errorf("unexpected capabilities: %+v", caps)
	}
	for _, want := range []string{"JSON", "percent_diff"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestCheckCapabilitiesNeverMutates proves the connect-time probe only issues
// scalar reads: the fake server records every query and none of them is DDL or
// DML.
func TestCheckCapabilitiesNeverMutates(t *testing.T) {
	srv := startFakeServer(t, capabilityHandler(true, true))
	db, err := sql.Open(DriverName, srv.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := CheckCapabilities(context.Background(), db); err != nil {
		t.Fatal(err)
	}

	for _, q := range srv.recorded() {
		lower := strings.ToLower(q)
		for _, bad := range []string{"create ", "insert ", "update ", "delete ", "drop ", "alter "} {
			if strings.HasPrefix(strings.TrimSpace(lower), bad) {
				t.Errorf("connect-time capability probe issued a mutating statement: %q", q)
			}
		}
	}
}

func behaviorHandler(returning, generated bool) func(string) []fakeResponse {
	return func(query string) []fakeResponse {
		q := strings.ToLower(strings.TrimSpace(query))
		switch {
		default:
			return nil
		case q == "select last_insert_rowid()":
			return []fakeResponse{{fields: []pgproto3.FieldDescription{fieldOID("l", 20)}, rows: [][][]byte{{[]byte("0")}}}}
		case strings.HasPrefix(q, "create table"):
			return []fakeResponse{{tag: "CREATE TABLE"}}
		case strings.HasPrefix(q, "insert into") && strings.Contains(q, "returning id"):
			if !returning {
				return []fakeResponse{{err: "syntax error near returning"}}
			}
			return []fakeResponse{{fields: []pgproto3.FieldDescription{fieldOID("id", 20)}, rows: [][][]byte{{[]byte("1")}}}}
		case strings.HasPrefix(q, "insert into"):
			if !generated {
				return []fakeResponse{{err: "expected )"}}
			}
			return []fakeResponse{{tag: "INSERT 0 1"}}
		case strings.HasPrefix(q, "select b from"):
			return []fakeResponse{{fields: []pgproto3.FieldDescription{fieldOID("b", 20)}, rows: [][][]byte{{[]byte("2")}}}}
		case strings.HasPrefix(q, "drop table"):
			return []fakeResponse{{tag: "DROP TABLE"}}
		}
	}
}

func TestVerifyBehavior(t *testing.T) {
	srv := startFakeServer(t, behaviorHandler(true, true))
	db, err := sql.Open(DriverName, srv.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	behavior, err := VerifyBehavior(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !behavior.OK() {
		t.Errorf("expected OK behaviour: %v", behavior.Missing())
	}
	if !behavior.Returning || !behavior.GeneratedColumns {
		t.Errorf("unexpected behaviour: %+v", behavior)
	}
}

func TestVerifyBehaviorMissing(t *testing.T) {
	srv := startFakeServer(t, behaviorHandler(false, false))
	db, err := sql.Open(DriverName, srv.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	behavior, err := VerifyBehavior(context.Background(), db)
	if err == nil {
		t.Fatal("expected an UnsupportedError")
	}
	var unsup *UnsupportedError
	if !errors.As(err, &unsup) {
		t.Fatalf("expected *UnsupportedError; got %T: %v", err, err)
	}
	if behavior.OK() {
		t.Errorf("expected behaviour to be missing: %+v", behavior)
	}
	for _, want := range []string{"RETURNING", "generated"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
