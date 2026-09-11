package pizzasql

import (
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// sqliteTimeFormat matches the single timestamp format GoatCounter configures
// for SQLite (see goatcounter's helper_cgo.go). SQLite check constraints in the
// GoatCounter schema compare a column to strftime('%Y-%m-%d %H:%M:%S', column),
// so a timestamp with a timezone or fractional seconds would be rejected.
const sqliteTimeFormat = "2006-01-02 15:04:05"

// bind inlines the given parameters in place of ? placeholders using SQLite
// literal syntax, so the statement is safe to send over the simple-query
// protocol, which does not accept out-of-band parameters.
//
// A statement with no parameters is passed through unchanged. That is how zdb
// runs the multi-statement schema batch through Exec: those statements carry no
// placeholders and must reach the engine as written.
//
// Placeholders inside string literals, quoted identifiers, and comments are
// never substituted, and the substitution is byte-for-byte so the statement
// keeps its original layout.
func bind(query string, args []driver.NamedValue) (string, error) {
	if len(args) == 0 {
		return query, nil
	}
	for _, arg := range args {
		if arg.Name != "" {
			return "", errors.New("pizzasql: named parameters are unsupported")
		}
	}

	var out strings.Builder
	out.Grow(len(query) + len(args)*8)

	used := make([]bool, len(args))
	next := 0
	ended := false
	for i := 0; i < len(query); {
		ch := query[i]

		if ch == 0 {
			return "", errors.New("pizzasql: NUL in SQL")
		}

		if ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n' {
			out.WriteByte(ch)
			i++
			continue
		}

		if i+1 < len(query) && query[i:i+2] == "--" {
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				end = len(query) - i
			}
			out.WriteString(query[i : i+end])
			i += end
			continue
		}

		if i+1 < len(query) && query[i:i+2] == "/*" {
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				return "", errors.New("pizzasql: unterminated comment")
			}
			end += i + 4
			out.WriteString(query[i:end])
			i = end
			continue
		}

		// With parameters present only a single statement is allowed: the
		// positional ? placeholders of a second statement cannot be attributed
		// safely. Trailing whitespace and comments after the terminator remain
		// valid.
		if ended {
			return "", errors.New("pizzasql: multiple statements with parameters are unsupported")
		}

		if ch == ';' {
			ended = true
			out.WriteByte(ch)
			i++
			continue
		}

		if ch == '\'' || ch == '"' || ch == '`' || ch == '[' {
			end, err := scanQuoted(query, i)
			if err != nil {
				return "", err
			}
			out.WriteString(query[i:end])
			i = end
			continue
		}

		if ch == '?' {
			for next < len(args) && used[next] {
				next++
			}
			if next >= len(args) {
				return "", errors.New("pizzasql: not enough parameters")
			}
			literal, err := sqlLiteral(args[next].Value)
			if err != nil {
				return "", err
			}
			out.WriteString(literal)
			used[next] = true
			next++
			i++
			continue
		}

		if ch == '$' && i+1 < len(query) && query[i+1] >= '0' && query[i+1] <= '9' {
			end := i + 2
			for end < len(query) && query[end] >= '0' && query[end] <= '9' {
				end++
			}
			ordinal, err := strconv.Atoi(query[i+1 : end])
			if err != nil || ordinal < 1 || ordinal > len(args) {
				return "", errors.New("pizzasql: numbered parameter out of range")
			}
			literal, err := sqlLiteral(args[ordinal-1].Value)
			if err != nil {
				return "", err
			}
			out.WriteString(literal)
			used[ordinal-1] = true
			i = end
			continue
		}

		out.WriteByte(ch)
		i++
	}

	for _, ok := range used {
		if !ok {
			return "", errors.New("pizzasql: too many parameters")
		}
	}
	return out.String(), nil
}

// scanQuoted returns the index just past the quoted token that begins at start,
// which must point at ', ", `, or [. It verifies the token is terminated.
//
// Only string literals treat a doubled delimiter as an escape, matching the
// PizzaSQL lexer. Quoted, backtick, and bracket identifiers that contain their
// own delimiter are not escaped by the lexer, so they are not treated as
// escaped here either.
func scanQuoted(query string, start int) (int, error) {
	delim := query[start]
	end := delim
	escaped := delim == '\''
	if end == '[' {
		end = ']'
	}

	i := start + 1
	for i < len(query) {
		if query[i] != end {
			i++
			continue
		}
		i++
		if escaped && i < len(query) && query[i] == end {
			i++
			continue
		}
		return i, nil
	}
	return 0, errors.New("pizzasql: unterminated quoted token")
}

func sqlLiteral(value any) (string, error) {
	switch v := value.(type) {
	case nil:
		return "NULL", nil
	case bool:
		if v {
			return "1", nil
		}
		return "0", nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return "", errors.New("pizzasql: non-finite number")
		}
		return strconv.FormatFloat(v, 'g', -1, 64), nil
	case time.Time:
		return "'" + v.UTC().Format(sqliteTimeFormat) + "'", nil
	case []byte:
		return "X'" + hex.EncodeToString(v) + "'", nil
	case string:
		if strings.ContainsRune(v, 0) {
			return "", errors.New("pizzasql: NUL in text parameter")
		}
		return "'" + strings.ReplaceAll(v, "'", "''") + "'", nil
	default:
		return "", fmt.Errorf("pizzasql: unsupported parameter type %T", value)
	}
}
