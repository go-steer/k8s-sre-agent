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

package evals

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/budget"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/k8s-sre-agent/internal/lookout"
	"github.com/go-steer/k8s-sre-agent/internal/schema"
	"github.com/go-steer/k8s-sre-agent/internal/sre"
)

// AppName identifies eval sessions in the session store.
const AppName = "sre-eval"

// Runner executes one example against a built agent and collects everything
// the evaluators need: the final text, the tool trajectory, and the decoded
// HealthReport when the agent produced one.
type Runner struct {
	Agent    adkagent.Agent
	Recorder *lookout.Recorder
	// Specialists is the delegation roster. Optional; see Runner.known.
	Specialists []string
	// Limits bound one run's model spend. The zero value is unlimited, and
	// that is the default every command ships with.
	//
	// Off unless the operator asks is deliberate, and it is the same rule as
	// "no silent caps" in the eval harness: a ceiling that fires part-way
	// through a suite turns a run into something that reads exactly like an
	// agent that failed to report, and a baseline whose examples were
	// quietly cut short is worse than an expensive one. What a ceiling is
	// for is the operational path — a scheduler on a cycle, where an agent
	// that loops is a bill rather than a bad number.
	//
	// Enforcement is after the call, not before it: the meter sees a
	// request's usage only once its event lands, so a single runaway call
	// still completes. See pkg/budget.
	Limits budget.Limits
}

// Run executes a single scenario in a fresh session.
//
// Each example gets its own session: the scenarios are independent incidents,
// and sharing a session would let example 7's context leak into example 8's
// diagnosis — which inflates scores in a way that looks like capability.
func (r *Runner) Run(ctx context.Context, id string, ex Example) (Run, error) {
	if r.Recorder != nil {
		r.Recorder.Reset()
	}

	rn, err := runner.NewInMemory(AppName, r.Agent)
	if err != nil {
		return Run{}, fmt.Errorf("build runner: %w", err)
	}

	msg := genai.NewContentFromText(ex.Inputs.Scenario, genai.RoleUser)

	var text strings.Builder
	var health *schema.HealthReport
	var healthRank int
	var delegations, delegationErrors, stalls, protests []string
	var usage Usage
	meter := budget.NewMeter(r.Limits)

	// snapshot assembles what the run produced so far. A failed run is not an
	// empty run: the delegations and tool calls that preceded the failure are
	// the only evidence of what the agent was doing when it broke, and
	// returning Run{} throws them away — which is how an intermittent ADK
	// failure in the live tier came back with an empty trajectory and nothing
	// to reason about.
	snapshot := func() Run {
		out := Run{
			Response:         strings.TrimSpace(text.String()),
			Health:           health,
			Delegations:      delegations,
			DelegationErrors: delegationErrors,
			Stalls:           stalls,
			Protests:         protests,
			Usage:            usage,
		}
		if health != nil {
			out.Report = &StructuredResult{
				OverallSeverity: string(health.OverallSeverity),
				Summary:         health.Summary,
			}
		}
		if r.Recorder != nil {
			out.Trajectory = r.Recorder.Names()
			out.Calls = r.Recorder.Calls()
		}
		return out
	}

	// StreamingModeNone: we consume the terminal event of each turn, and
	// partials would only duplicate text into the scored response.
	for ev, err := range rn.Run(ctx, "eval", id, msg, adkagent.RunConfig{}) {
		if err != nil {
			return snapshot(), fmt.Errorf("run %s: %w", id, err)
		}
		if ev.Partial {
			continue
		}
		// After the Partial check, deliberately. Each terminal model response
		// carries the usage for its own request and the bill is their sum, so
		// counting a partial as well would double-charge whatever turn it
		// belonged to.
		usage.Observe(ev)
		// The two accountants are separate on purpose. usage is the record and
		// never refuses anything; the meter is the ceiling and never keeps a
		// breakdown. Folding them together would mean either a record that can
		// abort a run or a ceiling that has to be asked about the answer.
		//
		// Abandoning the iterator here leaves the run's partial trajectory in
		// the snapshot, which is the point: a run stopped by its own ceiling
		// should say what it had spent the money on.
		if err := meter.Observe(ev); err != nil {
			return snapshot(), fmt.Errorf("run %s: %w", id, err)
		}
		// Keep the best carrier seen, not the last one. Specialists report
		// through finish_task too, so last-wins would credit a specialist's
		// sub-report as the orchestrator's answer on any run where the
		// orchestrator failed to submit one — scoring a partial fan-out as a
		// complete diagnosis.
		if h, rank := extractReport(ev); h != nil && rank >= healthRank {
			health, healthRank = h, rank
		}
		delegations = append(delegations, specialistCalls(ev, r.known())...)
		delegationErrors = append(delegationErrors, failedDelegations(ev, r.known())...)
		stalls = append(stalls, stalledDelegations(ev, r.known())...)
		protests = append(protests, protestedSubmissions(ev)...)
		text.WriteString(eventText(ev))
	}

	return snapshot(), nil
}

