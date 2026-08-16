// Package evals implements the SRE agent's evaluation harness.
//
// # Two tiers
//
// Tier 1 ("scenario") replays the upstream project's 31 prose scenarios with
// repaired evaluators. Its job is the honest head-to-head: the same dataset
// and the same evaluators score both the Python original and this Go port.
//
// Tier 2 ("live") injects real faults into a throwaway kind cluster and
// asserts on the structured HealthReport the agent produces. Tier 1 measures
// how well an agent narrates a diagnosis it was handed in the prompt; only
// tier 2 measures whether the agent can actually find one.
//
// # Why the upstream evaluators are not reused verbatim
//
// All three upstream evaluators are defective against the upstream dataset,
// which is provable without running an agent (see upstream_test.go):
//
//  1. tool_coverage reads outputs["expected_trajectory"], but the dataset
//     writes "expected_tools". The key appears nowhere in evals/. Every
//     example therefore takes the empty-trajectory branch and scores a
//     constant 1.0.
//
//  2. severity_accuracy extracts severity with the regex \[(CRITICAL|...)\],
//     but 0 of 31 expected_response values contain a bracketed severity —
//     they are written "CRITICAL: ...". Extraction returns nil for the
//     expected side of every example, so the evaluator scores a constant 0.
//
//  3. 7 of 23 distinct expected_tools name tools that do not exist in the
//     agent's registry (kubectl_describe_service, kubectl_describe_node,
//     kubectl_get_service, kubectl_get_secrets, kubectl_describe_ingress,
//     kubectl_get_pvcs, kubectl_describe_hpa), affecting 13 of 31 examples.
//     Bug 1 hid this: a constant-1.0 evaluator is never inspected.
//
// Bugs 1 and 2 are fixed in code here. Bug 3 is data, and is repaired by the
// alias table in alias.go rather than by editing the upstream JSONL, so the
// original file stays a faithful copy and the repair stays auditable.
package evals

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/go-steer/k8s-sre-agent/internal/lookout"
	"github.com/go-steer/k8s-sre-agent/internal/schema"
)

// Example is one row of the tier-1 dataset, in the upstream wire shape.
type Example struct {
	Inputs struct {
		// Scenario is a prose description of a cluster situation.
		Scenario string `json:"scenario"`
	} `json:"inputs"`
	Outputs struct {
		// ExpectedTools is the reference trajectory. Note that upstream's
		// evaluator reads "expected_trajectory" instead, which is bug 1.
		ExpectedTools []string `json:"expected_tools"`
		// ExpectedActions is prose guidance, not currently scored.
		ExpectedActions []string `json:"expected_actions"`
		// ExpectedResponse is the ground-truth answer, prefixed with a bare
		// severity word ("CRITICAL: ..."), not a bracketed one.
		ExpectedResponse string `json:"expected_response"`
	} `json:"outputs"`
}

// LoadJSONL reads a newline-delimited JSON dataset.
func LoadJSONL(path string) ([]Example, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseJSONL(f)
}

// ParseJSONL reads a newline-delimited JSON dataset from r.
func ParseJSONL(r io.Reader) ([]Example, error) {
	var out []Example
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var ex Example
		if err := json.Unmarshal([]byte(text), &ex); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if ex.Inputs.Scenario == "" {
			return nil, fmt.Errorf("line %d: empty scenario", line)
		}
		out = append(out, ex)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("dataset is empty")
	}
	return out, nil
}

