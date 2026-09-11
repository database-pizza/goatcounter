package pizzasql

import (
	"errors"
	"strings"

	"zgo.at/zdb/drivers"
)

// RedactConnect returns a safe representation of a zdb connect string for error
// messages and logs. A PizzaSQL connect string carries the managed API key as
// its password, so it is never echoed in full: the scheme is kept and the
// rest, which includes the DSN, is replaced. Non-PizzaSQL connect strings are
// returned unchanged.
func RedactConnect(connect string) string {
	if !IsConnectString(connect) {
		return connect
	}
	head, _, found := strings.Cut(connect, "+")
	if !found {
		head, _, _ = strings.Cut(connect, "://")
	}
	return head + "+[redacted]"
}

// RedactError removes a PizzaSQL connect string from an error before it can
// reach a user or a log. zdb's NotExistError embeds the raw connect string,
// which for PizzaSQL includes the managed API key. The error keeps its type so
// callers that branch on NotExistError still work.
func RedactError(connect string, err error) error {
	if err == nil || !IsConnectString(connect) {
		return err
	}
	var nErr *drivers.NotExistError
	if errors.As(err, &nErr) {
		return &drivers.NotExistError{Driver: DriverName, DB: nErr.DB, Connect: "[redacted]"}
	}
	message := err.Error()
	redacted := strings.ReplaceAll(message, connect, RedactConnect(connect))
	if _, dsn, ok := strings.Cut(connect, "+"); ok && dsn != "" {
		redacted = strings.ReplaceAll(redacted, dsn, "[redacted]")
	}
	if redacted != message {
		return errors.New(redacted)
	}
	return err
}
