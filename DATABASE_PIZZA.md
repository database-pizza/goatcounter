# Running GoatCounter on PizzaSQL

This document describes the database.pizza fork additions that let GoatCounter
run on a managed [PizzaSQL](../pizzasql) tenant through the
[app.database.pizza](../app.database.pizza) PostgreSQL proxy (`pgproxy`)
instead of PostgreSQL or a local SQLite file.

## Architecture

```text
GoatCounter (zdb, SQLite dialect)
  -> pkg/pizzasql          database/sql driver over pgx pgconn simple-query
  -> pgproxy               API key, scopes, per-organization quota
  -> managed PizzaSQL      SQLite dialect executor
  -> PizzaKV               durable storage
```

The transport is the PostgreSQL wire protocol, but the SQL that travels over it
is SQLite dialect. `pkg/pizzasql` therefore reports the `sqlite` dialect to
zdb, which selects GoatCounter's SQLite schema, migrations, and query files. The
driver never opens a local SQLite file.

`pkg/pizzasql` contains two layers:

- A `database/sql` driver registered as `pizzasql` (`sql.Register`). It uses
  `github.com/jackc/pgx/v5/pgconn` with the simple-query protocol and preserves
  result metadata for empty queries.
- A small zdb driver adapter (`pizzasql.Register`) that names the driver
  `pizzasql`, reports the `sqlite` dialect, enforces the capability checks, and
  bounds the pool.

It is registered on demand rather than from `init()`: adding a second driver
for the `sqlite` dialect would make zdb's driver selection ambiguous for
ordinary `sqlite+file` connections. GoatCounter calls `pizzasql.Register()`
from `connectDB` and `cmdDBTest`, and the test harness calls it only when
`PIZZASQL_TEST_DSN` is set.

## Configuration

A PizzaSQL connection uses the zdb form:

```text
sqlite/pizzasql+<dsn>
```

and the `pizzasql` database/sql driver expects the same `<dsn>` (a PostgreSQL
keyword/value string or URL) directly.

Managed tenants are scoped as `org/db`; the API key is the password and the
user field is ignored by the proxy. TLS is not available on the local proxy hop,
so use `sslmode=disable` and only connect over loopback or another trusted hop:

```sh
export BROWSER=...   # optional, see the flags below
export GOATCOUNTER_DB="sqlite/pizzasql+host=127.0.0.1 port=5432 user=pizza password=$PIZZASQL_API_KEY dbname=myorg/mydb sslmode=disable"
goatcounter serve -automigrate
```

`-dbconn` controls the pool as `max_open,max_idle`. PizzaSQL connections default
to a bounded `2,2` because the proxy enforces a per-organization connection
quota; pass `-dbconn` explicitly to override it.

A local, un-proxied PizzaSQL engine accepts any user and has no key:

```text
sqlite/pizzasql+host=127.0.0.1 port=15434 user=u dbname=mydb sslmode=disable
```

## Driver behaviour

- Parameter binding inlines `?` and numbered `$1` placeholders as SQLite literals. The scanner is
  quote-, bracket-, backtick-, and comment-aware, so `?` inside a literal,
  identifier, or comment is never substituted.
- A statement with no parameters is passed through unchanged, which is how
  zdb's multi-statement schema batch (`db/schema.gotxt`) reaches the engine
  through `Exec`. With parameters present only a single statement is allowed,
  because positional placeholders cannot be attributed across statements.
- `NULL` becomes `NULL`; `bool` becomes `1`/`0`; `time.Time` becomes a UTC
  `YYYY-MM-DD hh:mm:ss` literal (the format GoatCounter's SQLite check
  constraints expect); `[]byte` becomes `X'hex'`.
- Results decode `date`, `timestamp`, `timestamptz`, and `time` OIDs to
  `time.Time`, and decode `bytea` values that arrive raw, as `\x` hex, or as
  Go's decimal byte list. A `TEXT` column is never reinterpreted as a time even
  when its value happens to look like one, so string and `[]byte` destinations
  receive the exact stored value.
- Inserts executed through `Exec` recover the ID with a follow-up
  `SELECT last_insert_rowid()` on the same connection. The follow-up is never
  treated as retryable, so an acknowledged insert is not replayed.
