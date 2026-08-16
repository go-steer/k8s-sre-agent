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

package bounded

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/k8s-sre-agent/internal/schema"
	"github.com/go-steer/k8s-sre-agent/internal/sre"
)

// stamp keeps the prompt deterministic.
var stamp = time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC)

// recordingModel captures the request and replies with a scripted response.
type recordingModel struct {
	name string
	got  *adkmodel.LLMRequest
	resp *adkmodel.LLMResponse
	err  error
}

func (m *recordingModel) Name() string { return m.name }

func (m *recordingModel) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	m.got = req
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if m.err != nil {
			yield(nil, m.err)
			return
		}
		yield(m.resp, nil)
	}
}

func callResponse(args map[string]any) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{Name: ReportToolName, Args: args}},
		}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount: 4200, CandidatesTokenCount: 380,
		},
		ModelVersion: "claude-haiku-4-5-20251001",
	}
}

func validReport() map[string]any {
	return map[string]any{
		"overall_severity": "critical",
		"summary":          "checkout-api cannot pull its image",
		"findings": []any{map[string]any{
			"severity": "critical", "title": "ImagePullBackOff", "detail": "tag does not resolve",
			"namespace": "shop", "kind": "Deployment", "resource_name": "checkout-api",
			"reason": "ImagePullBackOff",
		}},
	}
}

// The load-bearing assertion of the whole package, and the one this repo has
// twice shipped without.
//
// ADK's Chat-mode OutputSchema is gated on IsGeminiModel, so on an Anthropic
// model nothing is injected, the native ResponseSchema is ignored by the
// provider, and the agent answers in prose — a tier-1 run scored
// severity_accuracy 0.000 that way, with thirty well-argued markdown reports.
// Forced tool use is a different mechanism and does reach the API, but only in
// one spelling: mode ANY with exactly *one* allowed name, which mast's
// Anthropic provider turns into tool_choice {type: tool, name}. Two names, or
// mode AUTO, and the constraint silently becomes a suggestion.
//
// So this asserts the wire form rather than the outcome. The outcome is a
// prose answer that scores zero, which is precisely the failure that does not
// announce itself.
func TestTheReportToolIsForcedOnTheWire(t *testing.T) {
	req := Request(Snapshot{Health: "health.category status=degraded"}, []string{"shop"}, stamp)

	fcc := req.Config.ToolConfig.FunctionCallingConfig
	if fcc.Mode != genai.FunctionCallingConfigModeAny {
		t.Errorf("FunctionCallingConfig.Mode = %v, want ANY — any other mode leaves the model "+
			"free to answer in prose, which nothing downstream detects", fcc.Mode)
	}
	if len(fcc.AllowedFunctionNames) != 1 || fcc.AllowedFunctionNames[0] != ReportToolName {
		t.Errorf("AllowedFunctionNames = %v, want exactly [%s]: mast's provider only pins a "+
			"specific tool when there is exactly one name, and maps anything else to a bare "+
			"\"any\"", fcc.AllowedFunctionNames, ReportToolName)
	}

	// Exactly one tool is offered. A second would give the forced call
	// somewhere else to go and would also make this a one-step agent.
	var names []string
	for _, tl := range req.Config.Tools {
		for _, d := range tl.FunctionDeclarations {
			names = append(names, d.Name)
		}
	}
	if len(names) != 1 || names[0] != ReportToolName {
		t.Fatalf("tools declared = %v, want only %s", names, ReportToolName)
	}

	// And it carries the shared contract, not a private copy of it. Two
	// producers with two schemas make every switch between them look like the
	// whole fault set changing.
	decl := req.Config.Tools[0].FunctionDeclarations[0]
	want, got := schema.ReportSchema(), decl.Parameters
	wj, _ := json.Marshal(want)
	gj, _ := json.Marshal(got)
	if string(wj) != string(gj) {
		t.Errorf("the report tool does not carry schema.ReportSchema()")
	}
}

func TestCheckProducesAReportFromASnapshot(t *testing.T) {
	m := &recordingModel{name: "haiku", resp: callResponse(validReport())}
	report, resp, err := Analyse(context.Background(), m,
		Snapshot{Health: "health.category category=crashloops status=degraded", Delta: "pod broken"},
		[]string{"shop"}, stamp)
	if err != nil {
		t.Fatal(err)
	}
	if report.OverallSeverity != schema.OverallCritical || len(report.Findings) != 1 {
		t.Errorf("report = %+v", report)
	}
	if resp == nil {
		t.Fatal("no response kept, so the call cannot be priced")
	}

	// Both scans reach the prompt, labelled. A snapshot that arrives unlabelled
	// is two blobs the model has to guess the provenance of.
	user := m.got.Contents[0].Parts[0].Text
	for _, want := range []string{"lookout health", "triage delta", "crashloops", "pod broken", "shop"} {
		if !strings.Contains(user, want) {
			t.Errorf("the prompt does not mention %q:\n%s", want, user)
		}
	}
}