// Run is what an agent produced for one Example.
type Run struct {
	// Response is the agent's final free-text answer. Empty when the agent
	// returned a structured report instead.
	Response string
	// Trajectory is the ordered list of cluster tool names called, by the
	// orchestrator and by any specialist it delegated to.
	Trajectory []string
	// Calls is the same sequence with each call's arguments. Every evaluator
	// scores names only, so this is diagnostic rather than graded — but a name
	// on its own cannot answer why a run called the same tool four times, and
	// that is exactly the question a trajectory gets read for. A tier-3 run
	// called k8s_list_resources four times and the transcript could not say
	// whether that was four namespaces or one call repeated, which is the
	// difference between thorough and wasteful.
	Calls []lookout.Call `json:",omitempty"`
	// Delegations names the specialists the orchestrator invoked, in order.
	// Kept separate from Trajectory because it answers a different question:
	// Trajectory is what the agent looked at, Delegations is how it organized
	// the work. Without it there is no way to tell a specialist roster that is
	// being used from one that is inert.
	Delegations []string `json:",omitempty"`
	// DelegationErrors are delegations that came back carrying an error
	// instead of a specialist's answer.
	//
	// Recorded separately because Delegations counts function *calls*, and a
	// call that fails looks identical to one that succeeds until you read the
	// response. That gap hid a real defect: every specialist was reached
	// through agenttool, which runs a Task agent under its own runner and is
	// refused, so both eval tiers reported healthy delegation counts while no
	// specialist had ever executed. A count that cannot distinguish an attempt
	// from an outcome is not evidence, so the outcome is now recorded too and
	// cmd/sre-eval refuses to print a delegation line without it.
	DelegationErrors []string `json:",omitempty"`
	// Stalls names specialists that ended their turn without reporting and were
	// closed out by the stall guard, each with the last thing it said —
	// "name: text", the same shape as DelegationErrors. Use StallName to key a
	// histogram off an entry.
	//
	// A third outcome, alongside a real answer and an error. Before the stall
	// guard existed it was not an outcome at all — the run simply ended with no
	// report — and now that it degrades gracefully it is the one that most needs
	// counting, because a run with a stalled specialist scores like a complete
	// one while being an answer with a hole in it. This is the same lesson as
	// DelegationErrors, one rung further along.
	Stalls []string `json:",omitempty"`
	// Protests are the submissions accepted after the report tool ran out of
	// handbacks, each carrying the contract violations that were never
	// resolved. See sre.Protest, which is the seam this matches through.
	//
	// The same argument as Stalls, at the other end of the run. maxRejections
	// bounds how many times one report may be handed back, because a rejection
	// loop with -max-turns unlimited costs the whole run and `reported` 31/31
	// is the headline tier-1 result. Accepting under protest costs exactly what
	// the defect cost before the check existed — but only if somebody can tell
	// it happened, and a protested report is otherwise indistinguishable from
	// one that satisfied the contract on the first try. That is the "no silent
	// caps" rule applied to a bound the model hit rather than one the operator
	// set.
	//
	// It has never fired. It became much likelier to when crossLayerReasons
	// grew its second signal, which is why it is counted now rather than after
	// the first run that leaves everyone squinting at a transcript.
	Protests []string `json:",omitempty"`
	// Usage is the run's token consumption, per model. Diagnostic rather than
	// graded, for the same reason Calls is: a record that omits a field cannot
	// answer questions about it, and the questions here are already being
	// asked. Tier 3 measured 8 calls and 91 calls reaching the same headline
	// finding and could put no cost on the difference; the two model tiers are
	// an explicit cost decision that has never been checked; and every
	// delegate-or-do-it-yourself argument so far has been made on latency
	// alone. Cheap to record, and unrecoverable for any run that finishes
	// without it.
	Usage Usage `json:"usage,omitempty"`
	// Report is the agent's structured output, when it produced one. The Go
	// agent always does; the Python agent does so only on the scheduler path.
	Report *StructuredResult
	// Health is the full decoded report. Kept alongside Report so the tier-1
	// evaluators stay decoupled from the schema package while the transcript
	// still records what the agent actually emitted — a finding's kind and
	// resource_name are what the live tier asserts on.
	Health *schema.HealthReport `json:",omitempty"`
}

// StructuredResult carries the fields tier 1 can score from a HealthReport
// without depending on the full schema package shape.
type StructuredResult struct {
	OverallSeverity string `json:"overall_severity"`
	Summary         string `json:"summary"`
}

// ScoredText is the agent's answer as the text evaluators see it.
//
// An agent with an OutputSchema puts its answer in finish_task, not in prose,
// so scoring Response alone would grade a structured agent on whatever
// incidental text it happened to stream — usually nothing. Rendering the
// report into the same haystack is what lets one evaluator grade both the
// Python agent (prose) and this one (structured) without a second code path.
func (r Run) ScoredText() string {
	var b strings.Builder
	b.WriteString(r.Response)
	if r.Health == nil {
		return b.String()
	}
	fmt.Fprintf(&b, "\n%s: %s\n", strings.ToUpper(string(r.Health.OverallSeverity)), r.Health.Summary)
	for _, f := range r.Health.Findings {
		fmt.Fprintf(&b, "%s: %s — %s [%s %s/%s %s]\n",
			strings.ToUpper(string(f.Severity)), f.Title, f.Detail,
			f.Kind, f.Namespace, f.ResourceName, f.Reason)
	}
	for _, a := range r.Health.RecommendedActions {
		fmt.Fprintf(&b, "- %s\n", a)
	}
	return b.String()
}
