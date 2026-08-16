package evals

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/go-steer/k8s-sre-agent/internal/schema"
)

// Score is one evaluator's verdict on one example.
type Score struct {
	Name    string  `json:"name"`
	Value   float64 `json:"value"` // normalized to [0,1]
	Comment string  `json:"comment"`
	// Skipped marks a score excluded from aggregation (e.g. an example whose
	// entire expected trajectory is unreachable by any agent).
	Skipped bool `json:"skipped,omitempty"`
}

// Evaluator grades one run against one example.
type Evaluator interface {
	Name() string
	Score(ex Example, run Run) Score
}

// severityPattern matches a severity in either the bracketed form the agent
// prompt specifies ("[CRITICAL]") or the bare prefix form the dataset ground
// truth actually uses ("CRITICAL: ..."). Upstream matched only the former,
// which is why it scored 0 on all 31 examples — this is the fix for bug 2.
//
// The bare form is anchored to a line start followed by a colon so that the
// word "critical" occurring mid-sentence ("this is critical to fix") is not
// mistaken for a severity declaration.
var severityPattern = regexp.MustCompile(`(?im)^\s*\[?(CRITICAL|WARNING|INFO|OK)\]?\s*[:\]]`)

// ExtractSeverity pulls a severity from free text, preferring a structured
// value when one is available.
func ExtractSeverity(text string, structured *StructuredResult) (schema.OverallSeverity, bool) {
	if structured != nil && structured.OverallSeverity != "" {
		return schema.ParseSeverity(structured.OverallSeverity)
	}
	m := severityPattern.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return schema.ParseSeverity(m[1])
}

// SeverityAccuracy checks whether the agent reached the same severity as the
// ground truth. Exact match, 1 or 0.
type SeverityAccuracy struct{}

func (SeverityAccuracy) Name() string { return "severity_accuracy" }

func (SeverityAccuracy) Score(ex Example, run Run) Score {
	want, wantOK := ExtractSeverity(ex.Outputs.ExpectedResponse, nil)
	got, gotOK := ExtractSeverity(run.ScoredText(), run.Report)

	if !wantOK {
		// The dataset row itself is ungradeable. Skip rather than score 0 —
		// scoring 0 is precisely the upstream bug.
		return Score{
			Name:    "severity_accuracy",
			Skipped: true,
			Comment: "no severity in expected_response; example is ungradeable",
		}
	}
	if !gotOK {
		return Score{
			Name:    "severity_accuracy",
			Value:   0,
			Comment: fmt.Sprintf("agent declared no severity; expected %s", want),
		}
	}
	v := 0.0
	if got == want {
		v = 1
	}
	return Score{
		Name:    "severity_accuracy",
		Value:   v,
		Comment: fmt.Sprintf("actual=%s expected=%s", got, want),
	}
}

// severityRank orders the severity ladder for ordinal comparison.
var severityRank = map[schema.OverallSeverity]int{
	schema.OverallOK: 0, schema.OverallInfo: 1,
	schema.OverallWarning: 2, schema.OverallCritical: 3,
}

// SeverityCalibration scores how far off the agent's severity was, rather than
// whether it was exactly right.
//
// SeverityAccuracy alone cannot tell a systematically miscalibrated agent from
// a confused one: both score 0. On the tier-1 dataset the distinction is the
// whole finding — every miss observed so far has been exactly one level too
// high, which is a prompt rubric problem, whereas an agent calling a healthy
// cluster critical would be a comprehension problem. Distance separates them.
//
// The signed direction is reported in the comment, because "too hot" and "too
// cold" call for opposite fixes and the mean of a symmetric score hides which
// one is happening.
type SeverityCalibration struct{}

func (SeverityCalibration) Name() string { return "severity_calibration" }

func (SeverityCalibration) Score(ex Example, run Run) Score {
	want, wantOK := ExtractSeverity(ex.Outputs.ExpectedResponse, nil)
	if !wantOK {
		return Score{
			Name:    "severity_calibration",
			Skipped: true,
			Comment: "no severity in expected_response; example is ungradeable",
		}
	}
	got, gotOK := ExtractSeverity(run.ScoredText(), run.Report)
	if !gotOK {
		return Score{
			Name:    "severity_calibration",
			Value:   0,
			Comment: fmt.Sprintf("agent declared no severity; expected %s", want),
		}
	}

	delta := severityRank[got] - severityRank[want]
	dist := delta
	if dist < 0 {
		dist = -dist
	}
	// Three is the widest possible gap (ok↔critical), so this maps an exact
	// match to 1.0 and the worst case to 0.0.
	v := 1 - float64(dist)/3

	dir := "exact"
	switch {
	case delta > 0:
		dir = fmt.Sprintf("%d too high", delta)
	case delta < 0:
		dir = fmt.Sprintf("%d too low", -delta)
	}
	return Score{
		Name:    "severity_calibration",
		Value:   v,
		Comment: fmt.Sprintf("actual=%s expected=%s (%s)", got, want, dir),
	}
}

// ToolCoverage is the fraction of expected tool *intents* the agent's
// trajectory answered.
//
// Fixes bug 1 (reads ExpectedTools, the key the dataset writes) and applies
// alias.go so an agent is credited for answering an intent with a differently
// named tool — including the lookout checks that deliberately answer several
// kubectl intents in one call.
type ToolCoverage struct{}

func (ToolCoverage) Name() string { return "tool_coverage" }

