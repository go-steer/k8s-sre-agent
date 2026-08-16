package evals

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// TestHarnessCeiling scores the ground truth against itself: a hypothetical
// agent that reproduces the reference answer verbatim and calls exactly the
// expected tools.
//
// Every deterministic evaluator must return 1.0 here. That is what makes a
// real score interpretable — without a known ceiling, "0.72" could mean a
// mediocre agent or a miscalibrated evaluator, and upstream shipped both a
// floor (constant 0) and a ceiling (constant 1.0) that measured neither.
func TestHarnessCeiling(t *testing.T) {
	examples := load(t)
	evaluators := DefaultEvaluators()

	all := make([][]Score, 0, len(examples))
	for _, ex := range examples {
		perfect := Run{
			Response:   ex.Outputs.ExpectedResponse,
			Trajectory: ex.Outputs.ExpectedTools,
		}
		scores := make([]Score, 0, len(evaluators))
		for _, e := range evaluators {
			scores = append(scores, e.Score(ex, perfect))
		}
		all = append(all, scores)
	}

	agg := Summarize(all)

	names := make([]string, 0, len(agg.Means))
	for n := range agg.Means {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		line := fmt.Sprintf("%-20s mean=%.3f scored=%d skipped=%d",
			n, agg.Means[n], agg.Counts[n], agg.Skipped[n])
		if agg.Means[n] < 1.0 {
			t.Errorf("ceiling below 1.0 — evaluator is miscalibrated: %s", line)
		} else {
			t.Log(line)
		}
	}
}

// TestFloorIsNotZeroByConstruction guards the opposite failure: an evaluator
// that scores 1.0 for everything measures nothing. A deliberately bad agent
// must score below the ceiling on every evaluator.
func TestFloorIsWellBelowCeiling(t *testing.T) {
	examples := load(t)

	all := make([][]Score, 0, len(examples))
	for _, ex := range examples {
		useless := Run{
			Response:   "Something might be wrong with the cluster. Please investigate.",
			Trajectory: []string{"kubectl_get_namespaces"},
		}
		var scores []Score
		for _, e := range DefaultEvaluators() {
			scores = append(scores, e.Score(ex, useless))
		}
		all = append(all, scores)
	}

	agg := Summarize(all)
	for name, mean := range agg.Means {
		if mean > 0.25 {
			t.Errorf("%s scored %.3f for a useless agent — evaluator is not discriminating", name, mean)
		} else {
			t.Logf("%-20s useless-agent mean=%.3f", name, mean)
		}
	}
}

// TestGroundTruthSeverityDependsOnUnstatedContext documents a ceiling on
// severity_accuracy that no agent can reach by reasoning from the scenario.
//
// Examples 4 and 5 are the same fault shape: a workload Pending at zero ready
// replicas, blocked on a resource it cannot get. Example 4 has been down four
// times longer. Example 4 is labelled WARNING and example 5 CRITICAL. The only
// discriminator is what the workload is — a database matters more than an ETL
// job — and neither scenario says so.
//
// This is not a defect to repair, the way the three upstream evaluator bugs
// are: it is a real property of SRE severity, which genuinely depends on
// business context. But it means severity_accuracy is bounded below 1.0 in
// practice, so a score short of the ceiling is not by itself evidence the
// agent is miscalibrated. The test pins the pair so that this stays a known
// property rather than a rediscovered surprise, and fails if the dataset is
// ever edited out from under the claim.
func TestGroundTruthSeverityDependsOnUnstatedContext(t *testing.T) {
	examples := load(t)
	if len(examples) < 6 {
		t.Skip("dataset too small")
	}

	pending, database := examples[4], examples[5]
	for _, ex := range []Example{pending, database} {
		if !strings.Contains(ex.Inputs.Scenario, "Pending") {
			t.Fatalf("dataset changed: expected a Pending scenario, got %.60s", ex.Inputs.Scenario)
		}
	}

	got, _ := ExtractSeverity(pending.Outputs.ExpectedResponse, nil)
	want, _ := ExtractSeverity(database.Outputs.ExpectedResponse, nil)
	if got == want {
		t.Fatalf("dataset changed: the two Pending scenarios now agree (%s) — "+
			"the unstated-context ceiling may no longer apply", got)
	}
	t.Logf("same fault shape, different labels: ex-04=%s ex-05=%s", got, want)
}

// TestSubsumptionDoesNotMakeOneToolSufficient bounds the cost of the
// one-to-many mapping in alias.go.
//
// k8s_triage_workload answers five kubectl intents by design, so crediting it
// for all five necessarily raises the score of an agent that calls nothing
// else. This test measures exactly how much: a reflexive agent that opens
// every incident with that one check and then stops. Its score is not zero
// and should not be — it really does answer the common intents — but if it
// approaches the ceiling, tool_coverage has stopped measuring judgment and the
// mapping needs narrowing.
func TestSubsumptionDoesNotMakeOneToolSufficient(t *testing.T) {
	examples := load(t)

	var sum float64
	var n, perfect int
	for _, ex := range examples {
		s := ToolCoverage{}.Score(ex, Run{Trajectory: []string{"k8s_triage_workload"}})
		if s.Skipped {
			continue
		}
		sum += s.Value
		n++
		if s.Value == 1.0 {
			perfect++
		}
	}
	if n == 0 {
		t.Fatal("no scorable examples")
	}
	mean := sum / float64(n)
	t.Logf("one-tool agent: tool_coverage mean=%.3f over %d examples, %d scored a full 1.0", mean, n, perfect)

	// The gap between this and the 1.0 ceiling is the evaluator's remaining
	// discriminating power. Half is the point at which a single reflexive call
	// would be worth as much as a real investigation.
	if mean > 0.5 {
		t.Errorf("one-tool agent scores %.3f — subsumption is too generous to measure tool judgment", mean)
	}
}

// TestABlindFanOutStillDoesNotScoreLikeAnInvestigation bounds the same cost
// across several checks at once, which is where it actually bites.
//
// Every entry in `satisfies` passes the one-tool bound on its own and the
// credit still compounds: these three are the broad discovery calls an agent
// can issue against any namespace without having read a single result, so
// their combined score is what a trajectory of pure reflex is worth. Adding
// k8s_list_resources moved it, which is how the two describe-collapsed intents
// came out of that entry — with kubectl_get_services and kubectl_get_ingress
// in, this measured 0.842 against a real agent's 0.881.
//
// The bound is the reason the number is worth watching rather than the number
// itself. tool_coverage measures judgment only while a real investigation
// scores meaningfully above a blind one.
func TestABlindFanOutStillDoesNotScoreLikeAnInvestigation(t *testing.T) {
	examples := load(t)
	blind := []string{"k8s_triage_workload", "k8s_triage_delta", "k8s_list_resources"}

	var sum float64
	var n int
	for _, ex := range examples {
		s := ToolCoverage{}.Score(ex, Run{Trajectory: blind})
		if s.Skipped {
			continue
		}
		sum += s.Value
		n++
	}
	if n == 0 {
		t.Fatal("no scorable examples")
	}
	mean := sum / float64(n)
	t.Logf("blind fan-out (%s): tool_coverage mean=%.3f over %d examples",
		strings.Join(blind, "+"), mean, n)

	// Measured 0.702 when this was written; the published agent scores 0.881.
	// Past 0.75 the two are inside the run-to-run spread of each other and the
	// evaluator has stopped separating them.
	if mean > 0.75 {
		t.Errorf("a blind three-call fan-out scores %.3f — subsumption now credits "+
			"reflex at the same rate as investigation", mean)
	}
}
