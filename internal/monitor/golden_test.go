package monitor

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/go-steer/core-sre-agent/internal/schema"
)

// TestGoldenAgainstPython is the differential fidelity check: every vector in
// testdata/golden.json was produced by executing the upstream Python
// monitor_state.py, so a mismatch here means the Go port and the Python
// original disagree about finding identity. That disagreement would be
// invisible at runtime and would corrupt both the monitoring diff and any
// eval scored across the two implementations.
//
// Regenerate with: go generate ./internal/monitor  (see dev/diffcheck.py)
func TestGoldenAgainstPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var golden struct {
		Norm []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
			Norm string `json:"norm"`
		} `json:"norm"`
		FP []struct {
			Namespace    string `json:"namespace"`
			Kind         string `json:"kind"`
			ResourceName string `json:"resource_name"`
			Reason       string `json:"reason"`
			Title        string `json:"title"`
			FP           string `json:"fp"`
		} `json:"fp"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("parse golden vectors: %v", err)
	}
	if len(golden.Norm) == 0 || len(golden.FP) == 0 {
		t.Fatal("golden vectors are empty")
	}

	var bad int
	for _, tc := range golden.Norm {
		if got := NormalizeResourceName(tc.Kind, tc.Name); got != tc.Norm {
			bad++
			t.Errorf("NormalizeResourceName(%q, %q) = %q, python = %q", tc.Kind, tc.Name, got, tc.Norm)
		}
	}
	for _, tc := range golden.FP {
		f := schema.Finding{
			Namespace:    tc.Namespace,
			Kind:         tc.Kind,
			ResourceName: tc.ResourceName,
			Reason:       tc.Reason,
			Title:        tc.Title,
		}
		if got := Fingerprint(f); got != tc.FP {
			bad++
			if bad < 15 {
				t.Errorf("Fingerprint(%+v) = %q, python = %q", tc, got, tc.FP)
			}
		}
	}
	t.Logf("checked %d normalize + %d fingerprint vectors against Python", len(golden.Norm), len(golden.FP))
}
