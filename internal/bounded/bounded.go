// Package bounded is the health check the scheduler runs every cycle.
//
// # Why this exists next to a perfectly good agent
//
// Upstream does not run its agent on a cycle, and the reasoning is in
// scheduler.py: run_structured_health_check "performs a fixed number of steps
// and therefore can never hit the agent's recursion limit — unlike routing a
// 'health check' request through the full Deep Agents orchestrator."
// MonitoringScheduler takes an agent and never calls it. mast's W4.3 reaches
// the same design independently and calls it their strongest lesson.
//
// Our own numbers say why. The 2026-08-15 tier-2 run cost $0.2374 per
// two-object namespace, and tier 3 measured 48s to 3m39s per real namespace at
// 8 to 91 tool calls. A five-minute cycle is 288 runs a day, so the full agent
// on ten namespaces is roughly $680/day and does not fit the interval at the
// slow end. This pass is one subagent-tier call over a zero-token collection:
// two subprocesses and one round trip, with a step count that cannot loop.
//
// # What it deliberately cannot do
//
// It cannot find a fault that is an *absence*. tier 3's prod-checkout is one
// Deployment, one pod, 1/1 Running, twelve days old, nothing wrong in any
// status field — and the correct answer is critical, because no Service exists
// and nothing can reach it. That came from enumerating a namespace and noticing
// what was missing, which a fixed-step scan of what *is* there structurally
// cannot do.
//
// So the collector is the two lookout scans and deliberately not
// kuberead.List. Adding enumeration would let the model reason about absences
// and would make this a small agent with a hidden step count, which is the
// thing being avoided. fault-badselector is the prediction: a Service selecting
// nothing in front of a healthy Deployment is the absence class in fixture
// form, and this pass should be expected to fail it. If a future version passes
// that fixture, check what it grew before believing it.
//
// # One contract, two producers
//
// The report is a schema.HealthReport, submitted through the same schema the
// agent's own report tool uses. That is not a convenience. internal/monitor
// fingerprints the report and diffs runs on (kind, resource_name, reason), so
// two producers with two vocabularies would make every switch between them look
// like the entire fault set changing. It is also what makes this measurable:
// every tier-2 fixture and all four live evaluators work against any producer
// of a HealthReport, so the cost of the bounded path is publishable as a delta
// against the agent's own numbers rather than argued about.
package bounded

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/k8s-sre-agent/internal/lookout"
	"github.com/go-steer/k8s-sre-agent/internal/schema"
	"github.com/go-steer/k8s-sre-agent/internal/sre"
)

// AgentName attributes this pass's tokens in an evals.Usage record. It is not
// an agent — there is no loop and no tool the model may choose — but the usage
// table is keyed by (author, model) and a cost that arrives under "unknown" is
// the failure that whole record exists to prevent.
const AgentName = "bounded-pass"

// ReportToolName is the single tool the model is required to call. It differs
// from the agent's submit_health_report on purpose: they carry the same schema
// but not the same contract, since nothing here validates the submission or
// hands it back. A shared name would make the two indistinguishable in a
// transcript, and telling them apart is the entire point of measuring one
// against the other.
const ReportToolName = "report_health"

// Config is one bounded pass.
type Config struct {
	// Lookout pins the cluster. Kubeconfig and Context are both required, for
	// the reasons lookout.Toolset documents: the ambient current-context is
	// never resolved.
	Lookout lookout.Config

	// Namespaces limits the scan. Empty scans every namespace, which is the
	// scheduler's normal mode; the eval harness passes exactly one.
	Namespaces []string

	// Model is the LLM for the single analysis call. The subagent tier is the
	// intended one — this is a summarizer over a pre-collected snapshot, which
	// is the shape Haiku is priced for, and it is most of why the cycle is
	// affordable at all.
	Model adkmodel.LLM

	// Now stamps the snapshot. Zero uses time.Now; a test sets it so the
	// prompt is deterministic.
	Now time.Time
}

