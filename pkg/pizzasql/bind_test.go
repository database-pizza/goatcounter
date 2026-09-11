package pizzasql

import (
	"database/sql/driver"
	"math"
	"testing"
	"time"
)

func nv(values ...any) []driver.NamedValue {
	n := make([]driver.NamedValue, len(values))
	for i, v := range values {
		n[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return n
}

func TestBind(t *testing.T) {
	ts := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)

	tests := []struct {
		name  string
		query string
		args  []driver.NamedValue
		want  string
	}{
		{"no parameters passes through", "select 1; select 2", nil, "select 1; select 2"},
		{"no parameters untouched", "select '?'", nil, "select '?'"},
		{"string", "select ?", nv("a"), "select 'a'"},
		{"string escape", "select ?", nv("a'b"), "select 'a''b'"},
		{"nil", "select ?", nv(nil), "select NULL"},
		{"bool true", "select ?", nv(true), "select 1"},
		{"bool false", "select ?", nv(false), "select 0"},
		{"int", "select ?", nv(int64(-42)), "select -42"},
		{"float", "select ?", nv(3.5), "select 3.5"},
		{"time", "select ?", nv(ts), "select '2024-05-06 07:08:09'"},
		{"bytes", "select ?", nv([]byte{0x41, 0x00, 0xff}), "select X'4100ff'"},
		{"multiple", "select ?, ?", nv("a", int64(1)), "select 'a', 1"},
		{"numbered", "select $1, $2", nv("a", int64(1)), "select 'a', 1"},
		{"numbered repeated", "select $2, $1, $2", nv("a", int64(1)), "select 1, 'a', 1"},
		{"mixed", "select $2, ?", nv("a", int64(1)), "select 1, 'a'"},

		{
			"placeholder in string is ignored",
			"select '?', ?",
			nv("x"),
			"select '?', 'x'",
		},
		{
			"numbered placeholder in string is ignored",
			"select '$1', $1",
			nv("x"),
			"select '$1', 'x'",
		},
		{
			"placeholder in identifier is ignored",
			`select "?", ?`,
			nv("x"),
			`select "?", 'x'`,
		},
		{
			"placeholder in backtick identifier is ignored",
			"select `?`, ?",
			nv("x"),
			"select `?`, 'x'",
		},
		{
			"placeholder in bracket identifier is ignored",
			"select [?], ?",
			nv("x"),
			"select [?], 'x'",
		},
		{
			"placeholder in line comment is ignored",
			"select 1 -- ?\n, ?",
			nv("x"),
			"select 1 -- ?\n, 'x'",
		},
		{
			"placeholder in block comment is ignored",
			"select /* ? */ ?",
			nv("x"),
			"select /* ? */ 'x'",
		},
		{
			"doubled quote in literal",
			"select 'it''s ?', ?",
			nv("x"),
			"select 'it''s ?', 'x'",
		},
		{
			"trailing semicolon with comment",
			"select ?; -- done",
			nv("x"),
			"select 'x'; -- done",
		},
		{
			"semicolon in string not a terminator",
			"select ';', ?",
			nv("x"),
			"select ';', 'x'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := bind(tt.query, tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("\ngot:  %q\nwant: %q", got, tt.want)
			}
		})
	}
}

func TestBindErrors(t *testing.T) {
	tests := []struct {
		name  string
		query string
		args  []driver.NamedValue
	}{
		{"too few parameters", "select ?, ?", nv("a")},
		{"too many parameters", "select ?", nv("a", "b")},
		{"numbered parameter zero", "select $0", nv("a")},
		{"numbered parameter out of range", "select $2", nv("a")},
		{"multiple statements with parameters", "select ?; select ?", nv("a", "b")},
		{"second statement after terminator", "select ?; select 2", nv("a")},
		{"nul in sql", "select \x00", nv("a")},
		{"nul in text parameter", "select ?", nv("a\x00b")},
		{"named parameter", "select ?", []driver.NamedValue{{Ordinal: 1, Name: "x", Value: "a"}}},
		{"unterminated string", "select '?", nv("a")},
		{"unterminated comment", "select /* ?", nv("a")},
		{"non-finite float", "select ?", nv(math.Inf(1))},
		{"unsupported type", "select ?", []driver.NamedValue{{Ordinal: 1, Value: struct{}{}}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := bind(tt.query, tt.args); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
