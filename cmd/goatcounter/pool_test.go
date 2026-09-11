package main

import (
	"errors"
	"strings"
	"testing"

	"zgo.at/goatcounter/v2/pkg/pizzasql"
	"zgo.at/zdb/drivers"
)

func TestEffectivePool(t *testing.T) {
	const pz = "sqlite/pizzasql+host=127.0.0.1 password=secret dbname=org/db"
	const sqlite = "sqlite+./db.sqlite3"

	tests := []struct {
		name    string
		connect string
		dbConn  string
		set     bool
		open    int
		idle    int
		wantErr bool
	}{
		{"pizzasql ignores the generic default", pz, "16,4", false, 2, 2, false},
		{"explicit overrides pizzasql default", pz, "8,3", true, 8, 3, false},
		{"non-pizzasql falls back to zdb default", sqlite, "16,4", false, 0, 0, false},
		{"non-pizzasql explicit", sqlite, "9,5", true, 9, 5, false},
		{"malformed explicit", pz, "9", true, 0, 0, true},
		{"empty explicit", pz, "", true, 0, 0, true},
		{"non-numeric explicit", pz, "x,y", true, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			open, idle, err := effectivePool(tt.connect, tt.dbConn, tt.set)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if open != tt.open || idle != tt.idle {
				t.Errorf("effectivePool() = %d,%d; want %d,%d", open, idle, tt.open, tt.idle)
			}
		})
	}
}

// TestConnectDBAppliesPool proves the resolved sizes reach the underlying
// database/sql pool, not just the option struct.
func TestConnectDBAppliesPool(t *testing.T) {
	db, _, err := connectDB("sqlite3+:memory:?cache=shared", "3,2", true, []string{"pending"}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	sqlDB, _ := db.DBSQL()
	if got := sqlDB.Stats().MaxOpenConnections; got != 3 {
		t.Errorf("MaxOpenConnections = %d; want 3", got)
	}
}

func TestRedactConnectHidesKey(t *testing.T) {
	connect := "sqlite/pizzasql+host=127.0.0.1 password=super-secret dbname=org/db"
	got := redactConnect(connect)
	if got != "sqlite/pizzasql+[redacted]" {
		t.Errorf("redactConnect() = %q", got)
	}
	if strings.Contains(got, "super-secret") {
		t.Fatal("redacted connect string leaked the API key")
	}
}

func TestRedactDBErrorKeepsTypeAndHidesKey(t *testing.T) {
	connect := "sqlite/pizzasql+host=127.0.0.1 password=super-secret dbname=org/db"
	orig := &drivers.NotExistError{Driver: "SQLite", Connect: connect}

	got := redactDBError(connect, orig)
	var nErr *drivers.NotExistError
	if !errors.As(got, &nErr) {
		t.Fatalf("redacted error lost its type: %T", got)
	}
	if strings.Contains(got.Error(), "super-secret") {
		t.Fatalf("redacted error leaked the API key: %v", got)
	}

	// A non-PizzaSQL error is returned unchanged.
	plain := errors.New("boom")
	if redactDBError("sqlite+./db.sqlite3", plain) != plain {
		t.Error("non-PizzaSQL error should pass through unchanged")
	}
}

func TestMaxOpenIdleDefaultsAreBounded(t *testing.T) {
	if pizzasql.MaxOpenConns != 2 || pizzasql.MaxIdleConns != 2 {
		t.Fatalf("PizzaSQL pool default = %d,%d; want 2,2",
			pizzasql.MaxOpenConns, pizzasql.MaxIdleConns)
	}
}