// Snapshot is what the collection step produced. Text rather than a struct,
// because lookout's logfmt is already the compressed, secret-safe rendering
// this would otherwise have to invent, and it is the same vocabulary the agent
// reads through the MCP tools.
type Snapshot struct {
	// Health is `lookout health`: a ten-category scorecard in which every
	// category answers healthy, degraded or unavailable. The explicit healthy
	// is the property that makes this usable as the only input — "no findings"
	// and "the check did not run" are different lines rather than the same
	// silence.
	Health string

	// Delta is `lookout triage delta`: every abnormal object in one scan.
	Delta string

	// Errors are collection failures, one per scan that did not complete.
	//
	// They travel into the prompt rather than aborting the pass. A cycle that
	// reached one of two scans still knows something, and the model has to be
	// told which half is missing — otherwise it certifies a cluster it could
	// not see, which is the failure lookout.OfflineMessage exists to prevent
	// one layer up.
	Errors []string
}

// Empty reports whether the collection produced no usable output at all.
func (s Snapshot) Empty() bool {
	return strings.TrimSpace(s.Health) == "" && strings.TrimSpace(s.Delta) == ""
}

// Result is one completed pass.
type Result struct {
	Report   *schema.HealthReport
	Snapshot Snapshot
	// Response is the raw model response, kept so the caller can price the
	// call. See Event.
	Response *adkmodel.LLMResponse

	// Protests are report-contract violations that could not be handed back.
	//
	// The agent's report tool rejects these and the model fixes them with the
	// evidence in context; this pass has one turn, so the choice is between
	// emitting the report with the violations recorded and failing the cycle
	// outright. Emitting wins for the reason maxRejections exists at all — a
	// scheduler with no report for this cycle has no diff for this cycle, which
	// costs more than one imperfect finding — and recording is what stops the
	// two outcomes reading alike.
	//
	// Structural violations are different and are a hard error: a report that
	// does not satisfy schema.HealthReport.Validate cannot be fingerprinted at
	// all, so there is nothing to emit.
	Protests []string
}

// Event wraps the analysis call's usage in the shape evals.Usage.Observe reads,
// attributed to AgentName.
//
// The alternative was a second usage entry point taking an LLMResponse, and
// this is cheaper: a session.Event embeds one, so the existing record needs no
// new API and the (author, model) pairing it is built around still holds.
// Returns nil when the model reported no usage, because "not measured" and
// "free" must not render alike.
func (r Result) Event() *session.Event {
	if r.Response == nil || r.Response.UsageMetadata == nil {
		return nil
	}
	return &session.Event{Author: AgentName, LLMResponse: *r.Response}
}

// Check runs the whole pass: collect, render, one model call.
//
// Every step is unconditional and there is no loop, which is the property the
// scheduler is buying. A collection failure does not abort — see Snapshot.Errors
// — but a collection that produced nothing at all does, because a report
// written from no evidence is worse than no report.
func Check(ctx context.Context, cfg Config) (Result, error) {
	if cfg.Model == nil {
		return Result{}, fmt.Errorf("bounded: Config.Model is required")
	}
	snap := Collect(ctx, cfg.Lookout, cfg.Namespaces)
	if snap.Empty() {
		return Result{Snapshot: snap}, fmt.Errorf("bounded: no cluster data collected: %s",
			strings.Join(snap.Errors, "; "))
	}
	report, resp, err := Analyse(ctx, cfg.Model, snap, cfg.Namespaces, cfg.Now)
	res := Result{Report: report, Snapshot: snap, Response: resp}
	if report != nil {
		res.Protests = sre.ValidateReport(report)
	}
	return res, err
}

// Collect runs the two scans. No model call, so no tokens.
//
// It returns a Snapshot rather than an error: a scan that fails records itself
// in Errors and the other one still runs. Both failing is the caller's problem
// to notice, which Check does through Snapshot.Empty.
func Collect(ctx context.Context, cfg lookout.Config, namespaces []string) Snapshot {
	var snap Snapshot
	scope := scopeArgs(namespaces)

	health, err := lookout.Scan(ctx, cfg, append([]string{"health"}, scope...)...)
	snap.Health = health
	if err != nil {
		snap.Errors = append(snap.Errors, err.Error())
	}

	delta, err := lookout.Scan(ctx, cfg, append([]string{"triage", "delta"}, scope...)...)
	snap.Delta = delta
	if err != nil {
		snap.Errors = append(snap.Errors, err.Error())
	}
	return snap
}

// scopeArgs renders the namespace selection as lookout flags.
//
// lookout takes one --namespace or -A and has no repeatable form, so more than
// one namespace scans everything and the prompt names the ones that matter.
// Over-collecting is the safe direction here: the alternative is one subprocess
// pair per namespace, which multiplies the fixed cost this pass exists to keep
// fixed.
func scopeArgs(namespaces []string) []string {
	if len(namespaces) == 1 {
		return []string{"--namespace=" + namespaces[0]}
	}
	return []string{"-A"}
}

