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

package faults

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/k8s-sre-agent/internal/kindcluster"
	"github.com/go-steer/k8s-sre-agent/internal/schema"
)

// EnvLiveCluster gates the tests that build a real kind cluster. Off by
// default so `go test ./...` stays hermetic and fast.
const EnvLiveCluster = "SRE_LIVE_CLUSTER"

// Every fixture must actually produce its fault. This is the test that keeps
// the live tier honest: a fixture whose Settle conditions never hold would
// otherwise present the agent with a healthy namespace and score it wrong for
// saying so — a harness bug that reads exactly like an agent failure.
func TestFixturesInjectTheirFaults(t *testing.T) {
	if os.Getenv(EnvLiveCluster) != "1" {
		t.Skipf("set %s=1 to run against a throwaway kind cluster", EnvLiveCluster)
	}
	ctx := context.Background()

	cluster, err := kindcluster.Create(ctx, kindcluster.Config{
		Name: kindcluster.NamePrefix + "fixtures",
	})
	if err != nil {
		t.Fatalf("create cluster: %v", err)
	}
	t.Cleanup(func() {
		if err := cluster.Delete(context.Background()); err != nil {
			t.Errorf("delete cluster: %v", err)
		}
	})
	if err := cluster.LoadImage(ctx, BaseImage); err != nil {
		t.Fatalf("load %s: %v", BaseImage, err)
	}

	// Injected concurrently: the faults are in separate namespaces and the
	// slow part is waiting for backoff timers, which run in parallel.
	all := All()
	var wg sync.WaitGroup
	errs := make([]error, len(all))
	for i, f := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Matches cmd/sre-eval-live's -settle default. A test budget
			// tighter than the harness's turns a slow fixture into a red test
			// on a suite that would have run it fine, and — worse — a fixture
			// that settles at 4m01s looks broken here and healthy there. The
			// crash-loop fixtures need ~170s of kubelet backoff before
			// CrashLoopBackOff is observable at all; see crashLoopSettle.
			errs[i] = f.Inject(ctx, cluster, 5*time.Minute)
		}()
	}
	wg.Wait()

	for i, f := range all {
		if errs[i] != nil {
			t.Errorf("%s did not manifest: %v", f.Name, errs[i])
			continue
		}
		t.Logf("%s injected", f.Name)
	}
}

// The prompt must not contain the answer. Tier 1 already measures narration of
// a diagnosis supplied in the prompt; if these prompts leak the symptom, tier 2
// measures the same thing again and the extra infrastructure buys nothing.
func TestPromptsDoNotLeakTheDiagnosis(t *testing.T) {
	leaks := []string{
		"crashloop", "crash loop", "oom", "out of memory", "imagepull",
		"image pull", "pending", "unschedulable", "failed", "selector",
		"restart", "backoff", "broken", "error",
	}
	for _, f := range All() {
		lower := strings.ToLower(f.Prompt)
		for _, leak := range leaks {
			// The namespace name is part of the scope, and every fixture's
			// namespace is named after its fault. Strip it before scanning, or
			// every prompt trivially "leaks".
			scrubbed := strings.ReplaceAll(lower, strings.ToLower(f.Namespace()), "")
			if strings.Contains(scrubbed, leak) {
				t.Errorf("%s prompt leaks %q: %q", f.Name, leak, f.Prompt)
			}
		}
	}
}

func TestFixtureSetIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	var faulty int
	for _, f := range All() {
		if seen[f.Name] {
			t.Errorf("duplicate fixture name %q", f.Name)
		}
		seen[f.Name] = true

		if strings.TrimSpace(f.Manifest) == "" {
			t.Errorf("%s: empty manifest", f.Name)
		}
		if len(f.Settle) == 0 {
			t.Errorf("%s: no settle conditions — the agent could look before the fault exists", f.Name)
		}
		if !strings.Contains(f.Manifest, "namespace: "+f.Namespace()) {
			t.Errorf("%s: manifest does not pin every object to the fixture namespace", f.Name)
		}
		if len(f.Want) > 0 {
			faulty++
			if f.WantSeverity == schema.OverallOK {
				t.Errorf("%s: expects findings but an overall severity of ok", f.Name)
			}
		} else if f.WantSeverity != schema.OverallOK {
			t.Errorf("%s: expects no findings but a severity of %q", f.Name, f.WantSeverity)
		}
	}
	// Precision needs something to be wrong about. One healthy fixture in
	// seven is the floor; zero would make "report everything" a winning
	// strategy, and the recall numbers would stop meaning anything.
	if healthy := len(All()) - faulty; healthy == 0 {
		t.Error("no healthy fixture — recall would be gameable by reporting every failure mode everywhere")
	}
}

