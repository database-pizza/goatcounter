package gctest

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"zgo.at/goatcounter/v2"
	"zgo.at/goatcounter/v2/pkg/pizzasql"
	"zgo.at/zdb"
	"zgo.at/zstd/ztime"
)

// TestPizzaSQLIntegration runs the full GoatCounter schema, every migration,
// and a representative workflow against a real PizzaSQL engine. It is gated on
// PIZZASQL_TEST_DSN and otherwise skipped, so an ordinary test run never talks
// to a remote database.
//
// The DSN must point at a disposable, empty database owned by a key that has
// write and alter_table scopes: this test creates the schema and runs all
// migrations.
func TestPizzaSQLIntegration(t *testing.T) {
	dsn := os.Getenv("PIZZASQL_TEST_DSN")
	if dsn == "" {
		t.Skip("PIZZASQL_TEST_DSN not set; skipping PizzaSQL integration test")
	}

	raw, err := sql.Open(pizzasql.DriverName, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := raw.Ping(); err != nil {
		t.Fatalf("connect to PizzaSQL: %s", err)
	}
	if _, err := pizzasql.CheckCapabilities(context.Background(), raw); err != nil {
		t.Skipf("engine cannot run GoatCounter: %s", err)
	}
	// RETURNING and generated columns are verified behaviourally, not at
	// connect time, because the probes need DDL/DML.
	if _, err := pizzasql.VerifyBehavior(context.Background(), raw); err != nil {
		t.Skipf("engine cannot run GoatCounter: %s", err)
	}

	// DB() creates the schema, runs all migrations, and seeds a site and user.
	ctx := DB(t)
	if d := zdb.SQLDialect(ctx); d != zdb.DialectSQLite {
		t.Fatalf("dialect = %s; want SQLite", d)
	}
	ctx = ztime.WithNow(ctx, ztime.FromString("2024-05-06 07:08:09"))

	var configured goatcounter.Site
	configured.Defaults(ctx)
	configured.Code = "pizzasql"
	configured.Settings.Collect.Set(goatcounter.CollectHits)
	ctx = Site(ctx, t, &configured, nil)
	site := goatcounter.GetSite(ctx)

	StoreHits(ctx, t, false,
		goatcounter.Hit{Path: "/one", FirstVisit: true,
			UserAgentHeader: "Mozilla/5.0 (X11; Linux x86_64; rv:79.0) Gecko/20100101 Firefox/79.0"},
		goatcounter.Hit{Path: "signup", Event: true,
			UserAgentHeader: "Mozilla/5.0 (X11; Linux x86_64; rv:79.0) Gecko/20100101 Firefox/79.0"},
	)

	var hits, paths int
	if err := zdb.Get(ctx, &hits, `select count(*) from hits where site_id = ?`, site.ID); err != nil {
		t.Fatalf("count hits: %s", err)
	}
	if err := zdb.Get(ctx, &paths, `select count(*) from paths where site_id = ?`, site.ID); err != nil {
		t.Fatalf("count paths: %s", err)
	}
	if hits != 2 {
		t.Errorf("hits = %d; want 2", hits)
	}
	if paths != 2 {
		t.Errorf("paths = %d; want 2", paths)
	}
}
