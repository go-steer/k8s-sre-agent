package sre

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mastagent "github.com/go-steer/mast/pkg/agent"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-sre-agent/internal/lookout"
	"github.com/go-steer/core-sre-agent/internal/schema"
)

// The behaviour the whole thing is for: a specialist that stops without
// reporting costs its own section of the answer and nothing else.
//
// The script is the tier-2 failure verbatim — the specialist runs out of moves
// and asks the user to run a command for it — with the orchestrator scripted to
// do what it did on that run: delegate, and then have enough to report anyway.
func TestASilentSpecialistDoesNotKillTheRun(t *testing.T) {
	orch := &scriptedModel{name: "orch", turns: [][]*genai.Part{
		{{FunctionCall: &genai.FunctionCall{ID: "d1", Name: "pod-inspector", Args: map[string]any{}}}},
		{{FunctionCall: &genai.FunctionCall{ID: "r1", Name: ReportToolName, Args: map[string]any{
			"overall_severity": "ok",
			"summary":          "OK: nothing actionable; pod-inspector did not complete.",
			"findings":         []any{},
		}}}},
		{{Text: "done"}},
	}}
	sub := &scriptedModel{name: "sub", turns: [][]*genai.Part{
		{{Text: "Could you run `kubectl get all -n shop` and paste the output?"}},
	}}

	orchHist := runDelegation(t, "sre-stall-survives", orch, sub)

	if len(orchHist) < 2 {
		t.Fatalf("the orchestrator was called %d time(s): it never regained control after "+
			"the specialist stalled, which is the failure the stall guard exists to remove", len(orchHist))
	}
	resp := findFunctionResponse(orchHist[1], "pod-inspector")
	if resp == nil {
		t.Fatalf("the orchestrator's second turn carries no answer from pod-inspector:\n%s",
			renderHistory(orchHist[1]))
	}
	summary, _ := resp.Response["summary"].(string)
	if !mastagent.Stalled(summary) {
		t.Errorf("the delegation was closed without marking it incomplete; summary was %q", summary)
	}
	if !strings.Contains(summary, "kubectl get all -n shop") {
		t.Errorf("the specialist's own last words were dropped; they name the data it could "+
			"not get, which is the useful part. summary was %q", summary)
	}
	if f, ok := resp.Response["findings"].([]any); !ok || len(f) != 0 {
		t.Errorf("a stalled specialist reported findings it never made: %v", resp.Response["findings"])
	}
}

// The defect, reproduced against the same script with the callback removed.
//
// This is the counterpart to TestTransferFromASpecialistIsFatal: it documents
// what the guard buys and will start failing if ADK ever synthesises a
// delegation-closing response of its own, at which point the guard becomes a
// choice about the *content* of that response rather than a fix.
func TestWithoutTheGuardASilentSpecialistEndsTheRun(t *testing.T) {
	orch := &scriptedModel{name: "orch", turns: [][]*genai.Part{
		{{FunctionCall: &genai.FunctionCall{ID: "d1", Name: "pod-inspector", Args: map[string]any{}}}},
		{{FunctionCall: &genai.FunctionCall{ID: "r1", Name: ReportToolName, Args: map[string]any{
			"overall_severity": "ok",
			"summary":          "OK: nothing actionable.",
			"findings":         []any{},
		}}}},
	}}
	sub := &scriptedModel{name: "sub", turns: [][]*genai.Part{
		{{Text: "Could you run `kubectl get all -n shop` and paste the output?"}},
	}}

	offline, _, err := lookout.Offline()
	if err != nil {
		t.Fatal(err)
	}
	root, err := buildUnguarded(orch, sub, offline)
	if err != nil {
		t.Fatal(err)
	}
	drain(t, "sre-stall-unguarded", root)

	if n := len(orch.histories()); n != 1 {
		t.Fatalf("the orchestrator ran %d turns without the guard; ADK may now close an "+
			"unfinished delegation itself — re-read mastagent.FinishOnStall before deleting it", n)
	}
}

// The callback's own mechanics — that a turn carrying any function call is left
// alone, that a partial, interrupted, errored or failed response is not a
// stall, and that the rewrite is additive so a thinking block survives with its
// signature — are mast's now and are pinned by mast's own tests
// (TestFinishOnStallLeavesLiveTurnsAlone, TestFinishOnStallKeepsTheOriginalParts).
// Keeping a second copy here would fail in two repos for one cause and would
// drift the day mast changed. What stays is the wiring above and the payload
// below, which is the half mast deliberately does not own.

