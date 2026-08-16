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
	"strings"
	"testing"

	"github.com/go-steer/k8s-sre-agent/internal/schema"
)

// The cases below are ported one-for-one from the upstream Python project's
// tests/test_monitor_state.py. They are the fidelity contract: if the Go
// fingerprint diverges from the Python one, an eval or a monitoring diff
// scored against the other implementation silently means something different.

func finding(mut ...func(*schema.Finding)) schema.Finding {
	f := schema.Finding{
		Severity:     schema.SeverityCritical,
		Title:        "CrashLoopBackOff on api",
		Detail:       "container exits 1",
		Namespace:    "prod",
		Kind:         "Pod",
		ResourceName: "api-6b474476c4-6nqxr",
		Reason:       "CrashLoopBackOff",
	}
	for _, m := range mut {
		m(&f)
	}
	return f
}

func TestNormalizeResourceName(t *testing.T) {
	for _, tc := range []struct{ kind, name, want string }{
		// Deployment pods: strip replicaset hash + pod suffix.
		{"Pod", "nginx-deployment-6b474476c4-6nqxr", "nginx-deployment"},
		{"Pod", "coredns-5d78c9869d-vwq2t", "coredns"},
		// DaemonSet / Job / bare-ReplicaSet pods: strip the single suffix.
		{"Pod", "log-shipper-4tzvn", "log-shipper"},
		// StatefulSet ordinals are part of the identity and must survive.
		{"Pod", "web-0", "web-0"},
		{"Pod", "postgres-2", "postgres-2"},
		// A bare name is left alone.
		{"Pod", "redis", "redis"},
		// Non-pod kinds are never rewritten, even if they look suffixed.
		{"Deployment", "api-6b474476c4", "api-6b474476c4"},
		{"Node", "ip-10-0-1-23.ec2.internal", "ip-10-0-1-23.ec2.internal"},
		{"", "", ""},
	} {
		if got := NormalizeResourceName(tc.kind, tc.name); got != tc.want {
			t.Errorf("NormalizeResourceName(%q, %q) = %q, want %q", tc.kind, tc.name, got, tc.want)
		}
	}
}

// Real k8s suffixes exclude vowels, 0 and 1, so words are never eaten.
func TestNormalizePreservesNamesWithVowelsAndZeros(t *testing.T) {
	// "cache" is 5 chars but contains vowels -> not a generated suffix.
	if got := NormalizeResourceName("Pod", "redis-cache"); got != "redis-cache" {
		t.Errorf("got %q, want redis-cache", got)
	}
	// "10001" contains 0 and 1 -> not a generated suffix.
	if got := NormalizeResourceName("Pod", "shard-10001"); got != "shard-10001" {
		t.Errorf("got %q, want shard-10001", got)
	}
}

// The whole reason identity fields exist: titles drift, identity must not.
func TestFingerprintStableWhenModelRewordsTitle(t *testing.T) {
	a := finding(func(f *schema.Finding) { f.Title = "CrashLoopBackOff on api-6b474476c4-6nqxr" })
	b := finding(func(f *schema.Finding) {
		f.Title = "api is crash looping (restarts=240)"
		f.Detail = "different words"
	})
	if Fingerprint(a) != Fingerprint(b) {
		t.Errorf("titles drifted the fingerprint: %q vs %q", Fingerprint(a), Fingerprint(b))
	}
}

// A restarted pod gets a new random name but is the same incident.
func TestFingerprintStableAcrossPodRecreation(t *testing.T) {
	before := finding(func(f *schema.Finding) { f.ResourceName = "api-6b474476c4-6nqxr" })
	after := finding(func(f *schema.Finding) { f.ResourceName = "api-6b474476c4-9tzvn" })
	if Fingerprint(before) != Fingerprint(after) {
		t.Errorf("pod recreation changed identity: %q vs %q", Fingerprint(before), Fingerprint(after))
	}
}

func TestFingerprintDistinguishesObjectsAndReasons(t *testing.T) {
	base := Fingerprint(finding())
	for name, other := range map[string]schema.Finding{
		"resource": finding(func(f *schema.Finding) { f.ResourceName = "worker-5d78c9869d-vwq2t" }),
		"reason":   finding(func(f *schema.Finding) { f.Reason = "OOMKilled" }),
		"ns":       finding(func(f *schema.Finding) { f.Namespace = "staging" }),
	} {
		if Fingerprint(other) == base {
			t.Errorf("%s: distinct finding collided with base fingerprint %q", name, base)
		}
	}
}

// Degrades rather than failing when the model omits every identity field.
func TestFingerprintFallsBackToTitle(t *testing.T) {
	bare := schema.Finding{
		Severity: schema.SeverityInfo,
		Title:    "Cluster has no NetworkPolicies",
		Detail:   "d",
	}
	fp := Fingerprint(bare)
	if !strings.HasSuffix(fp, "cluster-has-no-networkpolicies") {
		t.Errorf("fingerprint %q does not end with the title slug", fp)
	}
	if Fingerprint(bare) != fp {
		t.Error("fingerprint is not deterministic")
	}
}

// Severity change must be an escalation of one finding, not a new one.
func TestFingerprintIgnoresSeverity(t *testing.T) {
	warn := finding(func(f *schema.Finding) { f.Severity = schema.SeverityWarning })
	crit := finding(func(f *schema.Finding) { f.Severity = schema.SeverityCritical })
	if Fingerprint(warn) != Fingerprint(crit) {
		t.Error("severity change created a new identity")
	}
}
