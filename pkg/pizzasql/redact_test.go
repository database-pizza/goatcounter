package pizzasql

import (
	"errors"
	"strings"
	"testing"

	"zgo.at/zdb/drivers"
)

func TestRedactConnect(t *testing.T) {
	const key = "s3cret-api-key"

	tests := []struct {
		name    string
		connect string
		want    string
	}{
		{
			"keyword dsn",
			"sqlite/pizzasql+host=127.0.0.1 port=5432 user=pizza password=" + key + " dbname=org/db sslmode=disable",
			"sqlite/pizzasql+[redacted]",
		},
		{
			"url dsn",
			"sqlite/pizzasql+postgres://pizza:" + key + "@127.0.0.1:5432/org/db?sslmode=disable",
			"sqlite/pizzasql+[redacted]",
		},
		{
			"unknown dialect form",
			"pizzasql+host=127.0.0.1 password=" + key,
			"pizzasql+[redacted]",
		},
		{
			"deprecated URL separator",
			"sqlite/pizzasql://host=127.0.0.1 password=" + key,
			"sqlite/pizzasql+[redacted]",
		},
		{
			"plain sqlite is unchanged",
			"sqlite+./db.sqlite3",
			"sqlite+./db.sqlite3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactConnect(tt.connect)
			if got != tt.want {
				t.Errorf("RedactConnect() = %q; want %q", got, tt.want)
			}
			if strings.Contains(got, key) {
				t.Fatalf("redacted connect string leaked the key: %q", got)
			}
		})
	}
}

func TestRedactError(t *testing.T) {
	const key = "s3cret-api-key"
	connect := "sqlite/pizzasql+host=127.0.0.1 password=" + key + " dbname=org/db"

	orig := &drivers.NotExistError{Driver: "SQLite", Connect: connect}
	got := RedactError(connect, orig)
	var nErr *drivers.NotExistError
	if !errors.As(got, &nErr) {
		t.Fatalf("redacted error lost its type: %T", got)
	}
	if strings.Contains(got.Error(), key) {
		t.Fatalf("redacted error leaked the key: %v", got)
	}
	generic := errors.New("connect " + connect + ": refused")
	if redacted := RedactError(connect, generic); strings.Contains(redacted.Error(), key) {
		t.Fatalf("generic error leaked the key: %v", redacted)
	}

	// Non-PizzaSQL errors and errors that are not NotExistError pass through.
	plain := errors.New("boom")
	if RedactError(connect, plain) != plain {
		t.Error("non-NotExistError should pass through unchanged")
	}
	if RedactError("sqlite+./db.sqlite3", orig) != error(orig) {
		t.Error("non-PizzaSQL connect string should pass the error through")
	}
}