// Specialists are the specialist names to count as delegations. When empty,
// any function call that is neither a cluster tool nor finish_task counts —
// which is the right default for a caller that has not enumerated them.
func (r *Runner) known() map[string]bool {
	if len(r.Specialists) == 0 {
		return nil
	}
	out := make(map[string]bool, len(r.Specialists))
	for _, n := range r.Specialists {
		out[n] = true
	}
	return out
}

// specialistCalls extracts delegation calls from an event.
//
// A delegated specialist appears as an ordinary function call named after the
// agent, so it is indistinguishable from a tool by shape alone. The names are
// matched against the known roster when one is supplied; otherwise cluster
// tools (which the Recorder already captures) and the report carriers are
// excluded and the rest is treated as delegation.
//
// This counts attempts. See failedDelegations for why that is not enough.
func specialistCalls(ev *session.Event, known map[string]bool) []string {
	if ev.Content == nil {
		return nil
	}
	var out []string
	for _, p := range ev.Content.Parts {
		if p.FunctionCall == nil {
			continue
		}
		name := p.FunctionCall.Name
		switch {
		case known != nil:
			if known[name] {
				out = append(out, name)
			}
		case reportCarriers[name] > 0, strings.HasPrefix(name, "k8s_"):
			// A cluster tool or a report carrier, not a delegation.
		default:
			out = append(out, name)
		}
	}
	return out
}

// failedDelegations names specialists whose response carried an error.
//
// The framework reports a broken delegation as a normal FunctionResponse with
// an "error" key, which specialistCalls has no way to see — it only reads the
// call. Counting calls alone is what let a completely inert specialist roster
// report full delegation across both eval tiers for two baselines running.
func failedDelegations(ev *session.Event, known map[string]bool) []string {
	if ev.Content == nil {
		return nil
	}
	var out []string
	for _, p := range ev.Content.Parts {
		fr := p.FunctionResponse
		if fr == nil || fr.Response == nil {
			continue
		}
		if known != nil && !known[fr.Name] {
			continue
		}
		if known == nil && (reportCarriers[fr.Name] > 0 || strings.HasPrefix(fr.Name, "k8s_")) {
			continue
		}
		for _, key := range []string{"error", "Error"} {
			if v, ok := fr.Response[key]; ok && fmt.Sprint(v) != "" {
				out = append(out, fmt.Sprintf("%s: %v", fr.Name, v))
				break
			}
		}
	}
	return out
}

// stalledDelegations names specialists that came back with a stall report —
// the empty HealthReport sre.stallReport writes, through mast's stall guard,
// when a specialist ends its turn without calling finish_task.
//
// Neither of the other two counters sees it. It is not an error: the delegation
// resolved cleanly and carries a schema-valid report. It is not an answer
// either: the specialist looked at nothing and concluded nothing. Left
// uncounted it is the most flattering of the three, because the run finishes,
// the orchestrator reports, and the scores look like those of an agent whose
// fan-out all came back.
func stalledDelegations(ev *session.Event, known map[string]bool) []string {
	if ev.Content == nil {
		return nil
	}
	var out []string
	for _, p := range ev.Content.Parts {
		fr := p.FunctionResponse
		if fr == nil || fr.Response == nil {
			continue
		}
		if known != nil && !known[fr.Name] {
			continue
		}
		if s, ok := fr.Response["summary"].(string); ok && isStallReport(s) {
			out = append(out, stallEntry(fr.Name, s))
		}
	}
	return out
}

// stallEntry records the stall as "name: what the specialist said last",
// matching failedDelegations' shape.
//
// The words are the point. A specialist stalls because it ran out of moves, so
// the question it wanted to ask names the data the read path could not give it
// — which is how the missing enumeration tool was found. Recording the name
// alone would count the failure and discard the only diagnosis of it.
func stallEntry(name, summary string) string {
	said := ""
	// mastagent.StallText appends the specialist's own words after a blank line,
	// following the marker and the instruction to the orchestrator.
	if _, rest, ok := strings.Cut(summary, "\n\n"); ok {
		said = strings.TrimSpace(rest)
	}
	if said == "" {
		return name
	}
	return name + ": " + said
}

// protestedSubmissions names the contract violations of any report accepted
// under protest in this event.
//
// Unlike the three delegation counters this takes no roster, because the report
// tool is the orchestrator's alone — a specialist reports through finish_task,
// whose payload ADK validates against the spec's OutputSchema rather than
// through validate(). So there is no name to key on and nothing to filter: any
// acknowledgement that leads with the marker is the one submission this run
// gave up on.
func protestedSubmissions(ev *session.Event) []string {
	if ev.Content == nil {
		return nil
	}
	var out []string
	for _, p := range ev.Content.Parts {
		fr := p.FunctionResponse
		if fr == nil || fr.Response == nil {
			continue
		}
		// The protest travels on the success channel, not the error one: the
		// report *was* accepted. That is the whole hazard — it looks like an
		// ordinary acknowledgement to everything that is not looking for it.
		result, ok := fr.Response["result"].(string)
		if !ok {
			continue
		}
		if violations, protested := sre.Protest(result); protested {
			out = append(out, violations)
		}
	}
	return out
}

