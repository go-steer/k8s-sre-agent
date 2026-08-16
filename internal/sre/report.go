package sre

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"

	"github.com/go-steer/core-sre-agent/internal/schema"
)

// ReportToolName is how the orchestrator delivers its structured HealthReport.
//
// # Why this exists instead of llmagent.Config.OutputSchema
//
// ADK's Chat-mode OutputSchema is Gemini-only, and it fails silently on
// everything else. `needOutputSchemaProcessor` gates the set_model_response
// injection on `googlellm.NeedsOutputSchemaProcessor`, which is
// `IsGeminiModel(llm.Name()) && !CanUseOutputSchemaWithTools(llm)`. With an
// Anthropic model the first conjunct is false, so no tool is injected and ADK
// falls back to setting `req.Config.ResponseSchema` natively
// (llminternal/basic_processor.go) — which the Anthropic/Vertex provider
// ignores. The agent then answers in prose. It is a *good* prose report, which
// is what makes it dangerous: the run looks successful and every structured
// evaluator scores zero.
//
// That was measured, not reasoned about: the first tier-1 run after the
// orchestrator moved to Chat mode returned 30 well-written markdown reports,
// `severity_accuracy` 0.000 and `severity_calibration` 0.022.
//
// So the report carrier is ours. It is the same mechanism ADK uses internally
// — a tool whose parameters are the output schema — minus the dependence on
// the model vendor. It also buys something OutputSchema never offered: the
// contract is validated at submission time and a violation is handed back to
// the model as a retryable tool error, instead of surfacing as a decode
// failure in the harness long after the run.
const ReportToolName = "submit_health_report"

// reportTool builds the report carrier. Shaped like lookout's offline tools
// (Declaration + toolutils.PackTool) rather than via functiontool, because
// schema.ReportSchema's hand-written field descriptions are the part the model
// actually reads and a reflected struct schema would drop them.
func reportTool() tool.Tool {
	return &submitReport{
		decl: &genai.FunctionDeclaration{
			Name: ReportToolName,
			Description: "Deliver your final health report. Call this exactly once, " +
				"as the last thing you do. This is the only channel through which " +
				"your report reaches the caller: prose in your reply is discarded.",
			Parameters: schema.ReportSchema(),
		},
	}
}

// ProtestMarker heads the acknowledgement of a report accepted after
// maxRejections handbacks. It exists so that "the contract held" and "the
// contract was given up on" do not read alike in a saved transcript — the same
// rule that makes UsageSummary print a coverage line and an unmeasured event
// write nothing rather than a zero.
const ProtestMarker = "ACCEPTED WITH UNRESOLVED CONTRACT VIOLATIONS:"

