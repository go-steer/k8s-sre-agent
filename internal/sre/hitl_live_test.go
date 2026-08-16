package sre

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	mastagent "github.com/go-steer/mast/pkg/agent"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/core-sre-agent/internal/llm"
	"github.com/go-steer/core-sre-agent/internal/schema"
)

// The HITL spike.
//
// Every write this agent will ever perform is supposed to be gated on a human
// approval that the *runtime* enforces, not one the prompt asks for politely.
// ADK v2 implements that as a session event: a tool calls
// ctx.RequestConfirmation, the turn ends with an adk_request_confirmation
// FunctionCall listed in Event.LongRunningToolIDs, and the caller resumes by
// replying with a matching FunctionResponse.
//
// Whether that survives *our* agent shape was an open question, and it is the
// one thing worth answering before any write tool is built on top of it. The
// write tool does not sit on the root: it sits inside change-executor, a
// Task-mode sub-agent of the Chat orchestrator, because the read/write split is
// structural. So the interrupt has to cross a Task-agent boundary on the way
// out and the approval has to cross it again on the way back in, while leaving
// the orchestrator's structured HealthReport intact.
//
// The first version of this spike wired change-executor through agenttool and
// never reached the tool at all — agenttool runs the sub-agent under its own
// runner, which rejects a Task root. That failure is what turned up the
// delegation defect this package's Build comment now documents; the wiring
// here matches the fixed Build.
//
// mast's HITL findings were validated against ADK v2.1.0. This runs against
// whatever is in go.mod (v2.2.0 at the time of writing).
//
// Gated on a live model because the flow only exists if a model decides to call
// the tool; there is no way to provoke a real confirmation event offline.
//
//	source ~/scripts/claude-env.sh
//	SRE_LIVE_MODEL=1 go test ./internal/sre/ -run TestHITL -v

const envLiveModel = "SRE_LIVE_MODEL"

// writeRecorder records whether the gated tool actually executed. The whole
// point of the gate is that a denied call leaves this empty.
type writeRecorder struct {
	mu    sync.Mutex
	calls []scaleArgs
}

func (w *writeRecorder) add(a scaleArgs) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, a)
}

func (w *writeRecorder) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.calls)
}

type scaleArgs struct {
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
	Replicas   int    `json:"replicas"`
}

type scaleResult struct {
	Status string `json:"status"`
}

// buildGatedAgent assembles the production nesting around one confirmed write
// tool: Chat orchestrator -> Task change-executor sub-agent -> gated tool.
//
// It deliberately uses the same primitives Build does (llmagent.New in Chat
// mode with Task sub-agents from NewTaskAgent) rather than loading the real
// specs. The specs are prose and would make this a test of the model's
// instruction-following; the nesting is the thing under test.
func buildGatedAgent(t *testing.T, rec *writeRecorder) adkagent.Agent {
	t.Helper()

	ctx := context.Background()
	_, sub, err := llm.Models(ctx)
	if err != nil {
		t.Fatalf("resolve models: %v", err)
	}

	scale, err := functiontool.New(functiontool.Config{
		Name: "k8s_scale_deployment",
		Description: "Scale a Kubernetes deployment to a given replica count. " +
			"This mutates the cluster.",
		RequireConfirmation: true,
	}, func(_ adkagent.Context, a scaleArgs) (scaleResult, error) {
		rec.add(a)
		return scaleResult{Status: "scaled"}, nil
	})
	if err != nil {
		t.Fatalf("build write tool: %v", err)
	}

	executor, err := mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
		Name:        "change-executor",
		Description: "Applies approved changes to the cluster.",
		// The finish_task clause is not boilerplate. A Task sub-agent that ends
		// its turn with prose leaves the delegation unresolved, and ADK ends the
		// caller's turn rather than synthesising a response — so the
		// orchestrator never regains control and never emits a report. Every
		// real spec in specs/ carries the same instruction for the same reason.
		// The denial half matters equally: a rejected write is an outcome to
		// report, not a reason to stop and ask.
		Instruction: "You apply cluster changes. When asked to scale a deployment, " +
			"call k8s_scale_deployment with the namespace, deployment and replica " +
			"count you were given. Then finish with finish_task, reporting what you " +
			"did. If the write was rejected or not approved, that is a normal " +
			"outcome: report it with finish_task. Never end your turn with a " +
			"question — nobody will answer it.",
		Model:        sub,
		Tools:        []tool.Tool{scale},
		OutputSchema: schema.ReportSchema(),
	})
	if err != nil {
		t.Fatalf("build change-executor: %v", err)
	}

	orchestrator, err := llmagent.New(llmagent.Config{
		Name:        OrchestratorName,
		Description: "SRE orchestrator.",
		Instruction: "You are an SRE orchestrator. You hold no write tools yourself. " +
			"To change the cluster you must delegate to the change-executor agent, " +
			"passing it the namespace, deployment and replica count. " +
			"When it reports back, finish by calling submit_health_report with the " +
			"outcome. That call is your answer; prose is discarded.",
		Model: sub,
		// The real carrier, not llmagent.Config.OutputSchema — which is inert
		// on an Anthropic model (see ReportToolName). Using it here would have
		// let this spike pass on the strength of the sub-agent's finish_task
		// while the orchestrator's own report was prose.
		Tools:     []tool.Tool{reportTool()},
		SubAgents: []adkagent.Agent{executor},
		Mode:      llmagent.ModeChat,
	})
	if err != nil {
		t.Fatalf("build orchestrator: %v", err)
	}
	return orchestrator
}