- `RETURNING` is passed through untouched. The driver neither synthesises nor
  strips it; the engine's real behaviour decides whether the statement works.
- Explicit transaction isolation levels and read-only transactions are
  rejected, matching the managed engine's support.

## Required engine capabilities

`CheckCapabilities` probes the engine once per pool with read-only statements.
GoatCounter fails fast if any scalar capability is missing, so startup never
needs write or alter_table scope and never mutates the tenant:

| Capability | Probe (read-only) |
| --- | --- |
| SQLite 3.35 or newer | `select sqlite_version()` parses as `>= 3.35` |
| Session-local insert IDs | `select last_insert_rowid()` |
| JSON functions | `json(...)` and `json_extract(...)` |
| `percent_diff` | `select percent_diff(1, 2)` |

`RETURNING` and stored generated columns cannot be checked without executing
DDL/DML, so they are not probed at connect time. `VerifyBehavior` checks them by
creating and dropping uniquely named tables, and is called only from the
explicit integration tests against a disposable database. `RETURNING` itself is
never synthesised or stripped: the engine sees the caller's statement verbatim.

## Migration and verification

On a fresh managed database the schema and every migration run through zdb:

```sh
goatcounter db migrate all -createdb -db "sqlite/pizzasql+$DSN"
goatcounter db test -db "sqlite/pizzasql+$DSN"
```

`serve -automigrate` runs pending migrations at startup. Migrations are
forward-only; use a disposable database when testing. zdb's migration wrapper
issues `PRAGMA foreign_keys = off`, and historical GoatCounter migrations issue
`ANALYZE`; PizzaSQL accepts both as explicit no-ops, and the pgproxy requires
`alter_table` scope for them.

Before upgrading an existing populated tenant, snapshot it and verify the
migration against a copy. Historical migrations rebuild and rename the `hits`
and `ref_counts` tables; PizzaSQL streams large renames in bounded atomic
batches. If a migration is interrupted, keep GoatCounter stopped and recover
from the snapshot or complete the rename on a copy before serving traffic.

The driver and adapter have focused tests that do not need an engine:

```sh
go test ./pkg/pizzasql/
```

Real-engine tests are gated on `PIZZASQL_TEST_DSN`. `TestPizzaSQLIntegration`
(in `gctest`) creates the schema, runs all migrations, and stores and counts
hits; `TestIntegrationDriverWorkflow` (in `pkg/pizzasql`) exercises binding,
`RETURNING`, blob and time round-trips, JSON, `percent_diff`, multi-statement
schema, and transactions. Both skip when the engine lacks a required
capability.

`scripts/pizzasql-local.sh` runs those tests against an already-running local
`app.database.pizza` pgproxy. It reads credentials only from the environment,
never prints them, accepts loopback targets only, and creates no resources:

```sh
PIZZASQL_LOCAL_ORG=myorg PIZZASQL_LOCAL_DB=mydb \
PIZZASQL_LOCAL_API_KEY=... scripts/pizzasql-local.sh
```

## No local SQLite policy

The `pizzasql` driver only ever dials a PizzaSQL tenant. It does not open,
create, or fall back to a SQLite file. Ordinary `sqlite+...` and `sqlite3+...`
connect strings continue to use `zgo.at/zdb-drivers/go-sqlite3` unchanged.

## Deployment prerequisites

- A managed PizzaSQL database (`org/db`) exists and is reachable through the
  pgproxy on `127.0.0.1:5432` (or another trusted/loopback hop).
- An API key with the scopes needed for the intended operation; the complete
  historical migration set needs write, alter_table, and drop_table.
- `sslmode=disable` only on the loopback hop. Do not expose an unencrypted
  PizzaSQL listener.
- The engine exposes every required capability above; otherwise `serve` refuses
  to start with an `UnsupportedError`.
- Keep the pool small (`-dbconn=2,2`) so one process stays inside the
  per-organization connection quota.

## Production service

The checked-in FreeBSD service runs `goatcounter.database.pizza` on loopback
port 3002. Install three files:

