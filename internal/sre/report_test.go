package sre

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/k8s-sre-agent/internal/lookout"
)

// The report carrier has to be visible to the model, and nothing else in the
// test suite can see it: ADK's Agent interface exposes no tool list, which is
// precisely why the previous carrier could be inert for two published
// baselines without a single test noticing. The only surface that tells the
// truth is the LLMRequest, so this test reads that.
func TestOrchestratorDeclaresTheReportTool(t *testing.T) {
	offline, _, err := lookout.Offline()
	if err != nil {
		t.Fatalf("offline toolset: %v", err)
	}
	rec := &recordingModel{}
	root, err := Build(Config{Main: rec, Subagent: rec, Toolsets: []tool.Toolset{offline}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rn, err := runner.NewInMemory("sre-report-wiring", root)
	if err != nil {
		t.Fatalf("NewInMemory: %v", err)
	}
	// The model yields nothing, so the turn ends after one request — which is
	// all this needs: the declarations are attached before the call.
	for _, err := range rn.Run(context.Background(), "u", "s",
		genai.NewContentFromText("ping", genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	declared := rec.declared()
	if len(declared) == 0 {
		t.Fatal("the model was never called, so this test proves nothing")
	}
	decl := declared[ReportToolName]
	if decl == nil {
		t.Fatalf("the orchestrator does not declare %s; it has no way to return a "+
			"structured report. Declared: %v", ReportToolName, keys(declared))
	}
	if decl.Parameters == nil || decl.Parameters.Properties["overall_severity"] == nil {
		t.Errorf("%s is declared without the report schema: %+v", ReportToolName, decl.Parameters)
	}
	// The delegation tools travel on the same request. Asserting them here
	// keeps "the specialists are reachable" a wire fact rather than an
	// inference from SubAgents being non-empty.
	for _, name := range readSpecialists(t) {
		if declared[name] == nil {
			t.Errorf("specialist %q is not declared as a delegation tool", name)
		}
	}
	// And the write specialist is not, because this Build granted no writes.
	// The orchestrator's spec tells it to route mutations to change-executor;
	// offering that target on a deployment that cannot mutate anything is how
	// an agent comes to report a change it never made.
	if declared[WriteAgentName] != nil {
		t.Errorf("%s is declared as a delegation target on a read-only deployment", WriteAgentName)
	}
}

func TestSubmitHealthReportAcceptsAValidReport(t *testing.T) {
	out := runReportTool(t, map[string]any{
		"overall_severity": "critical",
		"summary":          "CRITICAL: api-server is CrashLoopBackOff (18 restarts)",
		"findings": []any{map[string]any{
			"severity":      "critical",
			"title":         "api-server is CrashLoopBackOff",
			"detail":        "18 restarts, exit code 1 within seconds of start.",
			"namespace":     "production",
			"kind":          "Pod",
			"resource_name": "api-server-7d8f9c-xkp2v",
			"reason":        "CrashLoopBackOff",
		}},
	})
	if errText, ok := out["error"]; ok {
		t.Fatalf("a valid report was rejected: %v", errText)
	}
	if _, ok := out["result"]; !ok {
		t.Fatalf("no acknowledgement: %v", out)
	}
}

func TestSubmitHealthReportRejectsContractViolations(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{{
		name: "severity outside the enum",
		args: map[string]any{"overall_severity": "urgent", "summary": "s", "findings": []any{}},
		want: "overall_severity",
	}, {
		name: "empty summary",
		args: map[string]any{"overall_severity": "ok", "summary": "   ", "findings": []any{}},
		want: "summary is empty",
	}, {
		// The failure that matters operationally: the summary line says one
		// thing and the findings list says another, so the report pages
		// whoever reads it first and not the person who should be paged.
		name: "overall_severity disagrees with the findings",
		args: map[string]any{
			"overall_severity": "critical",
			"summary":          "CRITICAL: something",
			"findings": []any{map[string]any{
				"severity": "warning", "title": "hpa at max", "detail": "d",
			}},
		},
		want: "highest finding severity",
	}, {
		name: "wrong type for a schema field",
		args: map[string]any{"overall_severity": "ok", "summary": "s", "findings": "none"},
		want: "does not match the schema",
	}, {
		// Measured, not imagined: tier-2's fault-failedjob named the Job in
		// its title and detail, left the identity fields empty, and scored
		// fault_recall 0.000 on a fault it had diagnosed correctly.
		name: "finding does not say what it is about",
		args: map[string]any{
			"overall_severity": "warning",
			"summary":          "WARNING: a job failed",
			"findings": []any{map[string]any{
				"severity": "warning",
				"title":    "Job nightly-report failed permanently",
				"detail":   "BackoffLimitExceeded after 2 attempts",
			}},
		},
		want: "missing kind and resource_name and reason",
	}, {
		// namespace is deliberately not required: cluster-scoped objects have
		// none, and demanding one teaches the model to invent it.
		name: "a cluster-scoped finding needs no namespace",
		args: map[string]any{
			"overall_severity": "warning",
			"summary":          "WARNING: a node is under pressure",
			"findings": []any{map[string]any{
				"severity": "warning", "title": "node pressure", "detail": "d",
				"kind": "Node", "resource_name": "kind-worker",
			}},
		},
		want: "missing reason",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := runReportTool(t, tc.args)
			errText, ok := out["error"].(string)
			if !ok {
				t.Fatalf("accepted an invalid report: %v", out)
			}
			if !strings.Contains(errText, tc.want) {
				t.Errorf("error %q does not mention %q", errText, tc.want)
			}
			// The model has to know what to do next, or it falls back to prose
			// and the run yields nothing at all.
			if !strings.Contains(errText, ReportToolName) {
				t.Errorf("rejection %q does not tell the model to call %s again", errText, ReportToolName)
			}
		})
	}
}

// A report with several defects must come back as one message. One defect per
// round trip spends the turn budget on formatting.
func TestSubmitHealthReportReportsEveryProblemAtOnce(t *testing.T) {
	out := runReportTool(t, map[string]any{
		"overall_severity": "ok",
		"summary":          "",
		"findings": []any{map[string]any{
			"severity": "warning", "title": "", "detail": "d",
		}},
	})
	errText, _ := out["error"].(string)
	for _, want := range []string{"title", "summary is empty", "highest finding severity"} {
		if !strings.Contains(errText, want) {
			t.Errorf("error %q omits %q", errText, want)
		}
	}
}

// A rejection loop costs the whole run, and every command ships with
// -max-turns unlimited. So the handback is bounded, and the report that gets
// through says out loud that it did not satisfy the contract — accepting
// silently would make a given-up run read exactly like a clean one, which is
// the failure the marker exists to prevent.
func TestTheHandbackIsBoundedAndTheGiveUpIsLabelled(t *testing.T) {
	rt, ok := reportTool().(*submitReport)
	if !ok {
		t.Fatalf("reportTool is not a *submitReport")
	}
	// A defect the model cannot talk its way out of by rewording: the layer
	// check on an object that has no such failure mode.
	args := map[string]any{
		"overall_severity": "critical",
		"summary":          "PVC ledger-data is stuck",
		"findings": []any{map[string]any{
			"severity": "critical", "title": "PVC stuck Pending", "detail": "d",
			"kind": "PersistentVolumeClaim", "resource_name": "ledger-data",
			"reason": "Unschedulable",
		}},
	}

	for i := 1; i <= maxRejections; i++ {
		out, err := rt.Run(nil, args)
		if err != nil {
			t.Fatalf("attempt %d returned a Go error: %v", i, err)
		}
		if _, rejected := out["error"]; !rejected {
			t.Fatalf("attempt %d was accepted; the bound must not fire before %d handbacks",
				i, maxRejections)
		}
	}

	out, err := rt.Run(nil, args)
	if err != nil {
		t.Fatalf("the give-up returned a Go error: %v", err)
	}
	if _, rejected := out["error"]; rejected {
		t.Fatalf("the handback is unbounded; a run that cannot satisfy the check never reports")
	}
	result, _ := out["result"].(string)
	if !strings.HasPrefix(result, ProtestMarker) {
		t.Errorf("a report accepted under protest is indistinguishable from a clean one: %q", result)
	}
	// The unresolved violation has to be named, not just flagged — the marker
	// is what a saved transcript is grepped for, the text is what says which
	// finding to go and look at.
	if !strings.Contains(result, "PersistentVolumeClaim") {
		t.Errorf("the acknowledgement does not say what was left unresolved: %q", result)
	}

	// And the counting seam sees it. This is the half of the contract with
	// internal/evals that lives here: evals matches through Protest and tests
	// that it counts what Protest returns, so what this package owes it is that
	// the give-up is something Protest recognises. Without this assertion the
	// counter could be watching for a string the tool no longer writes and
	// would report "none" forever — the exact failure it exists to prevent, one
	// layer up.
	violations, protested := Protest(result)
	if !protested {
		t.Fatalf("Protest does not recognise the give-up acknowledgement: %q", result)
	}
	if !strings.Contains(violations, "PersistentVolumeClaim") {
		t.Errorf("Protest returned %q, which does not name the unresolved violation", violations)
	}
	if strings.Contains(violations, ProtestMarker) {
		t.Errorf("Protest returned the marker along with the violations: %q", violations)
	}

	// And a valid report on a fresh run is still acknowledged plainly, so the
	// marker never appears where nothing went wrong.
	clean := runReportTool(t, map[string]any{
		"overall_severity": "ok", "summary": "nothing to report", "findings": []any{},
	})
	if got, _ := clean["result"].(string); strings.Contains(got, ProtestMarker) {
		t.Errorf("a clean report was marked as accepted under protest: %q", got)
	}
}

func runReportTool(t *testing.T, args map[string]any) map[string]any {
	t.Helper()
	rt, ok := reportTool().(*submitReport)
	if !ok {
		t.Fatalf("reportTool is not a *submitReport")
	}
	out, err := rt.Run(nil, args)
	if err != nil {
		t.Fatalf("Run returned a Go error rather than a model-readable one: %v", err)
	}
	return out
}

func keys(m map[string]*genai.FunctionDeclaration) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// recordingModel captures the tool declarations ADK attaches to a request and
// then yields nothing, ending the turn.
type recordingModel struct {
	mu    sync.Mutex
	tools map[string]*genai.FunctionDeclaration
}

func (*recordingModel) Name() string { return "recording" }

func (m *recordingModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.mu.Lock()
	if m.tools == nil {
		m.tools = map[string]*genai.FunctionDeclaration{}
	}
	if req.Config != nil {
		for _, t := range req.Config.Tools {
			for _, fd := range t.FunctionDeclarations {
				m.tools[fd.Name] = fd
			}
		}
	}
	m.mu.Unlock()
	return func(yield func(*model.LLMResponse, error) bool) {}
}

func (m *recordingModel) declared() map[string]*genai.FunctionDeclaration {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]*genai.FunctionDeclaration, len(m.tools))
	for k, v := range m.tools {
		out[k] = v
	}
	return out
}