func TestWantMatching(t *testing.T) {
	w := Want{
		Kind:            "Pod",
		Name:            "checkout-api",
		AlsoAcceptKinds: []string{"Deployment"},
		Reasons:         []string{"ImagePullBackOff", "ErrImagePull"},
		MinSeverity:     schema.SeverityWarning,
	}

	for _, tc := range []struct {
		name string
		f    schema.Finding
		want bool
	}{
		{"exact", schema.Finding{Kind: "Pod", ResourceName: "checkout-api", Reason: "ImagePullBackOff", Severity: schema.SeverityCritical}, true},
		{"generated pod suffix", schema.Finding{Kind: "Pod", ResourceName: "checkout-api-7d9f4b6c8-x2k9", Reason: "ErrImagePull", Severity: schema.SeverityWarning}, true},
		{"controller instead of pod", schema.Finding{Kind: "Deployment", ResourceName: "checkout-api", Reason: "ImagePullBackOff", Severity: schema.SeverityWarning}, true},
		{"lowercase kind", schema.Finding{Kind: "pod", ResourceName: "checkout-api", Reason: "imagepullbackoff", Severity: schema.SeverityWarning}, true},
		{"snake reason", schema.Finding{Kind: "Pod", ResourceName: "checkout-api", Reason: "image_pull_back_off", Severity: schema.SeverityWarning}, true},

		{"wrong workload", schema.Finding{Kind: "Pod", ResourceName: "payments-worker", Reason: "ImagePullBackOff", Severity: schema.SeverityCritical}, false},
		{"wrong failure mode", schema.Finding{Kind: "Pod", ResourceName: "checkout-api", Reason: "CrashLoopBackOff", Severity: schema.SeverityCritical}, false},
		{"unrelated kind", schema.Finding{Kind: "Node", ResourceName: "checkout-api", Reason: "ImagePullBackOff", Severity: schema.SeverityCritical}, false},
		{"too mild", schema.Finding{Kind: "Pod", ResourceName: "checkout-api", Reason: "ImagePullBackOff", Severity: schema.SeverityInfo}, false},
		{"no resource named", schema.Finding{Kind: "Pod", Reason: "ImagePullBackOff", Severity: schema.SeverityCritical}, false},

		// A prefix match must not run the other way. "checkout" is a
		// different, shorter name, and crediting it would let an agent score
		// by naming the namespace's common prefix instead of the workload.
		{"prefix in reverse", schema.Finding{Kind: "Pod", ResourceName: "checkout", Reason: "ImagePullBackOff", Severity: schema.SeverityCritical}, false},
		// A sibling that merely shares a prefix is a different object.
		{"sibling workload", schema.Finding{Kind: "Pod", ResourceName: "checkout-apiserver", Reason: "ImagePullBackOff", Severity: schema.SeverityCritical}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := w.Matches(tc.f); got != tc.want {
				t.Errorf("Matches(%+v) = %v, want %v", tc.f, got, tc.want)
			}
		})
	}
}

// A Want with no Reasons grades Kind and Name only. Without this, adding a
// fixture and forgetting the reason list would silently accept any reason.
func TestWantWithNoReasonsIgnoresReason(t *testing.T) {
	w := Want{Kind: "Job", Name: "nightly-report"}
	if !w.Matches(schema.Finding{Kind: "Job", ResourceName: "nightly-report", Reason: "anything", Severity: schema.SeverityInfo}) {
		t.Error("an ungraded reason should not block a match")
	}
	if w.Matches(schema.Finding{Kind: "Job", ResourceName: "other-job", Reason: "anything", Severity: schema.SeverityInfo}) {
		t.Error("name is still graded when reason is not")
	}
}