// Analyse makes exactly one model call and returns the report it was forced to
// produce.
//
// # Forced tool use, and why it is not the OutputSchema trap
//
// This looks like the mechanism that cost a whole tier-1 run: ADK's Chat-mode
// OutputSchema is gated on IsGeminiModel, so with an Anthropic model nothing is
// injected, the native ResponseSchema is ignored, and the agent answers in
// prose while scoring zero on every structured evaluator. It is not the same
// mechanism. FunctionCallingConfig with mode ANY and exactly one allowed name
// is mapped by mast's Anthropic provider onto Anthropic's own
// tool_choice: {type: "tool", name: ...} (pkg/providers/anthropic/convert.go,
// toolChoiceParam), so the constraint reaches the API rather than being dropped
// on a vendor check. TestTheReportToolIsForcedOnTheWire asserts it on the
// request, because "a contract that looks configured and isn't" is the failure
// this repo has now shipped twice.
func Analyse(ctx context.Context, m adkmodel.LLM, snap Snapshot, namespaces []string, now time.Time) (*schema.HealthReport, *adkmodel.LLMResponse, error) {
	req := Request(snap, namespaces, now)
	req.Model = m.Name()

	var last *adkmodel.LLMResponse
	for resp, err := range m.GenerateContent(ctx, req, false) {
		if err != nil {
			return nil, last, fmt.Errorf("bounded: analyse: %w", err)
		}
		if resp == nil || resp.Partial {
			continue
		}
		last = resp
	}
	if last == nil {
		return nil, nil, fmt.Errorf("bounded: analyse: model returned no response")
	}

	report, err := reportFrom(last)
	if err != nil {
		return nil, last, err
	}
	return report, last, nil
}

// reportFrom pulls the forced call's arguments out of a response.
//
// A response with no function call is an error rather than a fallback to
// parsing the prose. Prose is exactly what the forced call exists to prevent,
// and accepting it would reintroduce the regex-scraping the report schema
// replaced — quietly, on whichever model version stopped honouring tool_choice.
func reportFrom(resp *adkmodel.LLMResponse) (*schema.HealthReport, error) {
	if resp.Content == nil {
		return nil, fmt.Errorf("bounded: the model returned no content")
	}
	for _, p := range resp.Content.Parts {
		if p == nil || p.FunctionCall == nil || p.FunctionCall.Name != ReportToolName {
			continue
		}
		blob, err := json.Marshal(p.FunctionCall.Args)
		if err != nil {
			return nil, fmt.Errorf("bounded: re-marshal report args: %w", err)
		}
		var h schema.HealthReport
		if err := json.Unmarshal(blob, &h); err != nil {
			return nil, fmt.Errorf("bounded: decode report: %w", err)
		}
		// Structural only, here. A report that fails this cannot be
		// fingerprinted at all, so there is nothing to emit and the cycle fails
		// loudly. The rest of the contract — the consistency rule and the two
		// identity checks — is checked by Check through sre.ValidateReport and
		// recorded in Result.Protests rather than thrown away, because a
		// scheduler with no report for this cycle has no diff for this cycle.
		if err := h.Validate(); err != nil {
			return nil, fmt.Errorf("bounded: the report is structurally invalid: %w", err)
		}
		return &h, nil
	}
	return nil, fmt.Errorf("bounded: the model answered without calling %s, so tool_choice "+
		"did not reach the provider — check the FunctionCallingConfig mapping before "+
		"trusting any number from this pass", ReportToolName)
}

// Request builds the single LLMRequest. Exported so a test can assert on the
// wire form without a model.
func Request(snap Snapshot, namespaces []string, now time.Time) *adkmodel.LLMRequest {
	if now.IsZero() {
		now = time.Now()
	}
	return &adkmodel.LLMRequest{
		Contents: []*genai.Content{
			genai.NewContentFromText(userPrompt(snap, namespaces, now), genai.RoleUser),
		},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: genai.NewContentFromText(systemPrompt, genai.RoleUser),
			Tools: []*genai.Tool{{
				FunctionDeclarations: []*genai.FunctionDeclaration{{
					Name:        ReportToolName,
					Description: "Report the structured health assessment of the scanned namespaces.",
					Parameters:  schema.ReportSchema(),
				}},
			}},
			ToolConfig: &genai.ToolConfig{
				FunctionCallingConfig: &genai.FunctionCallingConfig{
					// ANY with exactly one allowed name is the spelling mast's
					// Anthropic provider turns into a pinned tool_choice. Any
					// other combination becomes "any" or is dropped, and a
					// dropped constraint here is a prose answer nothing catches.
					Mode:                 genai.FunctionCallingConfigModeAny,
					AllowedFunctionNames: []string{ReportToolName},
				},
			},
		},
	}
}

