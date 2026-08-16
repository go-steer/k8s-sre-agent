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

package schema

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"google.golang.org/genai"
)

// TestReportSchemaMatchesStruct guards the hand-written genai schema against
// the Go structs it is supposed to describe.
//
// The schema is what the model fills; the struct is what we decode into. A
// field added to one and not the other is silently dropped at the boundary —
// the report parses, the field is just empty — so drift here is invisible at
// runtime and corrupts findings rather than failing them.
func TestReportSchemaMatchesStruct(t *testing.T) {
	s := ReportSchema()

	if got, want := jsonNames(HealthReport{}), keys(s.Properties); !reflect.DeepEqual(got, want) {
		t.Errorf("HealthReport fields %v != schema properties %v", got, want)
	}

	findings := s.Properties["findings"]
	if findings == nil || findings.Items == nil {
		t.Fatal("schema has no findings.items")
	}
	if got, want := jsonNames(Finding{}), keys(findings.Items.Properties); !reflect.DeepEqual(got, want) {
		t.Errorf("Finding fields %v != schema properties %v", got, want)
	}
}

// The enums must list exactly the values Valid() accepts. A model told that
// "ok" is a legal finding severity would produce reports that fail Validate().
func TestReportSchemaEnumsAreValidSeverities(t *testing.T) {
	s := ReportSchema()

	for _, v := range s.Properties["overall_severity"].Enum {
		if !OverallSeverity(v).Valid() {
			t.Errorf("overall_severity enum contains invalid value %q", v)
		}
	}
	if got := len(s.Properties["overall_severity"].Enum); got != 4 {
		t.Errorf("overall_severity enum has %d values, want 4 (critical/warning/info/ok)", got)
	}

	fs := s.Properties["findings"].Items.Properties["severity"]
	for _, v := range fs.Enum {
		if !Severity(v).Valid() {
			t.Errorf("finding severity enum contains invalid value %q", v)
		}
		if v == string(OverallOK) {
			t.Error("finding severity enum offers 'ok'; only overall_severity may be ok")
		}
	}
}

// A report the model could legally emit must round-trip into the struct.
func TestReportSchemaRoundTrips(t *testing.T) {
	raw := `{
	  "overall_severity": "critical",
	  "summary": "CRITICAL: api-server-7d8f9c-xkp2v is CrashLoopBackOff (18 restarts).",
	  "findings": [{
	    "severity": "critical",
	    "title": "api-server-7d8f9c-xkp2v is CrashLoopBackOff",
	    "detail": "Container exits 1 within seconds; DATABASE_URL is unset.",
	    "namespace": "production",
	    "kind": "Pod",
	    "resource_name": "api-server-7d8f9c-xkp2v",
	    "reason": "CrashLoopBackOff"
	  }],
	  "recommended_actions": ["Verify Secret 'api-server-secrets' contains DATABASE_URL"]
	}`
	var r HealthReport
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("a schema-conformant report failed validation: %v", err)
	}
	if r.Findings[0].Reason != "CrashLoopBackOff" {
		t.Errorf("reason = %q", r.Findings[0].Reason)
	}
}

// jsonNames lists a struct's JSON field names, sorted.
func jsonNames(v any) []string {
	t := reflect.TypeOf(v)
	out := make([]string, 0, t.NumField())
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("json")
		for j, c := range tag {
			if c == ',' {
				tag = tag[:j]
				break
			}
		}
		if tag != "" && tag != "-" {
			out = append(out, tag)
		}
	}
	sort.Strings(out)
	return out
}

func keys(m map[string]*genai.Schema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
