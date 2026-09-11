#!/bin/sh
# Run GoatCounter's PizzaSQL integration tests against an already-running local
# app.database.pizza managed pgproxy.
#
# Credentials are read from the environment only and are never printed. This
# script creates no resources and deploys nothing: it just runs the env-gated
# tests. Point it at a disposable local database, because the tests create the
# GoatCounter schema and run every migration.
#
# Either set a complete PIZZASQL_TEST_DSN yourself, or provide the managed
# coordinates and let this script assemble the keyword/value DSN:
#
#   PIZZASQL_LOCAL_ORG       organization slug (first half of the org/db name)
#   PIZZASQL_LOCAL_DB        database slug (second half)
#   PIZZASQL_LOCAL_API_KEY   managed API key (used as the password)
#   PIZZASQL_LOCAL_HOST      proxy host, default 127.0.0.1
#   PIZZASQL_LOCAL_PORT      proxy port, default 5432
#
# PizzaSQL has no TLS on the local proxy hop, so only loopback targets are
# accepted. The pgproxy expects the API key as the password and ignores the
# user field.
set -eu

host=${PIZZASQL_LOCAL_HOST:-127.0.0.1}
port=${PIZZASQL_LOCAL_PORT:-5432}
org=${PIZZASQL_LOCAL_ORG:-}
db=${PIZZASQL_LOCAL_DB:-}
key=${PIZZASQL_LOCAL_API_KEY:-}

if [ -n "${PIZZASQL_TEST_DSN:-}" ]; then
	dsn=$PIZZASQL_TEST_DSN
else
	if [ -z "$org" ] || [ -z "$db" ] || [ -z "$key" ]; then
		echo "set PIZZASQL_TEST_DSN, or PIZZASQL_LOCAL_ORG, PIZZASQL_LOCAL_DB, and PIZZASQL_LOCAL_API_KEY" >&2
		exit 2
	fi

	case $host in
	127.0.0.1 | localhost | ::1) ;;
	*)
		echo "refusing non-loopback host '$host': PizzaSQL TLS is unavailable on the local proxy hop" >&2
		exit 2
		;;
	esac

	# Escape for pgconn's keyword/value parser: a backslash and a single quote
	# are the only metacharacters inside a single-quoted value.
	esc() {
		printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e "s/'/\\\\'/g"
	}

	dsn="host='$(esc "$host")' port='$(esc "$port")' user='pizza' password='$(esc "$key")' dbname='$(esc "$org")/$(esc "$db")' sslmode='disable'"
fi

echo "running PizzaSQL integration tests" >&2
PIZZASQL_TEST_DSN=$dsn exec go test -count=1 -v \
	-run 'TestIntegrationDriverWorkflow|TestPizzaSQLIntegration' \
	./pkg/pizzasql/ ./gctest/