// Protest reports whether an acknowledgement is one of those, and returns the
// violations that were not resolved.
//
// It is the counting seam, and it exists for the same reason mast exports
// Stalled rather than just StallMarker: internal/evals has to be able to tell a
// protested submission from a clean one, and a second copy of the prefix match
// in another package drifts the day this wording changes. The match is at the
// *head* of the string, deliberately — an orchestrator that quotes the marker
// inside its own summary has still had its report accepted on the merits, and
// only the synthetic acknowledgement leads with it.
//
// The violations come back rather than a bare bool because they are the whole
// diagnosis. A protest says the model tried three times and could not satisfy
// a check; which check that was is what a human needs, and it is the same
// argument that makes a Run.Stalls entry carry the specialist's last words.
func Protest(result string) (violations string, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(result), ProtestMarker)
	if !found {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// maxRejections bounds how many times one run may have its report handed back.
//
// Every check in validate() is satisfiable — the identity ones by naming the
// object the finding is already about — so in the normal case this never
// binds. It exists for the case where it is not: an agent that cannot produce
// a token the layer check accepts would otherwise resubmit forever, and both
// eval commands ship with -max-turns unlimited because a ceiling that fires
// scores like a bad diagnosis. Refusing the report outright is the worse
// failure: `reported` 31/31 is the headline tier-1 result and a rejection loop
// costs the whole run, where accepting under protest costs exactly what the
// defect cost before this check existed.
const maxRejections = 3

type submitReport struct {
	decl *genai.FunctionDeclaration

	// ADK may run tool calls in parallel within a turn, and one instance is
	// built per agent rather than per call.
	mu         sync.Mutex
	rejections int
}

func (t *submitReport) Name() string                            { return t.decl.Name }
func (t *submitReport) Description() string                     { return t.decl.Description }
func (t *submitReport) IsLongRunning() bool                     { return false }
func (t *submitReport) Declaration() *genai.FunctionDeclaration { return t.decl }
func (t *submitReport) ProcessRequest(_ agent.Context, req *adkmodel.LLMRequest) error {
	return toolutils.PackTool(req, t)
}

// Run validates the submission and acknowledges it.
//
// The report itself is read off the FunctionCall arguments by whoever is
// consuming the event stream — the eval harness today, the monitor loop later
// — so this returns no payload. What it does return is a verdict: a malformed
// report comes back as an error the model can act on while it still has the
// evidence in context, which is the only moment a fix is cheap.
func (t *submitReport) Run(_ agent.Context, args any) (map[string]any, error) {
	const done = " You are done: acknowledge in one short sentence and do not call any further tools."

	report, err := decodeSubmission(args)
	if err != nil {
		if t.giveUp() {
			// A report that would not decode carries nothing to accept, so the
			// bound can only stop asking. Saying so is better than a fourth
			// identical rejection the model has already failed to act on.
			return map[string]any{"error": "the report still does not match the schema (" + err.Error() +
				") after " + fmt.Sprint(maxRejections) + " attempts. Stop calling " + ReportToolName +
				" and state your findings in prose instead."}, nil
		}
		return map[string]any{"error": err.Error() + " — fix and call " + ReportToolName + " again."}, nil
	}
	if problems := validate(report); len(problems) > 0 {
		if t.giveUp() {
			return map[string]any{
				"result": ProtestMarker + " " + strings.Join(problems, "; ") + "." + done,
			}, nil
		}
		return map[string]any{
			"error": "the report violates the contract: " + strings.Join(problems, "; ") +
				". Fix and call " + ReportToolName + " again.",
		}, nil
	}
	return map[string]any{"result": "Health report recorded." + done}, nil
}

// giveUp records one handback and reports whether the bound is now spent.
func (t *submitReport) giveUp() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rejections++
	return t.rejections > maxRejections
}

// decodeSubmission re-marshals the tool arguments through JSON into a HealthReport.
// The struct's json tags are the authoritative mapping from wire names to
// fields, so a round trip is more faithful than reading the map by hand.
func decodeSubmission(args any) (*schema.HealthReport, error) {
	blob, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("could not read the report arguments: %v", err)
	}
	var h schema.HealthReport
	if err := json.Unmarshal(blob, &h); err != nil {
		return nil, fmt.Errorf("the report does not match the schema: %v", err)
	}
	return &h, nil
}

// validate reports every contract violation at once rather than the first.
// One round trip per defect would let a report with three problems burn three
// turns of the budget, which is why schema.Validate — first-error-only, and
// the canonical structural contract — is used as one input rather than as the
// whole check.
// ValidateReport is the report contract, exported so a second producer can be
// held to it.
//
// The bounded pass (internal/bounded) emits the same schema.HealthReport this
// tool accepts, and "one contract, two producers" is load-bearing rather than
// tidy: internal/monitor fingerprints the report and diffs cycles on
// (kind, resource_name, reason), so a producer allowed to file findings the
// other would have rejected makes every switch between them look like the
// entire fault set changing.
//
// What the two do with the result differs, and must. This tool sits in a loop,
// so it hands violations back and the model fixes them with the evidence still
// in context. The bounded pass has one turn and no handback, so it emits the
// report and records the violations — the same trade maxRejections makes, taken
// immediately because there is nothing to retry. Both are visible through
// Protest.
func ValidateReport(h *schema.HealthReport) []string { return validate(h) }

