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
	"errors"
	"os"
	"testing"

	"github.com/go-steer/k8s-sre-agent/internal/schema"
)

//go:generate python3 ../../dev/diffcheck.py

// TestGoldenAgainstPython is the differential fidelity check: every vector in
// testdata/golden.json was produced by executing upstream's Python
// monitor_state.py, so a mismatch here means this implementation and that one
// disagree about finding identity. That disagreement would be invisible at
// runtime and would corrupt both the monitoring diff and any eval scored
// across the two.
//
// The vectors are tracked, so this runs on a fresh clone and in CI — a change
// to Fingerprint or NormalizeResourceName cannot land without it firing.
// Regenerate with `go generate ./internal/monitor` after deliberately changing
// either (see dev/diffcheck.py, which fetches the pinned commit itself and
// needs no checkout).
//
// They were briefly untracked, in the window where upstream published no
// licence and output produced by running their code was the one artifact here
// genuinely derived from that project. langchain-samples/sre-agent#15 added
// MIT, so the vectors ship again and carry their provenance and terms in the
// file's "source" block. The t.Log below names them: evidence of matching
// upstream is worth nothing without saying which upstream.
func TestGoldenAgainstPython(t *testing.T) {
	raw, err := os.ReadFile("testdata/golden.json")
	if errors.Is(err, os.ErrNotExist) {
		t.Fatal("testdata/golden.json is missing and is a tracked file — restore it, " +
			"or regenerate with `go generate ./internal/monitor` (see dev/diffcheck.py)")
	}
	if err != nil {
		t.Fatalf("read golden vectors: %v", err)
	}
	var golden struct {
		Source struct {
			Repo   string `json:"repo"`
			Commit string `json:"commit"`
			Blob   string `json:"monitor_state_blob"`
		} `json:"source"`
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
	t.Logf("checked %d normalize + %d fingerprint vectors against %s@%s (monitor_state.py blob %s)",
		len(golden.Norm), len(golden.FP), golden.Source.Repo, golden.Source.Commit, golden.Source.Blob)
}