// StallName is the specialist named by a Run.Stalls entry, for callers
// aggregating a histogram out of entries that also carry the stall text.
func StallName(entry string) string {
	if name, _, ok := strings.Cut(entry, ": "); ok {
		return name
	}
	return entry
}

// isStallReport reports whether a summary is one the stall guard wrote for a
// specialist that stopped without reporting.
//
// mastagent.Stalled is the counting seam mast exports for exactly this, and it
// matches the marker at the head of the string on purpose: an orchestrator that
// quotes a specialist's stall inside its own summary has still written a real
// report, and only the synthetic one leads with it.
func isStallReport(summary string) bool {
	return mastagent.Stalled(summary)
}

// eventText concatenates an event's text parts.
func eventText(ev *session.Event) string {
	if ev.Content == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range ev.Content.Parts {
		if p.Text != "" {
			b.WriteString(p.Text)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// reportCarriers are the tool calls through which an agent with an OutputSchema
// delivers its structured result.
//
// Which one is used is a property of the agent's mode and of the model vendor,
// not of our code:
//
//   - submit_health_report is ours (see internal/sre) and is the live
//     path for the Chat-mode orchestrator. It exists because ADK's Chat-mode
//     OutputSchema is honoured for Gemini models only and degrades to prose
//     for Anthropic ones, which scored an entire tier-1 run at zero.
//   - finish_task is ADK's Task-mode carrier. The specialists are still Task
//     mode, so this is how their sub-reports arrive.
//   - set_model_response is what ADK injects for a Chat agent with an
//     OutputSchema on a Gemini model. Nothing produces it today; it stays so
//     that changing a model tier's vendor cannot silently blank the report
//     column, which is the exact failure this map was widened for once already.
//
// The values rank the carriers. A run emits several — every specialist
// finishes with finish_task before the orchestrator answers — and the one that
// counts is the orchestrator's, so the higher rank wins regardless of order.
var reportCarriers = map[string]int{
	"submit_health_report": 2,
	"set_model_response":   2,
	"finish_task":          1,
}

// extractReport pulls a HealthReport out of an event.
//
// The report arrives as a function call rather than as text. Falling back to
// parsing prose would score the summary, not the structured contract we
// actually ship — a real regression in the schema would then be invisible to
// the evals.
//
// The rank identifies which carrier it came through; see reportCarriers.
//
// A stall report is refused outright. The stall guard writes a schema-valid
// "ok / no findings" HealthReport through finish_task on behalf of a specialist
// that stopped talking, and it is a placeholder, not an answer — crediting it
// would score a run whose orchestrator never reported as one that surveyed the
// cluster and found it healthy. That is the worst reading available: it turns
// the degradation this harness exists to notice into a clean sheet. Only the
// rank-1 carrier is filtered, because an orchestrator quoting a specialist's
// stall in its own submit_health_report has still written a real report.
func extractReport(ev *session.Event) (*schema.HealthReport, int) {
	if ev.Content == nil {
		return nil, 0
	}
	var best *schema.HealthReport
	var bestRank int
	for _, p := range ev.Content.Parts {
		if p.FunctionCall == nil {
			continue
		}
		rank := reportCarriers[p.FunctionCall.Name]
		if rank <= bestRank {
			continue
		}
		h := decodeReport(p.FunctionCall.Args)
		if h == nil {
			continue
		}
		if rank == 1 && isStallReport(h.Summary) {
			continue
		}
		best, bestRank = h, rank
	}
	return best, bestRank
}

// decodeReport re-marshals finish_task's args through JSON into HealthReport.
// The args arrive as map[string]any, and the struct's json tags are the only
// authoritative mapping from wire names to fields.
func decodeReport(args map[string]any) *schema.HealthReport {
	if len(args) == 0 {
		return nil
	}
	// finish_task may wrap the payload in a single-key envelope depending on
	// how the schema was installed; unwrap one level when that is the shape.
	if len(args) == 1 {
		for _, v := range args {
			if inner, ok := v.(map[string]any); ok {
				if _, hasSeverity := inner["overall_severity"]; hasSeverity {
					args = inner
				}
			}
		}
	}
	blob, err := json.Marshal(args)
	if err != nil {
		return nil
	}
	var h schema.HealthReport
	if err := json.Unmarshal(blob, &h); err != nil {
		return nil
	}
	if h.OverallSeverity == "" && h.Summary == "" {
		return nil
	}
	return &h
}
