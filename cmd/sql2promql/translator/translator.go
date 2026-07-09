// Package translator converts a constrained subset of SQL into PromQL.
//
// Supported shape:
//
//	SELECT <agg>(value) FROM <table>
//	[WHERE <col> {=|!=} <literal> [AND ...]]
//	[WHERE <col> IN (<literal>, ...)]
//	[WHERE <col> LIKE <pattern>]
//	[GROUP BY <col> [, ...]]
//	[HAVING <agg>(value) {=|!=|<|<=|>|>=} <number>]
//	[ORDER BY <agg>(value) {ASC|DESC} LIMIT <n>]
//
// Anything outside this shape (joins, subqueries, DISTINCT, UNION, time
// predicates, OR across different columns, window functions, ...) is
// rejected with an explicit error rather than translated approximately.
package translator

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/grafana/regexp"
	"github.com/xwb1989/sqlparser"

	"github.com/prometheus/prometheus/cmd/sql2promql/catalog"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

// aggOps maps supported SQL aggregate function names to PromQL aggregation
// operators.
var aggOps = map[string]parser.ItemType{
	"sum":   parser.SUM,
	"avg":   parser.AVG,
	"min":   parser.MIN,
	"max":   parser.MAX,
	"count": parser.COUNT,
}

// cmpOps maps supported SQL comparison operators to PromQL comparison item
// types, for use in HAVING clauses.
var cmpOps = map[string]parser.ItemType{
	sqlparser.EqualStr:        parser.EQLC,
	sqlparser.NotEqualStr:     parser.NEQ,
	sqlparser.LessThanStr:     parser.LSS,
	sqlparser.LessEqualStr:    parser.LTE,
	sqlparser.GreaterThanStr:  parser.GTR,
	sqlparser.GreaterEqualStr: parser.GTE,
}

// Translate converts a single SQL SELECT statement into a PromQL query
// string, using cat to resolve table/column names to metric/label names.
func Translate(sql string, cat *catalog.Catalog) (string, error) {
	stmt, err := sqlparser.Parse(sql)
	if err != nil {
		return "", fmt.Errorf("parsing SQL: %w", err)
	}

	sel, ok := stmt.(*sqlparser.Select)
	if !ok {
		return "", fmt.Errorf("unsupported statement %T: only SELECT is supported", stmt)
	}
	if sel.Distinct != "" {
		return "", errors.New("unsupported: DISTINCT has no PromQL equivalent")
	}

	table, err := singleTable(sel.From, cat)
	if err != nil {
		return "", err
	}

	aggOp, isBare, err := selectAggregate(sel.SelectExprs, table)
	if err != nil {
		return "", err
	}

	vs := &parser.VectorSelector{
		Name: table.Metric,
		LabelMatchers: []*labels.Matcher{
			labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, table.Metric),
		},
	}

	if sel.Where != nil {
		matchers, err := whereMatchers(sel.Where.Expr, table)
		if err != nil {
			return "", err
		}
		vs.LabelMatchers = append(vs.LabelMatchers, matchers...)
	}

	var expr parser.Expr = vs

	grouping, err := groupingLabels(sel.GroupBy, table)
	if err != nil {
		return "", err
	}
	if len(grouping) > 0 && isBare {
		return "", errors.New("unsupported: GROUP BY requires an aggregate function in SELECT")
	}
	if !isBare {
		expr = &parser.AggregateExpr{
			Op:       aggOp,
			Expr:     expr,
			Grouping: grouping,
		}
	}

	if sel.Having != nil {
		expr, err = applyHaving(expr, sel.Having.Expr)
		if err != nil {
			return "", err
		}
	}

	if len(sel.OrderBy) > 0 || sel.Limit != nil {
		expr, err = applyOrderLimit(expr, sel.OrderBy, sel.Limit)
		if err != nil {
			return "", err
		}
	}

	return expr.String(), nil
}

func singleTable(from sqlparser.TableExprs, cat *catalog.Catalog) (catalog.Table, error) {
	if len(from) != 1 {
		return catalog.Table{}, fmt.Errorf("unsupported: exactly one table is required, got %d", len(from))
	}
	aliased, ok := from[0].(*sqlparser.AliasedTableExpr)
	if !ok {
		return catalog.Table{}, fmt.Errorf("unsupported FROM clause %T: joins are not supported", from[0])
	}
	tableName, ok := aliased.Expr.(sqlparser.TableName)
	if !ok {
		return catalog.Table{}, fmt.Errorf("unsupported FROM expression %T: subqueries are not supported", aliased.Expr)
	}
	return cat.Table(tableName.Name.String())
}

