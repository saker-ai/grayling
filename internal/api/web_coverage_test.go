package api_test

import (
	"os"
	"strings"
	"testing"
)

// Every management operation must be deliberately classified when the API grows.
func TestWebManagementCoverageMatrix(t *testing.T) {
	operations, _ := specOperations(t, loadSpec(t))
	data, err := os.ReadFile("../../docs/web-management-coverage.md")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[op]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(line, " | ")
		if len(cells) != 5 {
			t.Fatalf("malformed matrix row: %q", line)
		}
		key := op{method: strings.Trim(cells[0], "| `"), path: strings.Trim(cells[1], "` ")}
		if listed[key] {
			t.Errorf("duplicate matrix operation: %s", key)
		}
		listed[key] = true
		if cells[2] != "已覆盖" && cells[2] != "仅 API" && cells[2] != "需补齐" {
			t.Errorf("unclassified operation %s: %q", key, cells[2])
		}
	}
	for key := range operations {
		if strings.HasPrefix(key.path, "/api/v1/admin/") && !listed[key] {
			t.Errorf("missing management operation: %s", key)
		}
	}
	for key := range listed {
		if _, ok := operations[key]; !ok {
			t.Errorf("matrix operation absent from OpenAPI: %s", key)
		}
	}
}