// pendingConfirmation is one interrupt waiting on a human.
type pendingConfirmation struct {
	id   string
	name string
	hint string
}

// collectConfirmations finds confirmation interrupts in a turn's events.
//
// The discriminator is membership in Event.LongRunningToolIDs, not the function
// name alone: an ordinary FunctionCall part and an interrupt look identical
// otherwise. Partial events are skipped because in streaming mode the same call
// is emitted repeatedly and only the aggregated event is settled.
func collectConfirmations(events []*session.Event) []pendingConfirmation {
	var out []pendingConfirmation
	seen := map[string]bool{}
	for _, ev := range events {
		if ev == nil || len(ev.LongRunningToolIDs) == 0 || ev.Partial || ev.Content == nil {
			continue
		}
		longRunning := map[string]bool{}
		for _, id := range ev.LongRunningToolIDs {
			longRunning[id] = true
		}
		for _, p := range ev.Content.Parts {
			fc := p.FunctionCall
			if fc == nil || !longRunning[fc.ID] || seen[fc.ID] {
				continue
			}
			seen[fc.ID] = true
			hint := ""
			if tc, ok := fc.Args["toolConfirmation"].(map[string]any); ok {
				hint, _ = tc["hint"].(string)
			}
			out = append(out, pendingConfirmation{id: fc.ID, name: fc.Name, hint: hint})
		}
	}
	return out
}

// summarize renders an event stream compactly. Every failure in this file is
// "the flow did not go where I expected", and the only way to tell which hop
// dropped it is to see the calls in order.
func summarize(events []*session.Event) string {
	var b strings.Builder
	for _, ev := range events {
		if ev == nil || ev.Partial || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				mark := ""
				for _, id := range ev.LongRunningToolIDs {
					if id == p.FunctionCall.ID {
						mark = " [LONG-RUNNING]"
					}
				}
				fmt.Fprintf(&b, "  call %s(%v) author=%s%s\n",
					p.FunctionCall.Name, p.FunctionCall.Args, ev.Author, mark)
			case p.FunctionResponse != nil:
				fmt.Fprintf(&b, "  resp %s -> %v author=%s\n",
					p.FunctionResponse.Name, p.FunctionResponse.Response, ev.Author)
			case strings.TrimSpace(p.Text) != "":
				fmt.Fprintf(&b, "  text author=%s: %s\n", ev.Author, strings.TrimSpace(p.Text))
			}
		}
	}
	if b.Len() == 0 {
		return "  (no non-partial content events)"
	}
	return b.String()
}

// turn runs one turn to completion and returns its events.
func turn(ctx context.Context, t *testing.T, rn *runner.Runner, sessionID string, msg *genai.Content) []*session.Event {
	t.Helper()
	var events []*session.Event
	for ev, err := range rn.Run(ctx, "hitl-spike", sessionID, msg, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("turn failed: %v", err)
		}
		events = append(events, ev)
	}
	return events
}

// confirm builds the FunctionResponse that answers an interrupt. The ID and
// name must match the interrupt exactly or the runtime cannot correlate it.
func confirm(p pendingConfirmation, approved bool) *genai.Content {
	return &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				ID:       p.id,
				Name:     p.name,
				Response: map[string]any{"confirmed": approved},
			},
		}},
	}
}

func reportFrom(events []*session.Event) *schema.HealthReport {
	for _, ev := range events {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			// The orchestrator reports through submit_health_report, a Task
			// sub-agent through finish_task. Both are accepted so this helper
			// does not silently blank out when an agent's mode changes — which
			// is exactly how it failed once.
			if p.FunctionCall == nil {
				continue
			}
			switch p.FunctionCall.Name {
			case ReportToolName, "finish_task":
			default:
				continue
			}
			if h := decodeReport(p.FunctionCall.Args); h != nil {
				return h
			}
		}
	}
	return nil
}