func validate(h *schema.HealthReport) []string {
	var problems []string
	if err := h.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	if strings.TrimSpace(h.Summary) == "" {
		problems = append(problems, "summary is empty")
	}
	// The schema says overall_severity *is* the highest finding severity, so a
	// mismatch is a contract violation and not a judgement call.
	//
	// Note what this does and does not police. It cannot make a report less
	// alarming than its own findings — the agent still chooses every finding's
	// severity, and that is where tier 1's one-directional over-escalation
	// actually lives. It only rejects a report that disagrees with itself,
	// which is the case where the summary line and the findings list would
	// otherwise page two different people.
	if worst := h.DerivedSeverity(); h.OverallSeverity.Valid() && worst != h.OverallSeverity {
		problems = append(problems, fmt.Sprintf(
			"overall_severity is %q but the highest finding severity is %q; "+
				"overall_severity must equal the highest finding severity (or 'ok' with no findings)",
			h.OverallSeverity, worst))
	}
	problems = append(problems, unidentifiedFindings(h)...)
	// The two ways a *present* identity field still identifies nothing. See
	// identity.go for why both are checked here rather than in the evaluator.
	problems = append(problems, unnamedResources(h)...)
	problems = append(problems, crossLayerReasons(h)...)
	return problems
}

// unidentifiedFindings names findings that do not say what they are about.
//
// `kind`, `resource_name` and `reason` are optional in ReportSchema, because
// making them JSON-required would reject a legitimately cluster-scoped finding
// out of hand. They are not optional in practice: together with `namespace`
// they are the fingerprint internal/monitor diffs runs on, so a finding without
// them cannot be tracked, deduplicated, or closed — it is a paragraph, not a
// finding.
//
// That is not hypothetical either. On one tier-2 run `fault-failedjob` reported
// "Job fault-failedjob/nightly-report has failed permanently
// (BackoffLimitExceeded)" in its title and detail, left all four fields empty,
// and scored `fault_recall` 0.000 for a fault it had diagnosed correctly. The
// prose was right and the record was unusable, which is the exact failure mode
// a structured contract exists to prevent.
//
// Once, though — the next run filled the fields and scored 1.000, so this is an
// intermittent lapse rather than a standing behaviour. That is the argument for
// checking it here rather than in the evaluator: an intermittent contract
// violation is worse than a consistent one, because it makes a score that
// moved for this reason indistinguishable from one that moved because the
// diagnosis changed. Catching it at submission, while the model still holds the
// evidence, costs one round trip and removes the variance.
//
// `namespace` stays unchecked: cluster-scoped objects genuinely have none, and
// a required field an honest answer cannot fill teaches the model to invent one.
func unidentifiedFindings(h *schema.HealthReport) []string {
	var problems []string
	for i, f := range h.Findings {
		var missing []string
		if strings.TrimSpace(f.Kind) == "" {
			missing = append(missing, "kind")
		}
		if strings.TrimSpace(f.ResourceName) == "" {
			missing = append(missing, "resource_name")
		}
		if strings.TrimSpace(f.Reason) == "" {
			missing = append(missing, "reason")
		}
		if len(missing) == 0 {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"finding %d (%q) is missing %s — name the object the finding is about "+
				"and give a terse stable condition word; these fields identify the "+
				"finding across monitoring runs and the report is not usable without them",
			i+1, f.Title, strings.Join(missing, " and ")))
	}
	return problems
}

// compile-time proof that submitReport satisfies everything ADK needs of a
// callable tool; the runnable interface itself is unexported in package tool.
var _ interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(agent.Context, any) (map[string]any, error)
	ProcessRequest(agent.Context, *adkmodel.LLMRequest) error
} = (*submitReport)(nil)