// selectAggregate inspects the (single) select expression and returns the
// aggregation operator to use. isBare is true when the query selects the raw
// value column with no aggregate function.
func selectAggregate(exprs sqlparser.SelectExprs, table catalog.Table) (parser.ItemType, bool, error) {
	if len(exprs) != 1 {
		return 0, false, fmt.Errorf("unsupported: exactly one select expression is required, got %d", len(exprs))
	}
	aliased, ok := exprs[0].(*sqlparser.AliasedExpr)
	if !ok {
		return 0, false, fmt.Errorf("unsupported select expression %T", exprs[0])
	}

	switch e := aliased.Expr.(type) {
	case *sqlparser.ColName:
		if e.Name.String() != table.ValueColumn {
			return 0, false, fmt.Errorf("unsupported: SELECT of column %q, only %q or an aggregate over it is supported", e.Name.String(), table.ValueColumn)
		}
		return 0, true, nil
	case *sqlparser.FuncExpr:
		op, ok := aggOps[strings.ToLower(e.Name.String())]
		if !ok {
			return 0, false, fmt.Errorf("unsupported aggregate function %q", e.Name.String())
		}
		if err := checkAggregateArg(e, table); err != nil {
			return 0, false, err
		}
		return op, false, nil
	default:
		return 0, false, fmt.Errorf("unsupported select expression %T", aliased.Expr)
	}
}

func checkAggregateArg(f *sqlparser.FuncExpr, table catalog.Table) error {
	if len(f.Exprs) != 1 {
		return fmt.Errorf("unsupported: %s() takes exactly one argument", f.Name.String())
	}
	switch a := f.Exprs[0].(type) {
	case *sqlparser.StarExpr:
		if strings.ToLower(f.Name.String()) != "count" {
			return fmt.Errorf("unsupported: %s(*) is only supported for count()", f.Name.String())
		}
		return nil
	case *sqlparser.AliasedExpr:
		col, ok := a.Expr.(*sqlparser.ColName)
		if !ok || col.Name.String() != table.ValueColumn {
			return fmt.Errorf("unsupported: aggregate functions may only be applied to %q", table.ValueColumn)
		}
		return nil
	default:
		return fmt.Errorf("unsupported aggregate argument %T", a)
	}
}

// whereMatchers walks a WHERE expression tree and produces label matchers.
// Only conjunctions (AND) of per-column equality/inequality/IN/LIKE
// predicates are supported; anything else (OR across columns, time
// predicates, subqueries, ...) is rejected explicitly.
func whereMatchers(expr sqlparser.Expr, table catalog.Table) ([]*labels.Matcher, error) {
	switch e := expr.(type) {
	case *sqlparser.AndExpr:
		left, err := whereMatchers(e.Left, table)
		if err != nil {
			return nil, err
		}
		right, err := whereMatchers(e.Right, table)
		if err != nil {
			return nil, err
		}
		return append(left, right...), nil

	case *sqlparser.ParenExpr:
		return whereMatchers(e.Expr, table)

	case *sqlparser.OrExpr:
		return nil, errors.New("unsupported: OR in WHERE is only supported as part of IN(...) on a single column")

	case *sqlparser.ComparisonExpr:
		return comparisonMatcher(e, table)

	default:
		return nil, fmt.Errorf("unsupported WHERE predicate %T", expr)
	}
}

func comparisonMatcher(e *sqlparser.ComparisonExpr, table catalog.Table) ([]*labels.Matcher, error) {
	col, ok := e.Left.(*sqlparser.ColName)
	if !ok {
		return nil, errors.New("unsupported WHERE predicate: left-hand side must be a column")
	}
	colName := col.Name.String()
	if colName == "time" {
		return nil, errors.New("unsupported: time-range predicates are not supported by this prototype; express the range in the surrounding query tooling instead")
	}
	label, err := table.Label(colName)
	if err != nil {
		return nil, err
	}

	switch e.Operator {
	case sqlparser.EqualStr, sqlparser.NotEqualStr:
		val, err := sqlValString(e.Right)
		if err != nil {
			return nil, err
		}
		mt := labels.MatchEqual
		if e.Operator == sqlparser.NotEqualStr {
			mt = labels.MatchNotEqual
		}
		m, err := labels.NewMatcher(mt, label, val)
		if err != nil {
			return nil, err
		}
		return []*labels.Matcher{m}, nil

	case sqlparser.InStr, sqlparser.NotInStr:
		tuple, ok := e.Right.(sqlparser.ValTuple)
		if !ok {
			return nil, fmt.Errorf("unsupported IN right-hand side %T", e.Right)
		}
		alts := make([]string, 0, len(tuple))
		for _, v := range tuple {
			s, err := sqlValString(v)
			if err != nil {
				return nil, err
			}
			alts = append(alts, regexp.QuoteMeta(s))
		}
		mt := labels.MatchRegexp
		if e.Operator == sqlparser.NotInStr {
			mt = labels.MatchNotRegexp
		}
		m, err := labels.NewMatcher(mt, label, "^(?:"+strings.Join(alts, "|")+")$")
		if err != nil {
			return nil, err
		}
		return []*labels.Matcher{m}, nil

	case sqlparser.LikeStr, sqlparser.NotLikeStr:
		val, err := sqlValString(e.Right)
		if err != nil {
			return nil, err
		}
		mt := labels.MatchRegexp
		if e.Operator == sqlparser.NotLikeStr {
			mt = labels.MatchNotRegexp
		}
		m, err := labels.NewMatcher(mt, label, likeToRegexp(val))
		if err != nil {
			return nil, err
		}
		return []*labels.Matcher{m}, nil

	default:
		return nil, fmt.Errorf("unsupported WHERE operator %q", e.Operator)
	}
}