// fault-invoicing is the one fixture whose correct reason has to be invented,
// because Kubernetes writes none for "the container is up and reaching
// nothing". That makes its accepted-reason list the only thing standing between
// the fixture and an agent that scores it without reading a log, and
// Want.MatchesReason makes the list wider than it reads: the match is
// bidirectional, so a short token the agent writes is accepted whenever some
// longer token here contains it.
//
// The first draft got this wrong in both of the ways that matter. It listed
// "DependencyUnavailable", which admitted a bare "Unavailable" — and
// Available=False is a real Deployment condition, so an agent could assert it
// from status alone, be wrong about a 2/2 Deployment, and still score recall
// 1.000. It listed "ApplicationErrors", which admitted a bare "Error", a token
// models write constantly and the reason evals.genericReasons exists.
//
// So this test runs the vocabulary recorded runs actually produced past the
// fixture's Want. The rejected corpus is what an agent writes about a workload
// it has only looked at the status of; the accepted corpus is the spellings of
// the real answer. The third group is the residue — bare substrings still
// admitted, listed explicitly so that "harmless" is a judgement on the record
// rather than an oversight.
func TestSilentFailureRejectsStatusOnlyReasons(t *testing.T) {
	var w Want
	for _, f := range All() {
		if f.Name == "fault-invoicing" {
			if len(f.Want) != 1 {
				t.Fatalf("fault-invoicing has %d wants; this test assumes one", len(f.Want))
			}
			w = f.Want[0]
		}
	}
	if w.Kind == "" {
		t.Fatal("fault-invoicing is gone from the suite; delete this test or repoint it")
	}

	// Everything here is observable — or assertable — without reading a log.
	// Kubernetes' own status vocabulary first, then the advisories this agent
	// has been recorded writing about healthy workloads, then the other
	// families' tokens, which are the misdiagnoses this namespace invites.
	statusOnly := []string{
		"Available", "Unavailable", "Progressing", "Ready", "NotReady", "Running",
		"ReplicaFailure", "ProgressDeadlineExceeded", "Error", "Errors", "Failed",
		"Pending", "Healthy",
		"MissingProbes", "MissingPDB", "NoPodDisruptionBudget", "SingleReplica",
		"SingleReplicaNoPDB", "MissingResourceLimits", "LatestImageTag",
		"NoServiceDefined", "RolloutIncomplete", "ExcessiveRestarts", "SuspectImage",
		"PodsNotReady", "SlowWebhookRisk",
		"CrashLoopBackOff", "OOMKilled", "ImagePullBackOff", "NoEndpoints",
		"ServiceHasNoEndpoints", "Unschedulable",
	}
	for _, r := range statusOnly {
		if w.MatchesReason(r) {
			t.Errorf("fault-invoicing accepts %q, which an agent can write without reading "+
				"a log — the fixture would score full recall for missing its only fault", r)
		}
	}

	// The answer, in the spellings the agent plausibly reaches for. Every one
	// of these requires having read what the container is writing.
	answers := []string{
		"ConnectionRefused", "connection_refused", "connection-refused",
		"ConnectionFailure", "Unreachable", "DatabaseUnreachable",
		"UpstreamUnreachable", "BackendUnreachable", "DependencyUnreachable",
		"DependencyFailure", "DependencyMissing", "DataDependencyMissing",
	}
	for _, r := range answers {
		if !w.MatchesReason(r) {
			t.Errorf("fault-invoicing rejects %q, which is the right diagnosis in a "+
				"spelling the agent could reasonably choose", r)
		}
	}

	// The residue: bare substrings of the five accepted compounds, admitted by
	// the bidirectional match and judged harmless because none of them is a
	// token any recorded run has produced on its own, and none is assertable
	// from a field on this workload. If a future run writes one of these bare,
	// move it to statusOnly and re-pick the compound that admits it.
	admitted := []string{"Connection", "Refused", "Failure", "Dependency", "Missing"}
	for _, r := range admitted {
		if !w.MatchesReason(r) {
			t.Errorf("%q is no longer admitted — the accepted list was narrowed and this "+
				"note is stale; delete it from the residue rather than leaving it asserted", r)
		}
	}
}
