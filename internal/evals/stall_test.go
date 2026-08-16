package evals

import (
	"strings"
	"testing"

	mastagent "github.com/go-steer/mast/pkg/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-sre-agent/internal/sre"
)

// stallSummary is the summary the stall guard writes. Only the marker is
// shared — the rest of the sentence is mast's business, and matching on more of
// it here would make a prose edit upstream fail a test in a package that does
// not care.
const stallSummary = mastagent.StallMarker + " (pod-inspector) This specialist ended its turn " +
	"without reporting.\n\nCould you run `kubectl get all -n shop` and paste the output?"

func responseEvent(name string, resp map[string]any) *session.Event {
	return &session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{
			{FunctionResponse: &genai.FunctionResponse{Name: name, Response: resp}},
		}}}}
}

func callEvent(name string, args map[string]any) *session.Event {
	return &session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{Name: name, Args: args}},
		}}}}
}

func TestAStalledSpecialistIsCounted(t *testing.T) {
	roster := map[string]bool{"pod-inspector": true, "log-analyzer": true}

	stall := responseEvent("pod-inspector", map[string]any{
		"overall_severity": "ok",
		"summary":          stallSummary,
		"findings":         []any{},
	})
	got := stalledDelegations(stall, roster)
	if len(got) != 1 {
		t.Fatalf("stalledDelegations = %v, want one entry", got)
	}
	if StallName(got[0]) != "pod-inspector" {
		t.Errorf("StallName(%q) = %q, want pod-inspector", got[0], StallName(got[0]))
	}
	// The specialist's own words survive into the transcript. They are the only
	// diagnosis of the stall: the question names the data it could not get.
	if !strings.Contains(got[0], "kubectl get all -n shop") {
		t.Errorf("the entry dropped what the specialist said: %q", got[0])
	}
	if strings.Contains(got[0], mastagent.StallMarker) {
		t.Errorf("the entry repeats the marker instead of the words: %q", got[0])
	}

	// A specialist can stall having said nothing at all, and the entry then
	// degrades to the bare name rather than to a dangling separator.
	silent := responseEvent("pod-inspector", map[string]any{
		"overall_severity": "ok",
		"summary":          mastagent.StallMarker + " (pod-inspector) It stopped.",
		"findings":         []any{},
	})
	if got := stalledDelegations(silent, roster); len(got) != 1 || got[0] != "pod-inspector" {
		t.Errorf("stalledDelegations = %v, want [pod-inspector]", got)
	}

	// A specialist that actually reported is not a stall, even when it found
	// nothing — "checked and clean" and "not checked" are the distinction the
	// whole marker exists to carry.
	clean := responseEvent("log-analyzer", map[string]any{
		"overall_severity": "ok",
		"summary":          "No errors in the last hour across all three replicas.",
		"findings":         []any{},
	})
	if got := stalledDelegations(clean, roster); len(got) != 0 {
		t.Errorf("a real clean report counted as a stall: %v", got)
	}

	// An agent that is not on the roster is a tool, and a tool result that
	// happens to contain the marker text is not a delegation outcome.
	imposter := responseEvent("k8s_cluster_health", map[string]any{"summary": stallSummary})
	if got := stalledDelegations(imposter, roster); len(got) != 0 {
		t.Errorf("a tool response counted as a stall: %v", got)
	}
}

// The counters have to partition the outcomes, not overlap: a stall is not an
// error, and reporting it as both would double-count one delegation in a
// summary that is read as "how did the fan-out go".
func TestAStallIsNotAlsoADelegationError(t *testing.T) {
	roster := map[string]bool{"pod-inspector": true}
	stall := responseEvent("pod-inspector", map[string]any{
		"overall_severity": "ok",
		"summary":          stallSummary,
		"findings":         []any{},
	})
	if got := failedDelegations(stall, roster); len(got) != 0 {
		t.Errorf("failedDelegations = %v, want none — a stall resolved cleanly", got)
	}

	broken := responseEvent("pod-inspector", map[string]any{"error": "transfer refused"})
	if got := stalledDelegations(broken, roster); len(got) != 0 {
		t.Errorf("stalledDelegations = %v, want none — an error is not a stall", got)
	}
	if got := failedDelegations(broken, roster); len(got) != 1 {
		t.Errorf("failedDelegations = %v, want the one error", got)
	}
}

// The report a stalled specialist did not write is a schema-valid, entirely
// unalarming HealthReport: severity "ok", no findings. Crediting it as the
// run's answer would score a run that produced no diagnosis at all as one that
// swept the cluster and found it healthy — the most flattering possible reading
// of the exact failure the stall guard exists to surface.
func TestAStallReportIsNotTheRunsHealthReport(t *testing.T) {
	stall := callEvent("finish_task", map[string]any{
		"overall_severity": "ok",
		"summary":          stallSummary,
		"findings":         []any{},
	})
	if h, rank := extractReport(stall); h != nil {
		t.Fatalf("extractReport credited a stall report (rank %d): %+v", rank, h)
	}

	// A specialist's genuine sub-report still comes through: it is outranked by
	// the orchestrator's, not discarded, and on a run where the orchestrator
	// never submits it is the only evidence of what was found.
	real := callEvent("finish_task", map[string]any{
		"overall_severity": "warning",
		"summary":          "Two replicas are Pending on insufficient memory.",
		"findings":         []any{},
	})
	if h, rank := extractReport(real); h == nil || rank != 1 {
		t.Errorf("extractReport = %v (rank %d), want the specialist's report", h, rank)
	}
}