// The synthetic report must satisfy the specialist's OutputSchema, because that
// schema is finish_task's parameter schema: FinishTaskTool validates the args
// and hands back a retryable error rather than a success response if they do
// not fit, which would leave the run in exactly the state this prevents.
func TestTheStallReportSatisfiesTheContract(t *testing.T) {
	blob, err := json.Marshal(stallReport("pod-inspector", "Which deployment?"))
	if err != nil {
		t.Fatal(err)
	}
	var h schema.HealthReport
	if err := json.Unmarshal(blob, &h); err != nil {
		t.Fatalf("the stall report does not decode as a HealthReport: %v", err)
	}
	if err := h.Validate(); err != nil {
		t.Fatalf("the stall report violates the schema: %v", err)
	}
	if problems := validate(&h); len(problems) > 0 {
		t.Fatalf("the stall report would be rejected by the report tool: %v", problems)
	}
	// "ok" is forced: the schema rejects any other severity with no findings.
	// It is also the field's weakest possible reading — "this report contains
	// no findings" — and read alone it looks like a clean bill of health, which
	// is why the marker has to lead the summary rather than sit inside it.
	if h.OverallSeverity != schema.OverallOK {
		t.Errorf("overall_severity is %q; the schema requires %q with no findings",
			h.OverallSeverity, schema.OverallOK)
	}
	if !mastagent.Stalled(h.Summary) {
		t.Errorf("the summary does not open with the marker, so an 'ok' report from a "+
			"stalled specialist reads as a healthy one: %q", h.Summary)
	}
	// A finding is a cluster fault: internal/monitor fingerprints it and tracks
	// it across cycles. "A subagent stopped talking" is not one.
	if len(h.Findings) != 0 {
		t.Errorf("the stall report invented %d finding(s)", len(h.Findings))
	}
	if !strings.Contains(h.Summary, "pod-inspector") {
		t.Errorf("the summary does not name the specialist that stalled: %q", h.Summary)
	}
}

// runDelegation drives the real Build() wiring through one delegation and
// returns the histories the orchestrator's model was handed.
func runDelegation(t *testing.T, app string, orch, sub adkmodel.LLM) [][]*genai.Content {
	t.Helper()
	offline, _, err := lookout.Offline()
	if err != nil {
		t.Fatal(err)
	}
	root, err := Build(Config{Main: orch, Subagent: sub, Toolsets: []tool.Toolset{offline}})
	if err != nil {
		t.Fatal(err)
	}
	drain(t, app, root)
	recorder, ok := orch.(*scriptedModel)
	if !ok {
		t.Fatalf("runDelegation needs a scriptedModel orchestrator, got %T", orch)
	}
	return recorder.histories()
}

func drain(t *testing.T, app string, root adkagent.Agent) {
	t.Helper()
	rn, err := runner.NewInMemory(app, root)
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range rn.Run(context.Background(), "u", "s",
		genai.NewContentFromText("assess the shop namespace", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
}

// buildUnguarded assembles buildOne's shape minus the stall guard. Written out here
// rather than exposed as a knob on Config, so the only way to get an unguarded
// specialist is to ask for one in a test.
func buildUnguarded(orch, sub adkmodel.LLM, offline tool.Toolset) (adkagent.Agent, error) {
	specialist, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name:                     "pod-inspector",
		Description:              "inspects pods",
		Instruction:              "Inspect pods.",
		Model:                    sub,
		Toolsets:                 []tool.Toolset{offline},
		OutputSchema:             schema.ReportSchema(),
		DisallowTransferToParent: true,
		DisallowTransferToPeers:  true,
	})
	if err != nil {
		return nil, err
	}
	return llmagent.New(llmagent.Config{
		Name:        OrchestratorName,
		Description: "coordinates",
		Instruction: "Coordinate.",
		Model:       orch,
		Tools:       []tool.Tool{reportTool()},
		Toolsets:    []tool.Toolset{offline},
		SubAgents:   []adkagent.Agent{specialist},
		Mode:        llmagent.ModeChat,
	})
}

// findFunctionResponse returns the response to a named call in a history.
func findFunctionResponse(contents []*genai.Content, name string) *genai.FunctionResponse {
	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == name {
				return p.FunctionResponse
			}
		}
	}
	return nil
}
