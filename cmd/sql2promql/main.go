// Command sql2promql translates a constrained subset of SQL into PromQL,
// using a catalog file to map SQL tables/columns onto Prometheus
// metrics/labels. It is a prototype: see translator.Translate for exactly
// what is and isn't supported.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/prometheus/prometheus/cmd/sql2promql/catalog"
	"github.com/prometheus/prometheus/cmd/sql2promql/translator"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sql2promql", flag.ContinueOnError)
	fs.SetOutput(stderr)
	sqlFlag := fs.String("sql", "", "SQL query to translate. If empty, read from stdin.")
	catalogFlag := fs.String("catalog", "", "Path to a YAML catalog file mapping SQL tables/columns to metrics/labels (required).")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *catalogFlag == "" {
		fmt.Fprintln(stderr, "sql2promql: -catalog is required")
		return 2
	}
	data, err := os.ReadFile(*catalogFlag)
	if err != nil {
		fmt.Fprintf(stderr, "sql2promql: reading catalog: %v\n", err)
		return 1
	}
	cat, err := catalog.Parse(data)
	if err != nil {
		fmt.Fprintf(stderr, "sql2promql: %v\n", err)
		return 1
	}

	sql := *sqlFlag
	if sql == "" {
		b, err := io.ReadAll(stdin)
		if err != nil {
			fmt.Fprintf(stderr, "sql2promql: reading SQL from stdin: %v\n", err)
			return 1
		}
		sql = string(b)
	}

	promql, err := translator.Translate(sql, cat)
	if err != nil {
		fmt.Fprintf(stderr, "sql2promql: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, promql)
	return 0
}