func (ToolCoverage) Score(ex Example, run Run) Score {
	var want []string
	var excluded []string
	for _, t := range CanonicalTools(ex.Outputs.ExpectedTools) {
		if IsUnreachable(t) {
			excluded = append(excluded, t)
			continue
		}
		want = append(want, t)
	}

	if len(want) == 0 {
		return Score{
			Name:    "tool_coverage",
			Skipped: true,
			Comment: fmt.Sprintf("no reachable expected tools (excluded %v)", excluded),
		}
	}

	got := SatisfiedIntents(run.Trajectory)

	var covered, missing []string
	for _, t := range want {
		if got[t] {
			covered = append(covered, t)
		} else {
			missing = append(missing, t)
		}
	}
	sort.Strings(missing)

	parts := []string{fmt.Sprintf("covered %d/%d", len(covered), len(want))}
	if len(missing) > 0 {
		parts = append(parts, "missing="+strings.Join(missing, ","))
	}
	if len(excluded) > 0 {
		parts = append(parts, "excluded_unreachable="+strings.Join(excluded, ","))
	}

	return Score{
		Name:    "tool_coverage",
		Value:   float64(len(covered)) / float64(len(want)),
		Comment: strings.Join(parts, ", "),
	}
}

// ReflexBaseline is the tool_coverage a trajectory would score on every
// example without looking at any of them.
//
// It exists because alias.go's subsumption mapping gives a single lookout
// check credit for several kubectl intents, so tool_coverage no longer has 0
// as its practical floor. Reporting what an agent gets for free is the
// difference between a measurement and a flattering number.
func ReflexBaseline(examples []Example, trajectory []string) (mean float64, n int) {
	var sum float64
	for _, ex := range examples {
		s := ToolCoverage{}.Score(ex, Run{Trajectory: trajectory})
		if s.Skipped {
			continue
		}
		sum += s.Value
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), n
}

// ResourceGrounding checks that the agent's answer names the specific
// Kubernetes objects the ground truth names.
//
// This has no upstream equivalent and is the cheap deterministic stand-in for
// the "specific" dimension of upstream's LLM judge. A response that says "a
// pod is crash looping" scores 0; one that says "api-server-7d8f9c-xkp2v is
// crash looping" scores 1. No model call, no API key, no flake.
type ResourceGrounding struct{}

func (ResourceGrounding) Name() string { return "resource_grounding" }

// resourceToken matches quoted identifiers in the ground truth — the dataset
// consistently quotes resource names ('api-server-7d8f9c-xkp2v', 'production').
var resourceToken = regexp.MustCompile(`'([a-z0-9][a-z0-9.\-]{2,})'`)

func (ResourceGrounding) Score(ex Example, run Run) Score {
	// The reference set is the identifiers quoted in the scenario that the
	// ground-truth answer *also* names. Requiring every quoted token would
	// demand more than a model answer provides — the dataset quotes the
	// namespace in the scenario but the reference response usually mentions
	// only the object. Calibrating against the ground truth keeps the ceiling
	// at 1.0, which is what makes the number interpretable.
	truth := strings.ToLower(ex.Outputs.ExpectedResponse)
	seen := map[string]bool{}
	var want []string
	for _, m := range resourceToken.FindAllStringSubmatch(ex.Inputs.Scenario, -1) {
		tok := m[1]
		if seen[tok] || !strings.Contains(truth, strings.ToLower(tok)) {
			continue
		}
		seen[tok] = true
		want = append(want, tok)
	}
	if len(want) == 0 {
		return Score{
			Name:    "resource_grounding",
			Skipped: true,
			Comment: "ground truth names no scenario identifier; nothing to ground against",
		}
	}

	hay := strings.ToLower(run.ScoredText())

	var hit, missing []string
	for _, t := range want {
		if strings.Contains(hay, strings.ToLower(t)) {
			hit = append(hit, t)
		} else {
			missing = append(missing, t)
		}
	}

	comment := fmt.Sprintf("named %d/%d identifiers", len(hit), len(want))
	if len(missing) > 0 {
		comment += ", missing=" + strings.Join(missing, ",")
	}
	return Score{
		Name:    "resource_grounding",
		Value:   float64(len(hit)) / float64(len(want)),
		Comment: comment,
	}
}

// DefaultEvaluators are the deterministic tier-1 evaluators: no model calls,
// no API keys, safe to gate CI on. The LLM judge is a separate opt-in pass.
func DefaultEvaluators() []Evaluator {
	return []Evaluator{SeverityAccuracy{}, SeverityCalibration{}, ToolCoverage{}, ResourceGrounding{}}
}

// Aggregate is the mean of non-skipped scores per evaluator.
type Aggregate struct {
	Means   map[string]float64 `json:"means"`
	Counts  map[string]int     `json:"counts"`
	Skipped map[string]int     `json:"skipped"`
	N       int                `json:"n"`
}

// Summarize reduces per-example scores to per-evaluator means.
func Summarize(all [][]Score) Aggregate {
	agg := Aggregate{
		Means:   map[string]float64{},
		Counts:  map[string]int{},
		Skipped: map[string]int{},
		N:       len(all),
	}
	sums := map[string]float64{}
	for _, scores := range all {
		for _, s := range scores {
			if s.Skipped {
				agg.Skipped[s.Name]++
				continue
			}
			sums[s.Name] += s.Value
			agg.Counts[s.Name]++
		}
	}
	for name, sum := range sums {
		if n := agg.Counts[name]; n > 0 {
			agg.Means[name] = sum / float64(n)
		}
	}
	return agg
}