// Only the synthetic carrier is filtered. An orchestrator that does what the
// stall instruction asks — say plainly which checks were not completed — will
// quote the marker in its own summary, and that report is the real answer.
func TestTheOrchestratorMayQuoteTheMarker(t *testing.T) {
	quoted := callEvent("submit_health_report", map[string]any{
		"overall_severity": "warning",
		"summary": "The cache deployment is degraded. " + mastagent.StallMarker +
			" — pod-inspector did not report, so pod-level checks are missing.",
		"findings": []any{},
	})
	h, rank := extractReport(quoted)
	if h == nil {
		t.Fatalf("extractReport dropped an orchestrator report that quotes the marker")
	}
	if rank != 2 {
		t.Errorf("rank = %d, want 2", rank)
	}
	if !strings.Contains(h.Summary, mastagent.StallMarker) {
		t.Errorf("summary lost the marker: %q", h.Summary)
	}
}

// The harness matches the marker at the head of the summary, and sre writes it
// there. The two packages agree through the exported constant; this pins the
// position, which is the part a prose edit in sre could quietly break.
func TestTheMarkerLeadsTheSummary(t *testing.T) {
	if !isStallReport(stallSummary) {
		t.Fatalf("isStallReport(%q) = false", stallSummary)
	}
	if isStallReport("Everything is fine. " + mastagent.StallMarker) {
		t.Error("a summary that merely mentions the marker was read as a stall")
	}
	// Leading whitespace is not a difference worth failing over — the model
	// never writes this string, sre does, but a formatting change should not
	// silently disarm the counter.
	if !isStallReport("\n  " + stallSummary) {
		t.Error("leading whitespace defeated the marker match")
	}
}

// The protest counter, at the other end of the run from the stall counter and
// for the same reason: maxRejections is a bound the *model* hit, and a run that
// gave up on the contract must not read like one that satisfied it.
//
// The value asserted here is sre.Protest's, not a literal — the marker's
// wording is sre's business, and matching more of it in this package would make
// a prose edit there fail a test here.
func TestAProtestedSubmissionIsCounted(t *testing.T) {
	violations := `finding 1 ("Session store unreachable"): reason "PodsNotReady" is a ` +
		`statement about a Pod, not about this object, but this finding names a Service.`
	protest := responseEvent("submit_health_report", map[string]any{
		"result": sre.ProtestMarker + " " + violations + " You are done.",
	})
	got := protestedSubmissions(protest)
	if len(got) != 1 {
		t.Fatalf("protestedSubmissions = %v, want one entry", got)
	}
	// The violations are the diagnosis: they name the check that could not be
	// satisfied in three tries, which is the only thing that makes a protest
	// actionable rather than just alarming.
	if !strings.Contains(got[0], "PodsNotReady") {
		t.Errorf("the entry dropped the violations, which are the whole point: %q", got[0])
	}
	if strings.Contains(got[0], sre.ProtestMarker) {
		t.Errorf("the entry repeats the marker instead of the violations: %q", got[0])
	}

	// A report accepted on the merits is not a protest, and this is the case
	// that matters: the protest travels on the success channel, so anything not
	// looking for the marker sees an ordinary acknowledgement.
	clean := responseEvent("submit_health_report", map[string]any{
		"result": "Health report recorded. You are done.",
	})
	if got := protestedSubmissions(clean); len(got) != 0 {
		t.Errorf("a clean acknowledgement counted as a protest: %v", got)
	}

	// A rejection the model can still act on is not a protest either — it is
	// the check working. Only the give-up is counted, or the line would report
	// every run in which the contract did its job.
	rejected := responseEvent("submit_health_report", map[string]any{
		"error": "the report violates the contract: " + violations + " Fix and call it again.",
	})
	if got := protestedSubmissions(rejected); len(got) != 0 {
		t.Errorf("an ordinary retryable rejection counted as a protest: %v", got)
	}

	// An orchestrator that quotes the marker inside its own prose has still had
	// its report accepted on the merits. sre.Protest matches at the head.
	quoted := responseEvent("submit_health_report", map[string]any{
		"result": "Health report recorded, and note " + sre.ProtestMarker + " was not needed.",
	})
	if got := protestedSubmissions(quoted); len(got) != 0 {
		t.Errorf("a summary that merely mentions the marker was read as a protest: %v", got)
	}
}