// "Not run" and "clean" are different answers and the prompt has to say which.
// lookout's own contract makes the distinction — a result without a summary
// line is void, findings=0 with a summary is scanned-and-healthy — and a blank
// section here would collapse them into the ambiguous silence the read path is
// built to avoid.
func TestAnEmptyScanIsNotRenderedAsClean(t *testing.T) {
	got := userPrompt(Snapshot{Health: "", Delta: "some abnormal object"}, nil, stamp)
	if !strings.Contains(got, "not run, not as clean") {
		t.Errorf("an empty scan section reads as clean:\n%s", got)
	}
}

// A collection failure travels into the prompt rather than aborting, and it
// travels *first*. A model that reads it last has already formed a view from
// partial data — which is how the first tier-3 run came to hedge correctly only
// because every single tool had failed loudly.
func TestCollectionFailuresReachTheModelBeforeTheData(t *testing.T) {
	snap := Snapshot{
		Health: "health.category status=healthy",
		Errors: []string{"lookout triage delta: exit status 1"},
	}
	got := userPrompt(snap, []string{"shop"}, stamp)
	if !strings.Contains(got, "exit status 1") {
		t.Fatalf("the failure is not in the prompt:\n%s", got)
	}
	if strings.Index(got, "exit status 1") > strings.Index(got, "health scorecard") {
		t.Error("the failure is reported after the data it qualifies")
	}
	if !strings.Contains(got, "Do not certify") {
		t.Error("the prompt does not tell the model what a failed scan means for its report")
	}
}

// A prose answer must be an error, never a fallback to scraping the text. Prose
// is what the forced call exists to prevent, and accepting it would reintroduce
// the regex-parsing the schema replaced — silently, on whichever model version
// stopped honouring tool_choice.
func TestAProseAnswerIsAnError(t *testing.T) {
	m := &recordingModel{name: "haiku", resp: &adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
			{Text: "The cluster looks broadly healthy, though checkout-api is failing to pull."},
		}},
	}}
	_, _, err := Analyse(context.Background(), m, Snapshot{Health: "x"}, nil, stamp)
	if err == nil {
		t.Fatal("a prose answer was accepted")
	}
	if !strings.Contains(err.Error(), "tool_choice") {
		t.Errorf("the error does not point at the mechanism that must have failed: %v", err)
	}
}

// A structurally invalid report is a hard error: it cannot be fingerprinted, so
// there is nothing to emit and nothing for the next cycle to diff against.
func TestAStructurallyInvalidReportIsAnError(t *testing.T) {
	bad := validReport()
	bad["overall_severity"] = "extremely bad"
	m := &recordingModel{name: "haiku", resp: callResponse(bad)}
	if _, _, err := Analyse(context.Background(), m, Snapshot{Health: "x"}, nil, stamp); err == nil {
		t.Fatal("an unfingerprintable report was returned as though it were usable")
	}
}

// The rest of the contract is not a hard error, and the split is the whole
// design. The agent's report tool hands a violation back and the model fixes it
// with the evidence still in context; this pass has one turn, so failing the
// cycle would mean no report and therefore no diff — which costs more than one
// imperfect finding. It emits and records, which is maxRejections' trade taken
// immediately because there is nothing to retry.
//
// Both producers are held to sre.ValidateReport, and that is the point: a
// producer allowed to file findings the other would reject makes every switch
// between them look like the whole fault set changing.
func TestAContractViolationIsEmittedUnderProtestRatherThanLost(t *testing.T) {
	bad := validReport()
	// overall_severity disagreeing with its own findings — the consistency rule
	// the agent's tool rejects. Structurally valid, so it survives Analyse.
	bad["overall_severity"] = "warning"
	m := &recordingModel{name: "haiku", resp: callResponse(bad)}

	report, _, err := Analyse(context.Background(), m, Snapshot{Health: "x"}, nil, stamp)
	if err != nil {
		t.Fatalf("a contract violation aborted the pass: %v", err)
	}
	problems := sre.ValidateReport(report)
	if len(problems) == 0 {
		t.Fatal("sre.ValidateReport accepted a report the agent's tool would reject; the two " +
			"producers are no longer held to one contract")
	}
	if !strings.Contains(strings.Join(problems, "; "), "overall_severity") {
		t.Errorf("problems = %v, want the consistency violation", problems)
	}

	// And an identity violation reaches it too — this is the check that has
	// never fired against a live model, and the bounded pass is a second place
	// it can.
	borrowed := validReport()
	borrowed["findings"] = []any{map[string]any{
		"severity": "critical", "title": "t", "detail": "d", "namespace": "shop",
		"kind": "Service", "resource_name": "session-store", "reason": "PodsNotReady",
	}}
	var h schema.HealthReport
	blob, _ := json.Marshal(borrowed)
	if err := json.Unmarshal(blob, &h); err != nil {
		t.Fatal(err)
	}
	if len(sre.ValidateReport(&h)) == 0 {
		t.Error("the layer check does not reach reports from this producer")
	}
}

