// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package monitor

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/go-steer/k8s-sre-agent/internal/schema"
)

//go:generate python3 ../../dev/diffcheck.py

// TestGoldenAgainstPython is the differential fidelity check: every vector in
// testdata/golden.json was produced by executing the upstream Python
// monitor_state.py, so a mismatch here means the Go port and the Python
// original disagree about finding identity. That disagreement would be
// invisible at runtime and would corrupt both the monitoring diff and any
// eval scored across the two implementations.
//
// The vectors name the upstream they came from — golden.json's "source" object
// carries the commit and the blob hash of monitor_state.py — because evidence
// that the port matches upstream is worth nothing without saying which
// upstream. Regenerate with: go generate ./internal/monitor (see
// dev/diffcheck.py, which fetches that pinned commit itself).
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
