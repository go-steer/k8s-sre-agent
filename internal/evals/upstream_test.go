package evals

import (
	"regexp"
	"testing"
)

const datasetPath = "../../evals/dataset/upstream-scenarios.jsonl"

func load(t *testing.T) []Example {
	t.Helper()
	ex, err := LoadJSONL(datasetPath)
	if err != nil {
		t.Fatalf("load dataset: %v", err)
	}
	if len(ex) != 31 {
		t.Fatalf("expected 31 upstream examples, got %d", len(ex))
	}
	return ex
}

// --- Proof that the upstream evaluators are defective -----------------------
//
// These tests reimplement the upstream logic exactly and assert it produces a
// constant. They exist so the claim "we replaced their evaluators" is backed
// by an executable demonstration rather than a code review, and so the claim
// stays true if someone re-syncs the dataset.

// upstreamSeverityPattern is verbatim from evals/evaluators.py.
var upstreamSeverityPattern = regexp.MustCompile(`(?i)\[(CRITICAL|WARNING|INFO|OK)\]`)

// Bug 2: severity_accuracy scores 0 on every example, because the ground
// truth writes "CRITICAL: ..." and the regex demands "[CRITICAL]".
func TestUpstreamSeverityEvaluatorScoresConstantZero(t *testing.T) {
	for i, ex := range load(t) {
		if upstreamSeverityPattern.MatchString(ex.Outputs.ExpectedResponse) {
			t.Fatalf("example %d unexpectedly contains a bracketed severity; "+
				"the upstream bug may have been fixed upstream", i)
		}
	}
	t.Log("confirmed: 0/31 expected_response values contain [SEVERITY]; " +
		"upstream _extract_severity returns None for every example, scoring a constant 0")
}

// Bug 1: tool_coverage reads a key the dataset never writes, so it always
// takes the "no expected trajectory" branch and returns a constant 1.0.
func TestUpstreamToolCoverageReadsAKeyThatDoesNotExist(t *testing.T) {
	raw, err := LoadJSONL(datasetPath)
	if err != nil {
		t.Fatal(err)
	}
	// The Example struct only decodes documented keys, so re-check the raw
	// bytes for the key upstream actually reads.
	blob, err := readFile(datasetPath)
	if err != nil {
		t.Fatal(err)
	}
	if containsKey(blob, "expected_trajectory") {
		t.Fatal("dataset now contains expected_trajectory; upstream bug 1 may be fixed")
	}
	withTools := 0
	for _, ex := range raw {
		if len(ex.Outputs.ExpectedTools) > 0 {
			withTools++
		}
	}
	t.Logf("confirmed: 0 rows carry 'expected_trajectory' while %d/%d carry "+
		"'expected_tools'; upstream tool_coverage scores a constant 1.0", withTools, len(raw))
}

// Bug 3: expected_tools names tools absent from the agent's registry.
func TestDatasetReferencesNonexistentTools(t *testing.T) {
	// Keys of datasetRepair were each verified absent from the upstream
	// tools/__init__.py registry; this asserts the dataset still needs them.
	needed := map[string]int{}
	for _, ex := range load(t) {
		for _, tool := range ex.Outputs.ExpectedTools {
			if _, broken := datasetRepair[tool]; broken {
				needed[tool]++
			}
			if unreachableTools[tool] {
				needed[tool]++
			}
		}
	}
	if len(needed) == 0 {
		t.Fatal("no nonexistent tools found; the dataset may have been repaired upstream")
	}
	t.Logf("confirmed: %d distinct nonexistent expected_tools still present: %v", len(needed), needed)
}

// --- Validation that the repaired evaluators actually work ------------------