- `deploy/goatcounter.rc` as `/usr/local/etc/rc.d/goatcounter`
- `deploy/goatcounter-run` as
  `/home/pizzadatabase/goatcounter/shared/goatcounter-run`
- a mode-0600 copy of `deploy/goatcounter.env.example` as
  `/home/pizzadatabase/goatcounter/shared/goatcounter.env`

The environment file and launcher are persistent state and must not be placed
in a release directory. `goatcounter-run` sources the environment file and
execs the served command; the rc script deliberately does not name its
environment variable `<name>_env`, because rc.subr reserves that suffix for an
environment file and would prepend `env <file>` to the command.

The service keeps releases under `/home/pizzadatabase/goatcounter/releases`
and resolves the active binary through the `current` symlink. It runs as the
dedicated `goatcounter` account, listens only on `127.0.0.1:3002`, enables
forward-only migrations at startup, and fixes the connection pool at `2,2`.
Changing serve flags means updating `goatcounter-run`; a new release only needs
the `current` symlink repointed.

Install the `goatcounter.database.pizza` block from the workspace's canonical
full-site Caddyfile before enabling the service. Validate both configurations
before startup:

```sh
sh -n /usr/local/etc/rc.d/goatcounter
caddy validate --config /usr/local/etc/caddy/Caddyfile
```

Provisioning, secret rotation, database snapshots, service restarts, and Caddy
reloads remain explicit operator actions. Never put the managed API key in a
command-line argument or a release artifact.

## Analytics and privacy

Production tracking uses one GoatCounter site at `goatcounter.database.pizza`.
Browsers load `count.js` from that origin and POST to
`https://goatcounter.database.pizza/count`. Each service prefixes the reported
path so the combined dashboard stays attributable:

| Source | Pageview prefix |
| --- | --- |
| Marketing site and blog | `/database.pizza` |
| Managed console | `/app.database.pizza` |
| Gogs | `/git.database.pizza` |
| Vikunja | `/tasks.database.pizza` |

The prefix is applied in the browser by a `window.goatcounter.path` callback in
each service's HTML shell; nothing domain-specific is stored server-side, which
is why every count goes to the single site.

Custom adoption events for the console use the
`app.database.pizza/event/<name>` namespace (GoatCounter trims the leading slash
from event paths, keeping them distinct from pageviews):

- `signup` — a console account finished passkey registration.
- `first_database` — the first database created from this browser.
- `first_query` — the first successful query run from this browser.

Events are emitted at most once per browser using a `localStorage` marker; the
marker never leaves the browser and identifies no one.

Privacy posture:

- GoatCounter does not set tracking cookies. It stores aggregate pageviews,
  paths, titles, referrers, screen sizes, and coarse location; it never stores
  raw IP addresses, names, emails, API keys, database URLs, query text,
  organization or database names, form values, or any user identifier.
- The complete allowlist of fields and event names is the table and list above.
  Never send secrets or tenant data through the counting endpoint.
- The GoatCounter dashboard itself is not tracked; only the four services are.
- Retention: the site currently keeps aggregate data indefinitely
  (`data_retention = 0`). Set a positive number of days in the site's settings
  if a shorter window is required.
- Consent: no cookie banner is used because no cookies or personal identifiers
  are stored. This section is the record to review against applicable privacy
  requirements.

### Reports

GoatCounter has no built-in multi-report or funnel view, so reports are the
dashboard filter URLs below. Append the filter to the dashboard as
`?filter=...`; `at:start` anchors the match to the start of the path and
`is:pageview` excludes events.

| Report | Filter |
| --- | --- |
| Marketing traffic | `in:path at:start /database.pizza` |
| Console pageviews | `in:path at:start /app.database.pizza is:pageview` |
| Signups | `in:path at:start app.database.pizza/event/signup` |
| First database | `in:path at:start app.database.pizza/event/first_database` |
| First query | `in:path at:start app.database.pizza/event/first_query` |

The adoption funnel is the ordered sequence console pageview → signup →
`first_database` → `first_query`. Read each step's total from its report (or
from the HTTP API) and compute the conversion outside GoatCounter.

Every report contains aggregate, non-identifying data only (path, title, count,
referrer, and coarse dimensions); there is no per-user view.