// systemPrompt is short on purpose.
//
// This model gets one turn over a snapshot it did not gather and cannot add to,
// so there is nothing useful to say about tool selection, delegation or
// investigation — the three things the orchestrator's spec is mostly about. The
// severity rubric is the one part worth carrying across, and it is carried in
// the same impact-keyed form for the reason tier 1 established: grading on
// symptom name ("OOM kills are critical") is what produced systematic
// over-escalation, and the two producers have to agree on severity or the diff
// reports a level change every time the scheduler escalates.
const systemPrompt = `You are an SRE assistant summarising a pre-collected Kubernetes scan.

You receive the output of two read-only scans and nothing else. You cannot run
further checks, and there is no one to ask: report what the scan supports and
say plainly what it does not cover.

Call ` + ReportToolName + ` exactly once. Every finding must name one object —
its kind, its own name, and the reason the control plane gave for its state. A
finding whose reason belongs to a different object is worse than no finding: it
cannot be matched against the next cycle.

Severity is about current impact, not the name of the symptom:
  critical  users are affected now, or a workload cannot serve at all
  warning   degraded, at risk, or self-recovering — a pod that restarts back
            into service is a warning, not a critical
  info      worth knowing, no action needed
  ok        healthy
Set overall_severity to the highest severity among your findings, or "ok" when
there are none. Do not invent findings to fill the report: a scan that found
nothing wrong is an "ok" report with no findings, and that is a useful answer.`

// userPrompt renders the snapshot and says what was scanned.
//
// The scope line is not decoration. A scan of every namespace reports findings
// from namespaces nobody asked about, and without being told which ones matter
// the model ranks them all equally — the same failure the orchestrator's
// scan-scoping problem describes, arriving here for a different reason.
func userPrompt(snap Snapshot, namespaces []string, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Cluster scan collected at %s.\n", now.UTC().Format(time.RFC3339))
	if len(namespaces) > 0 {
		fmt.Fprintf(&b, "Report on these namespaces only: %s.\n", strings.Join(namespaces, ", "))
	} else {
		b.WriteString("Report on the whole cluster.\n")
	}
	// The failures first: a model that reads them last has already formed a
	// view from partial data.
	if len(snap.Errors) > 0 {
		b.WriteString("\nSome collection steps failed. Do not certify what they would have " +
			"covered; say which parts are unchecked.\n")
		for _, e := range snap.Errors {
			fmt.Fprintf(&b, "  - %s\n", e)
		}
	}
	// The scorecard is context, not findings, and saying so is load-bearing.
	// Every category answers healthy|degraded|unavailable, which is what makes
	// a clean namespace legible — but a category is not an object, so a finding
	// filed from one has no kind and no resource_name and cannot satisfy the
	// report contract. Measured rather than predicted: on a kind cluster the
	// control-plane category answers `unavailable — requires cloud provider
	// metrics`, the model filed it as a finding, and the report was accepted
	// under protest for a missing resource_name. Same root cause as the
	// scorecard lines the scheduler strips before diffing them.
	b.WriteString("\n## health scorecard (lookout health)\n")
	b.WriteString("Category status lines, for context. A category is not an object: use these to " +
		"judge what is healthy and what to look at, and do not file a finding about a category. " +
		"A category that is `unavailable` means that check could not run here — say so in the " +
		"summary rather than reporting it as a fault.\n")
	b.WriteString(orNone(snap.Health))
	b.WriteString("\n## abnormal objects (lookout triage delta)\n")
	b.WriteString(orNone(snap.Delta))
	return b.String()
}

// orNone renders an empty scan as an explicit statement rather than a blank.
//
// lookout's own contract is that a result without a summary line is void and
// findings=0 with a summary means scanned-and-healthy. A blank section here
// would collapse those two into the ambiguous silence the whole read path is
// built to avoid.
func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(this scan produced no output — treat it as not run, not as clean)\n"
	}
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s
}
