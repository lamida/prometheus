// Package catalog maps SQL tables/columns onto Prometheus metrics/labels.
//
// PromQL has no schema of its own: a metric name and its label names cannot
// be inferred from SQL syntax. The catalog supplies that mapping externally,
// typically loaded from a YAML file checked in alongside the queries that use
// it.
package catalog

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

// Table describes how one SQL table name maps onto one Prometheus metric.
type Table struct {
	// Metric is the Prometheus metric name backing this table.
	Metric string `yaml:"metric"`
	// ValueColumn is the SQL column name that stands for the sample value.
	// Defaults to "value" if empty.
	ValueColumn string `yaml:"value_column"`
	// Columns maps SQL column names to Prometheus label names. Columns not
	// listed here (other than ValueColumn) are rejected at translation time.
	Columns map[string]string `yaml:"columns"`
}

// Catalog is a set of tables keyed by SQL table name.
type Catalog struct {
	Tables map[string]Table `yaml:"tables"`
}

// Parse reads a Catalog from YAML.
func Parse(data []byte) (*Catalog, error) {
	var c Catalog
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing catalog: %w", err)
	}
	for name, t := range c.Tables {
		if t.Metric == "" {
			return nil, fmt.Errorf("table %q: metric name is required", name)
		}
	}
	return &c, nil
}

// Table looks up a table by SQL name, applying the ValueColumn default.
func (c *Catalog) Table(name string) (Table, error) {
	t, ok := c.Tables[name]
	if !ok {
		return Table{}, fmt.Errorf("unknown table %q: not present in catalog", name)
	}
	if t.ValueColumn == "" {
		t.ValueColumn = "value"
	}
	return t, nil
}

// Label resolves a SQL column name to a Prometheus label name for this table.
func (t Table) Label(column string) (string, error) {
	label, ok := t.Columns[column]
	if !ok {
		return "", fmt.Errorf("unknown column %q for metric %q: not present in catalog", column, t.Metric)
	}
	return label, nil
}
