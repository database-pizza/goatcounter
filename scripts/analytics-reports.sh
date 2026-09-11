#!/bin/sh
#
# Print the GoatCounter traffic and adoption reports, plus the ordered adoption
# funnel. Read-only: it never writes to the tenant.
#
# Usage:
#   GOATCOUNTER_DB='sqlite/pizzasql+...' ./scripts/analytics-reports.sh
#
# GoatCounter has no native funnel, so the funnel below is the step counts for
# the ordered events defined in DATABASE_PIZZA.md. Set GOATCOUNTER_BIN if the
# binary is not on PATH.

set -eu

if [ -z "${GOATCOUNTER_DB:-}" ]; then
	echo "analytics-reports: GOATCOUNTER_DB must be set" >&2
	exit 1
fi

GC=${GOATCOUNTER_BIN:-goatcounter}
export GOATCOUNTER_DB

# The managed database.pizza deployment uses the single active site (site_id=1).
SITE=1

run() {
	"$GC" db query "$1"
}

echo "== marketing traffic =="
run "select p.path, sum(hc.total) as visits from hit_counts hc join paths p on p.path_id=hc.path_id where hc.site_id=$SITE and p.path like '/database.pizza%' group by p.path order by visits desc"

echo "== console pageviews =="
run "select p.path, sum(hc.total) as visits from hit_counts hc join paths p on p.path_id=hc.path_id where hc.site_id=$SITE and p.path like '/app.database.pizza%' and p.event=0 group by p.path order by visits desc"

echo "== adoption funnel =="
run "select p.path, sum(hc.total) as visits from hit_counts hc join paths p on p.path_id=hc.path_id where hc.site_id=$SITE and p.path in ('/app.database.pizza', 'app.database.pizza/event/signup', 'app.database.pizza/event/first_database', 'app.database.pizza/event/first_query') group by p.path order by p.path"