// decodeReport re-marshals finish_task's args into a HealthReport. A local
// copy of the evals harness's decoder; importing it would make the domain
// package depend on its own test harness.
func decodeReport(args map[string]any) *schema.HealthReport {
	if len(args) == 0 {
		return nil
	}
	// finish_task may wrap the payload in a single-key envelope.
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

const scalePrompt = "Scale the deployment \"checkout-api\" in namespace \"prod\" to 5 replicas."

// A write buried inside a Task sub-agent must still interrupt, and the
// interrupt must reach the caller rather than being swallowed or turned into a
// tool error the model narrates around.
func TestHITLInterruptCrossesTheSubAgentBoundary(t *testing.T) {
	if os.Getenv(envLiveModel) == "" {
		t.Skipf("set %s=1 to run", envLiveModel)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	rec := &writeRecorder{}
	rn, err := runner.NewInMemory("sre-hitl-spike", buildGatedAgent(t, rec))
	if err != nil {
		t.Fatal(err)
	}

	events := turn(ctx, t, rn, "interrupt", genai.NewContentFromText(scalePrompt, genai.RoleUser))

	pending := collectConfirmations(events)
	if len(pending) == 0 {
		t.Fatalf("no confirmation interrupt surfaced; the write was not gated. "+
			"tool ran %d time(s)\nevents:\n%s", rec.count(), summarize(events))
	}
	if got := pending[0].name; got != toolconfirmation.FunctionCallName {
		t.Errorf("interrupt name = %q, want %q", got, toolconfirmation.FunctionCallName)
	}
	if rec.count() != 0 {
		t.Errorf("the write executed %d time(s) before approval; the gate is not "+
			"load-bearing", rec.count())
	}
	t.Logf("interrupt id=%s name=%s hint=%q", pending[0].id, pending[0].name, pending[0].hint)
}

// Approving must actually run the write, and the run must still terminate with
// a structured HealthReport — the contract has to survive the interrupt/resume
// round trip, not just the interrupt.
func TestHITLApprovalResumesAndPreservesTheReport(t *testing.T) {
	if os.Getenv(envLiveModel) == "" {
		t.Skipf("set %s=1 to run", envLiveModel)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	rec := &writeRecorder{}
	rn, err := runner.NewInMemory("sre-hitl-spike", buildGatedAgent(t, rec))
	if err != nil {
		t.Fatal(err)
	}
	const sid = "approve"

	pending := collectConfirmations(
		turn(ctx, t, rn, sid, genai.NewContentFromText(scalePrompt, genai.RoleUser)))
	if len(pending) == 0 {
		t.Fatal("no confirmation interrupt to approve")
	}

	resumed := turn(ctx, t, rn, sid, confirm(pending[0], true))

	if rec.count() != 1 {
		t.Fatalf("after approval the write ran %d time(s), want exactly 1", rec.count())
	}
	got := rec.calls[0]
	if got.Namespace != "prod" || got.Deployment != "checkout-api" || got.Replicas != 5 {
		t.Errorf("write executed with %+v, want prod/checkout-api/5", got)
	}
	if report := reportFrom(resumed); report == nil {
		t.Errorf("no HealthReport after resume; the structured contract did not "+
			"survive the round trip\nevents:\n%s", summarize(resumed))
	} else {
		t.Logf("resumed report: severity=%s summary=%s", report.OverallSeverity, report.Summary)
	}
}

// Denying must leave the cluster untouched. This is the half that matters:
// an approval flow that cannot actually stop a write is decoration.
func TestHITLDenialBlocksTheWrite(t *testing.T) {
	if os.Getenv(envLiveModel) == "" {
		t.Skipf("set %s=1 to run", envLiveModel)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	rec := &writeRecorder{}
	rn, err := runner.NewInMemory("sre-hitl-spike", buildGatedAgent(t, rec))
	if err != nil {
		t.Fatal(err)
	}
	const sid = "deny"

	pending := collectConfirmations(
		turn(ctx, t, rn, sid, genai.NewContentFromText(scalePrompt, genai.RoleUser)))
	if len(pending) == 0 {
		t.Fatal("no confirmation interrupt to deny")
	}

	resumed := turn(ctx, t, rn, sid, confirm(pending[0], false))

	if rec.count() != 0 {
		t.Fatalf("the write executed %d time(s) despite denial", rec.count())
	}
	// The agent should still finish rather than hang or crash — a denied write
	// is a normal outcome it has to be able to report.
	var text strings.Builder
	for _, ev := range resumed {
		if ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			text.WriteString(p.Text)
		}
	}
	t.Logf("after denial: %s", strings.TrimSpace(text.String()))
}
