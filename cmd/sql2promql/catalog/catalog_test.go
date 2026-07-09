package catalog

import "testing"

const testYAML = `
tables:
  http_requests:
    metric: http_requests_total
    columns:
      job: job
      status: status
  no_value_column:
    metric: some_metric
    value_column: reading
    columns:
      env: environment
`

func TestParse(t *testing.T) {
	cat, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	t.Run("defaults value column to value", func(t *testing.T) {
		tbl, err := cat.Table("http_requests")
		if err != nil {
			t.Fatalf("Table: %v", err)
		}
		if tbl.Metric != "http_requests_total" {
			t.Errorf("Metric = %q, want http_requests_total", tbl.Metric)
		}
		if tbl.ValueColumn != "value" {
			t.Errorf("ValueColumn = %q, want %q", tbl.ValueColumn, "value")
		}
	})

	t.Run("respects explicit value column", func(t *testing.T) {
		tbl, err := cat.Table("no_value_column")
		if err != nil {
			t.Fatalf("Table: %v", err)
		}
		if tbl.ValueColumn != "reading" {
			t.Errorf("ValueColumn = %q, want %q", tbl.ValueColumn, "reading")
		}
	})

	t.Run("unknown table", func(t *testing.T) {
		if _, err := cat.Table("nope"); err == nil {
			t.Fatal("Table(\"nope\") succeeded, want error")
		}
	})

	t.Run("label lookup", func(t *testing.T) {
		tbl, err := cat.Table("http_requests")
		if err != nil {
			t.Fatalf("Table: %v", err)
		}
		label, err := tbl.Label("job")
		if err != nil {
			t.Fatalf("Label: %v", err)
		}
		if label != "job" {
			t.Errorf("Label(job) = %q, want job", label)
		}
		if _, err := tbl.Label("nope"); err == nil {
			t.Fatal("Label(\"nope\") succeeded, want error")
		}
	})
}

func TestParse_MissingMetric(t *testing.T) {
	_, err := Parse([]byte(`
tables:
  broken:
    columns:
      job: job
`))
	if err == nil {
		t.Fatal("Parse succeeded, want error for missing metric name")
	}
}