// likeToRegexp converts a SQL LIKE pattern (% and _ wildcards) into an
// anchored RE2 pattern suitable for a PromQL =~ matcher.
func likeToRegexp(pattern string) string {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return b.String()
}

func sqlValString(expr sqlparser.Expr) (string, error) {
	val, ok := expr.(*sqlparser.SQLVal)
	if !ok {
		return "", fmt.Errorf("unsupported literal %T", expr)
	}
	switch val.Type {
	case sqlparser.StrVal, sqlparser.IntVal, sqlparser.FloatVal:
		return string(val.Val), nil
	default:
		return "", fmt.Errorf("unsupported literal type for %q", string(val.Val))
	}
}

func groupingLabels(groupBy sqlparser.GroupBy, table catalog.Table) ([]string, error) {
	if len(groupBy) == 0 {
		return nil, nil
	}
	labelsOut := make([]string, 0, len(groupBy))
	for _, expr := range groupBy {
		col, ok := expr.(*sqlparser.ColName)
		if !ok {
			return nil, fmt.Errorf("unsupported GROUP BY expression %T", expr)
		}
		label, err := table.Label(col.Name.String())
		if err != nil {
			return nil, err
		}
		labelsOut = append(labelsOut, label)
	}
	return labelsOut, nil
}

func applyHaving(expr parser.Expr, having sqlparser.Expr) (parser.Expr, error) {
	cmp, ok := having.(*sqlparser.ComparisonExpr)
	if !ok {
		return nil, fmt.Errorf("unsupported HAVING predicate %T: only a single comparison is supported", having)
	}
	op, ok := cmpOps[cmp.Operator]
	if !ok {
		return nil, fmt.Errorf("unsupported HAVING operator %q", cmp.Operator)
	}
	// The left-hand side is expected to be the same aggregate already built
	// into expr; we don't re-derive it, we just require the right-hand side
	// to be a numeric literal to compare against.
	if _, ok := cmp.Left.(*sqlparser.FuncExpr); !ok {
		return nil, fmt.Errorf("unsupported HAVING left-hand side %T: must be the aggregate function from SELECT", cmp.Left)
	}
	n, err := numberLiteral(cmp.Right)
	if err != nil {
		return nil, err
	}
	return &parser.BinaryExpr{
		Op:  op,
		LHS: expr,
		RHS: n,
	}, nil
}

func numberLiteral(expr sqlparser.Expr) (*parser.NumberLiteral, error) {
	val, ok := expr.(*sqlparser.SQLVal)
	if !ok || (val.Type != sqlparser.IntVal && val.Type != sqlparser.FloatVal) {
		return nil, fmt.Errorf("unsupported literal %T: expected a number", expr)
	}
	f, err := strconv.ParseFloat(string(val.Val), 64)
	if err != nil {
		return nil, fmt.Errorf("parsing number %q: %w", string(val.Val), err)
	}
	return &parser.NumberLiteral{Val: f}, nil
}

func applyOrderLimit(expr parser.Expr, orderBy sqlparser.OrderBy, limit *sqlparser.Limit) (parser.Expr, error) {
	if limit == nil {
		return nil, errors.New("unsupported: ORDER BY without LIMIT has no PromQL equivalent")
	}
	if len(orderBy) != 1 {
		return nil, fmt.Errorf("unsupported: exactly one ORDER BY expression is required, got %d", len(orderBy))
	}
	switch orderBy[0].Expr.(type) {
	case *sqlparser.FuncExpr, *sqlparser.ColName:
		// Assumed to reference the same aggregate/value already built into
		// expr; PromQL's topk/bottomk always rank by the expression itself.
	default:
		return nil, fmt.Errorf("unsupported ORDER BY expression %T", orderBy[0].Expr)
	}

	n, err := numberLiteral(limit.Rowcount)
	if err != nil {
		return nil, fmt.Errorf("unsupported LIMIT: %w", err)
	}

	op := parser.ItemType(parser.TOPK)
	if orderBy[0].Direction == sqlparser.AscScr {
		op = parser.BOTTOMK
	}
	return &parser.AggregateExpr{
		Op:    op,
		Expr:  expr,
		Param: n,
	}, nil
}
