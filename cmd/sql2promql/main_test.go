package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func writeTestCatalog(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/catalog.yaml"
	content := `
tables:
  http_requests:
    metric: http_requests_total
    columns:
      job: job
      status: status
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing test catalog: %v", err)
	}
	return path
}

func TestRun_SQLFlag(t *testing.T) {
	catalogPath := writeTestCatalog(t)
	var stdout, stderr bytes.Buffer

	code := run([]string{
		"-catalog", catalogPath,
		"-sql", `SELECT sum(value) FROM http_requests WHERE job = 'api'`,
	}, strings.NewReader(""), &stdout, &stderr)

	if code != 0 {
		t.Fatalf("run() = %d, stderr = %q", code, stderr.String())
	}
	want := `sum(http_requests_total{job="api"})` + "\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRun_SQLFromStdin(t *testing.T) {
	catalogPath := writeTestCatalog(t)
	var stdout, stderr bytes.Buffer

	code := run([]string{"-catalog", catalogPath},
		strings.NewReader(`SELECT value FROM http_requests WHERE status = '500'`),
		&stdout, &stderr)

	if code != 0 {
		t.Fatalf("run() = %d, stderr = %q", code, stderr.String())
	}
	want := `http_requests_total{status="500"}` + "\n"
	if stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRun_MissingCatalogFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-sql", "SELECT value FROM x"}, strings.NewReader(""), &stdout, &stderr)
	if code == 0 {
		t.Fatal("run() = 0, want non-zero exit code")
	}
	if !strings.Contains(stderr.String(), "-catalog is required") {
		t.Errorf("stderr = %q, want it to mention -catalog is required", stderr.String())
	}
}

func TestRun_UnsupportedSQL(t *testing.T) {
	catalogPath := writeTestCatalog(t)
	var stdout, stderr bytes.Buffer

	code := run([]string{
		"-catalog", catalogPath,
		"-sql", `SELECT sum(value) FROM http_requests a JOIN http_requests b ON a.job = b.job`,
	}, strings.NewReader(""), &stdout, &stderr)

	if code == 0 {
		t.Fatal("run() = 0, want non-zero exit code for unsupported SQL")
	}
	if !strings.Contains(stderr.String(), "joins are not supported") {
		t.Errorf("stderr = %q, want it to mention unsupported joins", stderr.String())
	}
}