// Usage has to be attributable or the whole point — publishing this path's cost
// as a delta against the agent's — does not work. An event with no usage writes
// nothing rather than a zero, for the reason evals.Usage documents: "not
// measured" and "free" must not render alike.
func TestUsageIsAttributedAndAbsenceIsNotZero(t *testing.T) {
	r := Result{Response: callResponse(validReport())}
	ev := r.Event()
	if ev == nil {
		t.Fatal("no event, so the call cannot be priced")
	}
	if ev.Author != AgentName {
		t.Errorf("Author = %q, want %q — an unattributed cost lands under \"unknown\"", ev.Author, AgentName)
	}
	if ev.ModelVersion == "" || ev.UsageMetadata == nil {
		t.Error("the event carries no model or usage")
	}

	unmeasured := Result{Response: &adkmodel.LLMResponse{}}
	if unmeasured.Event() != nil {
		t.Error("a response with no usage produced an event, which records it as free")
	}
}

// The scope flags are lookout's, and more than one namespace has to widen to -A
// rather than silently scanning the first. lookout takes one --namespace and
// has no repeatable form.
func TestScopeArgs(t *testing.T) {
	for _, tc := range []struct {
		ns   []string
		want string
	}{
		{nil, "-A"},
		{[]string{"shop"}, "--namespace=shop"},
		{[]string{"shop", "prod"}, "-A"},
	} {
		if got := strings.Join(scopeArgs(tc.ns), " "); got != tc.want {
			t.Errorf("scopeArgs(%v) = %q, want %q", tc.ns, got, tc.want)
		}
	}
}

// Nothing gathers more evidence than the two scans, and that is a property
// worth pinning rather than a detail. The moment this grows an enumeration call
// it can start reasoning about absences, and then it is a small agent with a
// hidden step count instead of a bounded pass — see the package comment on
// fault-badselector, which is the fixture that should keep failing.
func TestTheStepCountIsFixed(t *testing.T) {
	m := &recordingModel{name: "haiku", resp: callResponse(validReport())}
	if _, _, err := Analyse(context.Background(), m, Snapshot{Health: "x"}, nil, stamp); err != nil {
		t.Fatal(err)
	}
	// One request, one user turn, no history to grow.
	if n := len(m.got.Contents); n != 1 {
		t.Errorf("the request carries %d contents; a bounded pass has exactly one turn", n)
	}
	if m.got.Contents[0].Role != genai.RoleUser {
		t.Errorf("role = %q, want user", m.got.Contents[0].Role)
	}
}

// The scorecard is context, not findings. `lookout health` answers
// healthy|degraded|unavailable per category, which is what makes a clean
// cluster legible — but a category has no object, so a finding filed from one
// has no kind and no resource_name and the report is accepted under protest.
//
// Measured, not predicted: the first switchboard end-to-end run against a kind
// cluster hit exactly this. The control-plane category answers `unavailable —
// requires cloud provider metrics; no cloud provider configured`, the model
// reported it as a finding, and ProtestMarker fired for the first time in this
// repo's history.
func TestThePromptSaysTheScorecardIsNotFindings(t *testing.T) {
	got := userPrompt(Snapshot{
		Health: "kind=health.category severity=info reason=Unavailable category=control-plane status=unavailable",
	}, []string{"shop"}, stamp)

	if !strings.Contains(got, "not an object") {
		t.Errorf("the prompt does not say a category is not an object:\n%s", got)
	}
	if !strings.Contains(got, "do not file a finding about a category") {
		t.Errorf("the prompt does not tell the model what not to do with the scorecard:\n%s", got)
	}
	// And it must still say what `unavailable` means, or the model either
	// ignores an un-run check or reports it as a fault — the two failures the
	// explicit-healthy contract exists to separate.
	if !strings.Contains(got, "could not run") {
		t.Errorf("the prompt does not explain an unavailable category:\n%s", got)
	}
}
