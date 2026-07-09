package translator

import (
	"strings"
	"testing"

	"github.com/prometheus/prometheus/cmd/sql2promql/catalog"
	"github.com/prometheus/prometheus/promql/parser"
)

const testCatalogYAML = `
tables:
  http_requests:
    metric: http_requests_total
    value_column: value
    columns:
      job: job
      instance: instance
      status: status
      method: method
`

func mustCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Parse([]byte(testCatalogYAML))
	if err != nil {
		t.Fatalf("parsing test catalog: %v", err)
	}
	return cat
}

func TestTranslate_Accept(t *testing.T) {
	cat := mustCatalog(t)
	promParser := parser.NewParser(parser.Options{})

	cases := []struct {
		name string
		sql  string
		want string
	}{
		{
			name: "bare value with equality filter",
			sql:  `SELECT value FROM http_requests WHERE job = 'api'`,
			want: `http_requests_total{job="api"}`,
		},
		{
			name: "not equal filter",
			sql:  `SELECT value FROM http_requests WHERE job != 'api'`,
			want: `http_requests_total{job!="api"}`,
		},
		{
			name: "sum with group by",
			sql:  `SELECT sum(value) FROM http_requests WHERE job = 'api' GROUP BY instance`,
			want: `sum by (instance) (http_requests_total{job="api"})`,
		},
		{
			name: "avg no grouping",
			sql:  `SELECT avg(value) FROM http_requests`,
			want: `avg(http_requests_total)`,
		},
		{
			name: "count star",
			sql:  `SELECT count(*) FROM http_requests WHERE job = 'api'`,
			want: `count(http_requests_total{job="api"})`,
		},
		{
			name: "in becomes anchored regex alternation",
			sql:  `SELECT sum(value) FROM http_requests WHERE status IN ('500', '502')`,
			want: `sum(http_requests_total{status=~"^(?:500|502)$"})`,
		},
		{
			name: "not in becomes negative regex",
			sql:  `SELECT sum(value) FROM http_requests WHERE status NOT IN ('200')`,
			want: `sum(http_requests_total{status!~"^(?:200)$"})`,
		},
		{
			name: "like becomes anchored regex",
			sql:  `SELECT avg(value) FROM http_requests WHERE method LIKE 'GET%'`,
			want: `avg(http_requests_total{method=~"^GET.*$"})`,
		},
		{
			name: "and of two filters",
			sql:  `SELECT sum(value) FROM http_requests WHERE job = 'api' AND status = '500'`,
			want: `sum(http_requests_total{job="api",status="500"})`,
		},
		{
			name: "having filters the aggregate",
			sql:  `SELECT sum(value) FROM http_requests GROUP BY instance HAVING sum(value) > 100`,
			want: `sum by (instance) (http_requests_total) > 100`,
		},
		{
			name: "order by desc limit becomes topk",
			sql:  `SELECT sum(value) FROM http_requests GROUP BY instance ORDER BY sum(value) DESC LIMIT 5`,
			want: `topk(5, sum by (instance) (http_requests_total))`,
		},
		{
			name: "order by asc limit becomes bottomk",
			sql:  `SELECT sum(value) FROM http_requests GROUP BY instance ORDER BY sum(value) ASC LIMIT 3`,
			want: `bottomk(3, sum by (instance) (http_requests_total))`,
		},
		{
			name: "full pipeline",
			sql: `SELECT sum(value) FROM http_requests
			      WHERE job = 'api' AND status IN ('500', '502')
			      GROUP BY instance
			      HAVING sum(value) > 100
			      ORDER BY sum(value) DESC LIMIT 5`,
			want: `topk(5, sum by (instance) (http_requests_total{job="api",status=~"^(?:500|502)$"}) > 100)`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Translate(tc.sql, cat)
			if err != nil {
				t.Fatalf("Translate(%q) returned error: %v", tc.sql, err)
			}
			if got != tc.want {
				t.Errorf("Translate(%q) = %q, want %q", tc.sql, got, tc.want)
			}
			// The output must itself be valid PromQL: round-trip it through
			// the real parser as a sanity check that we never emit garbage.
			if _, err := promParser.ParseExpr(got); err != nil {
				t.Errorf("Translate(%q) produced unparseable PromQL %q: %v", tc.sql, got, err)
			}
		})
	}
}

func TestTranslate_Reject(t *testing.T) {
	cat := mustCatalog(t)

	cases := []struct {
		name      string
		sql       string
		wantError string
	}{
		{
			name:      "join",
			sql:       `SELECT sum(value) FROM http_requests a JOIN http_requests b ON a.job = b.job`,
			wantError: "joins are not supported",
		},
		{
			name:      "distinct",
			sql:       `SELECT DISTINCT value FROM http_requests`,
			wantError: "DISTINCT",
		},
		{
			name:      "time predicate",
			sql:       `SELECT sum(value) FROM http_requests WHERE time > 100`,
			wantError: "time-range predicates are not supported",
		},
		{
			name:      "unknown table",
			sql:       `SELECT sum(value) FROM nope`,
			wantError: `unknown table "nope"`,
		},
		{
			name:      "unknown column",
			sql:       `SELECT sum(value) FROM http_requests WHERE nope = 'x'`,
			wantError: `unknown column "nope"`,
		},
		{
			name:      "unknown aggregate function",
			sql:       `SELECT stddev(value) FROM http_requests`,
			wantError: "unsupported aggregate function",
		},
		{
			name:      "or across columns",
			sql:       `SELECT sum(value) FROM http_requests WHERE job = 'api' OR status = '500'`,
			wantError: "OR",
		},
		{
			name:      "group by without aggregate",
			sql:       `SELECT value FROM http_requests GROUP BY instance`,
			wantError: "GROUP BY requires an aggregate function",
		},
		{
			name:      "order by without limit",
			sql:       `SELECT sum(value) FROM http_requests ORDER BY sum(value) DESC`,
			wantError: "ORDER BY without LIMIT",
		},
		{
			name:      "subquery in from",
			sql:       `SELECT sum(value) FROM (SELECT value FROM http_requests) t`,
			wantError: "subqueries are not supported",
		},
		{
			name:      "union",
			sql:       `SELECT value FROM http_requests UNION SELECT value FROM http_requests`,
			wantError: "unsupported statement",
		},
		{
			// The underlying SQL parser itself doesn't support INTERVAL
			// arithmetic, so this fails before our own time-predicate check
			// ever runs. Documented explicitly so the boundary is visible.
			name:      "interval arithmetic is rejected by the SQL parser itself",
			sql:       `SELECT sum(value) FROM http_requests WHERE time > now() - interval '5m'`,
			wantError: "parsing SQL",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Translate(tc.sql, cat)
			if err == nil {
				t.Fatalf("Translate(%q) succeeded, want error containing %q", tc.sql, tc.wantError)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("Translate(%q) error = %q, want it to contain %q", tc.sql, err.Error(), tc.wantError)
			}
		})
	}
}
