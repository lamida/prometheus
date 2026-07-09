# sql2promql (prototype)

Translates a constrained subset of SQL into PromQL, using a YAML catalog to
map SQL tables/columns onto Prometheus metrics/labels.

This is a separate Go module (see `go.mod`) so its dependency on
`github.com/xwb1989/sqlparser` doesn't leak into the main Prometheus module.
It is wired into the repo's `go.work` workspace so it builds against the
in-tree `promql/parser` package.

## Supported SQL shape

```sql
SELECT <agg>(value) FROM <table>
[WHERE <col> {=|!=} <literal> [AND ...]]
[WHERE <col> IN (<literal>, ...)]
[WHERE <col> LIKE <pattern>]
[GROUP BY <col> [, ...]]
[HAVING <agg>(value) {=|!=|<|<=|>|>=} <number>]
[ORDER BY <agg>(value) {ASC|DESC} LIMIT <n>]
```

`<agg>` is one of `sum`, `avg`, `min`, `max`, `count`. `LIKE`'s `%`/`_`
wildcards become an anchored RE2 pattern; `IN`/`NOT IN` become an anchored
regexp alternation.

Explicitly **not** supported, and rejected with a specific error rather than
translated approximately: joins, subqueries, `DISTINCT`, `UNION`, window
functions, `OR` across different columns, and any `WHERE`/time-range
predicate on a column named `time` (PromQL's range/instant query shape has no
SQL-syntax equivalent to translate against in this prototype).

See `translator.Translate` and its tests for the exact rules.

## Catalog format

```yaml
tables:
  http_requests:
    metric: http_requests_total
    value_column: value  # optional, defaults to "value"
    columns:
      job: job
      instance: instance
      status: status
```

## Usage

```sh
go run . -catalog testdata/catalog.yaml -sql "SELECT sum(value) FROM http_requests WHERE job='api' GROUP BY instance"
# sum by (instance) (http_requests_total{job="api"})
```

Or pipe SQL via stdin by omitting `-sql`.