// The load-bearing sanity check. Feed each example's own ground truth back as
// the agent's answer. A correct severity evaluator must score 1.0 across the
// board — if it cannot grade the ground truth against itself, it is as broken
// as the one it replaced.
func TestFixedSeverityEvaluatorScoresGroundTruthPerfectly(t *testing.T) {
	examples := load(t)
	var scored, sum int
	for i, ex := range examples {
		s := SeverityAccuracy{}.Score(ex, Run{Response: ex.Outputs.ExpectedResponse})
		if s.Skipped {
			t.Errorf("example %d: ground truth was ungradeable: %s", i, s.Comment)
			continue
		}
		scored++
		if s.Value == 1 {
			sum++
		} else {
			t.Errorf("example %d: self-score %.0f (%s)\n  expected_response: %.90s",
				i, s.Value, s.Comment, ex.Outputs.ExpectedResponse)
		}
	}
	if scored != len(examples) {
		t.Errorf("only %d/%d examples were gradeable", scored, len(examples))
	}
	t.Logf("fixed severity_accuracy self-scores %d/%d = %.2f (upstream: 0.00)",
		sum, scored, float64(sum)/float64(scored))
}

// The repaired tool_coverage must award full credit when the agent calls
// exactly the expected tools, including via alias.
func TestFixedToolCoverageCreditsAliasedTools(t *testing.T) {
	ex := Example{}
	ex.Inputs.Scenario = "x"
	// A row using two names that do not exist in the Python registry.
	ex.Outputs.ExpectedTools = []string{"kubectl_describe_node", "kubectl_get_pvcs"}

	// An agent calling the real Python tools gets full credit.
	py := ToolCoverage{}.Score(ex, Run{Trajectory: []string{"kubectl_get_nodes", "kubectl_get_pvc"}})
	if py.Value != 1 {
		t.Errorf("python-equivalent trajectory scored %.2f, want 1.00 (%s)", py.Value, py.Comment)
	}

	// An agent calling lookout's checks gets credit for the same intent.
	ex.Outputs.ExpectedTools = []string{"kubectl_describe_pod", "get_cluster_summary"}
	golang := ToolCoverage{}.Score(ex, Run{Trajectory: []string{"k8s_resource_spec", "k8s_cluster_health"}})
	if golang.Value != 1 {
		t.Errorf("lookout trajectory scored %.2f, want 1.00 (%s)", golang.Value, golang.Comment)
	}
}

// k8s_triage_status reads and writes a triage *record* — a diagnosis, an
// action taken, a severity judgment. It reads no cluster state, so it must
// satisfy no kubectl read intent. An earlier version of the alias table had it
// standing in for kubectl_describe_pod, which handed an agent describe credit
// for filing a status note.
func TestTriageStatusIsNotAReadOfClusterState(t *testing.T) {
	ex := Example{}
	ex.Outputs.ExpectedTools = []string{"kubectl_describe_pod"}
	s := ToolCoverage{}.Score(ex, Run{Trajectory: []string{"k8s_triage_status"}})
	if s.Value != 0 {
		t.Errorf("k8s_triage_status scored %.2f for a describe intent, want 0.00 (%s)", s.Value, s.Comment)
	}
}

// An example whose expected tools are all unreachable must be skipped, not
// scored 0 — otherwise we penalize an agent for a dataset defect.
func TestUnreachableToolsAreSkippedNotFailed(t *testing.T) {
	ex := Example{}
	ex.Outputs.ExpectedTools = []string{"kubectl_get_secrets"}
	s := ToolCoverage{}.Score(ex, Run{Trajectory: []string{"kubectl_get_pods"}})
	if !s.Skipped {
		t.Errorf("all-unreachable example scored %.2f instead of being skipped", s.Value)
	}
}

// A vague answer must lose points a specific one keeps.
func TestResourceGroundingDiscriminates(t *testing.T) {
	ex := load(t)[0] // pod 'api-server-7d8f9c-xkp2v' in namespace 'production'

	specific := ResourceGrounding{}.Score(ex, Run{Response: ex.Outputs.ExpectedResponse})
	vague := ResourceGrounding{}.Score(ex, Run{Response: "A pod is crash looping. Check the logs."})

	if specific.Value <= vague.Value {
		t.Errorf("grounding failed to discriminate: specific=%.2f vague=%.2f", specific.Value, vague.Value)
	}
	if vague.Value != 0 {
		t.Errorf("vague answer scored %.2f, want 0 (%s)", vague.Value, vague.Comment)
	}
	t.Logf("specific=%.2f (%s) vs vague=%.2f", specific.Value, specific.Comment, vague.Value)
}
