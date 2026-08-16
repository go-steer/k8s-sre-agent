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

package approval

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

// These tests drive the production nesting — Chat orchestrator, Task write
// specialist, gated tool — with a scripted model rather than a live one.
//
// The live spike in internal/sre proved the interrupt crosses the sub-agent
// boundary at all. What it cannot do is run in CI, and the gate is the one
// thing in this repo that must not regress unnoticed: a broken read path
// produces a bad report, a broken gate produces an unapproved write. So the
// protocol is pinned here, offline, and the live tests stay as evidence that a
// real model still walks into it.

const (
	orchestrator = "sre-orchestrator"
	executor     = "change-executor"
	writeTool    = "kubectl_scale_deployment"
)

func TestADeniedWriteNeverRuns(t *testing.T) {
	rec, sess := harness(t, DenyAll(), oneWrite())

	turn, err := sess.Send(context.Background(), userMsg("scale checkout-api to 5"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("the write ran %d time(s) after a denial; the gate is decoration", n)
	}
	if len(turn.Decisions) != 1 {
		t.Fatalf("%d decisions, want 1", len(turn.Decisions))
	}
	if turn.Decisions[0].Approved {
		t.Error("DenyAll approved a write")
	}
	// The run has to end, not hang: a declined write is an outcome the agent
	// reports, and the orchestrator must get its turn back to report it.
	if !finished(turn) {
		t.Errorf("the run did not reach the orchestrator's answer:\n%s", transcript(turn))
	}
}

func TestAnApprovedWriteRunsExactlyOnce(t *testing.T) {
	rec, sess := harness(t, ApproveAllUnattended(), oneWrite())

	turn, err := sess.Send(context.Background(), userMsg("scale checkout-api to 5"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if n := rec.count(); n != 1 {
		t.Fatalf("the write ran %d time(s) after one approval, want 1:\n%s", n, transcript(turn))
	}
	if got := rec.calls()[0]; got.Namespace != "prod" || got.Deployment != "checkout-api" || got.Replicas != 5 {
		t.Errorf("the approved arguments are not the ones that ran: %+v", got)
	}
	if turn.Approved() != 1 {
		t.Errorf("Approved() = %d, want 1", turn.Approved())
	}
}

// What the human is shown must be what runs. The whole reason kubewrite builds
// its own hint is that ADK's built-in one describes the protocol rather than
// the change, so a Request that drops the hint would silently return the gate
// to "approve an opaque function call".
func TestTheRequestCarriesWhatTheToolAskedToShow(t *testing.T) {
	var seen Request
	_, sess := harness(t, Func(func(_ context.Context, r Request) (bool, error) {
		seen = r
		return false, nil
	}), oneWrite())

	if _, err := sess.Send(context.Background(), userMsg("scale it")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(seen.Hint, "kubectl scale deployment/checkout-api") {
		t.Errorf("the hint does not carry the command: %q", seen.Hint)
	}
	if seen.Tool != writeTool {
		t.Errorf("Tool = %q, want %q", seen.Tool, writeTool)
	}
	if seen.Args["deployment"] != "checkout-api" {
		t.Errorf("Args lost the call's arguments: %v", seen.Args)
	}
	if seen.Agent != executor {
		t.Errorf("Agent = %q, want %q — an approval request from anywhere else means "+
			"a write tool escaped the write specialist", seen.Agent, executor)
	}
	// Payload survives whether or not the session was persisted in between.
	got, err := PayloadAs[map[string]any](seen)
	if err != nil {
		t.Fatalf("PayloadAs: %v", err)
	}
	if got["cluster"] != "kind-sre-eval-a1" {
		t.Errorf("payload = %v, want the cluster name", got)
	}
}

// Two writes in one turn are two independent decisions, and both answers have
// to travel in one message — see Answer.
func TestEachWriteIsDecidedSeparatelyAndAnsweredTogether(t *testing.T) {
	rec, sess := harness(t, Func(func(_ context.Context, r Request) (bool, error) {
		return r.Args["deployment"] == "checkout-api", nil
	}), twoWrites())

	turn, err := sess.Send(context.Background(), userMsg("scale both"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(turn.Decisions) != 2 {
		t.Fatalf("%d decisions, want 2:\n%s", len(turn.Decisions), transcript(turn))
	}
	ran := rec.calls()
	if len(ran) != 1 {
		t.Fatalf("%d writes ran, want just the approved one: %+v", len(ran), ran)
	}
	if ran[0].Deployment != "checkout-api" {
		t.Errorf("the wrong write ran: %+v", ran[0])
	}
	// One resume message, carrying both answers. Splitting them dangles a
	// tool_use block and the *next* request 400s, which reads as a provider
	// error rather than as this.
	if len(turn.Resumes) != 1 {
		t.Fatalf("the two decisions went back in %d messages, want 1", len(turn.Resumes))
	}
	if n := len(turn.Resumes[0].Parts); n != 2 {
		t.Errorf("the resume message carries %d answers, want 2", n)
	}
}

// An Approver that cannot answer must not be read as a yes.
func TestAFailedApprovalDenies(t *testing.T) {
	boom := errors.New("slack is down")
	rec, sess := harness(t, Func(func(context.Context, Request) (bool, error) {
		return true, boom // returns true *and* an error: the error wins
	}), oneWrite())

	turn, err := sess.Send(context.Background(), userMsg("scale it"))
	if !errors.Is(err, boom) {
		t.Errorf("Send did not report the approver failure: %v", err)
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("a write ran on a failed approval (%d times)", n)
	}
	if len(turn.Decisions) != 1 || turn.Decisions[0].Approved {
		t.Errorf("decisions = %+v, want one denial", turn.Decisions)
	}
	// The run still completed, so the agent got to report the outcome.
	if !finished(turn) {
		t.Errorf("the run was abandoned mid-write:\n%s", transcript(turn))
	}
}

// The loop bound. A model that re-requests a denied write forever would
// otherwise prompt a human forever.
func TestTheApprovalBoundStopsAskingAndKeepsDenying(t *testing.T) {
	asked := 0
	rec, sess := harness(t, Func(func(context.Context, Request) (bool, error) {
		asked++
		return false, nil
	}), insistentWrite())
	sess.MaxRequests = 3

	turn, err := sess.Send(context.Background(), userMsg("scale it"))
	if !errors.Is(err, ErrTooManyRequests) {
		t.Errorf("the bound was hit but not reported: %v", err)
	}
	if asked != 3 {
		t.Errorf("the human was asked %d times, want 3", asked)
	}
	if n := rec.count(); n != 0 {
		t.Errorf("%d writes ran past the bound", n)
	}
	if turn.Approved() != 0 {
		t.Error("the bound approved something")
	}
}

func TestNoApproverMeansNoWrites(t *testing.T) {
	rec, sess := harness(t, nil, oneWrite())
	if _, err := sess.Send(context.Background(), userMsg("scale it")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("a Session with no Approver ran %d write(s)", n)
	}
}

// A turn with no writes in it must not pay for the gate.
func TestAReadOnlyTurnAsksNobody(t *testing.T) {
	asked := 0
	_, sess := harness(t, Func(func(context.Context, Request) (bool, error) {
		asked++
		return true, nil
	}), noWrite())

	turn, err := sess.Send(context.Background(), userMsg("is prod healthy?"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if asked != 0 || len(turn.Decisions) != 0 {
		t.Errorf("a read-only turn raised %d approval(s)", asked)
	}
}

// --- harness -----------------------------------------------------------

type scaleArgs struct {
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
	Replicas   int    `json:"replicas"`
}

type scaleResult struct {
	Status string `json:"status"`
}

// recorder records the writes that actually executed. Every test in this file
// is ultimately an assertion about this number.
type recorder struct {
	mu  sync.Mutex
	got []scaleArgs
}

func (r *recorder) add(a scaleArgs) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, a)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func (r *recorder) calls() []scaleArgs {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]scaleArgs(nil), r.got...)
}

// harness builds the production shape — Chat orchestrator with a Task write
// specialist holding one gated tool — around a scripted model.
//
// The gate is reimplemented here the way internal/kubewrite implements it
// (RequestConfirmation with a hint the tool writes itself) rather than through
// functiontool's RequireConfirmation flag, because the hint is the part this
// package has to carry and the flag's hint is boilerplate about the protocol.
func harness(t *testing.T, ap Approver, script map[string][][]*genai.Part) (*recorder, *Session) {
	t.Helper()
	rec := &recorder{}

	scale, err := functiontool.New(
		functiontool.Config{Name: writeTool, Description: "Scale a deployment. Mutates the cluster."},
		func(ctx adkagent.Context, a scaleArgs) (scaleResult, error) {
			if ctx.ToolConfirmation() == nil {
				hint := fmt.Sprintf("Approve this change to cluster %q?\n\n    "+
					"kubectl scale deployment/%s -n %s --replicas=%d",
					"kind-sre-eval-a1", a.Deployment, a.Namespace, a.Replicas)
				if err := ctx.RequestConfirmation(hint, map[string]any{
					"cluster": "kind-sre-eval-a1",
					"effect":  "scale " + a.Deployment,
				}); err != nil {
					return scaleResult{}, err
				}
				return scaleResult{}, fmt.Errorf("write tool %q %w", writeTool, tool.ErrConfirmationRequired)
			}
			rec.add(a)
			return scaleResult{Status: "applied"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}

	m := &scriptedModel{script: script}
	sub, err := llmagent.New(llmagent.Config{
		Name:        executor,
		Description: "Applies changes.",
		Instruction: "Apply the change.",
		Model:       m,
		Tools:       []tool.Tool{scale},
		Mode:        llmagent.ModeTask,
		// Matching internal/sre's buildOne: a specialist that transfers instead
		// of finishing takes the run down with it.
		DisallowTransferToParent: true,
		DisallowTransferToPeers:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	root, err := llmagent.New(llmagent.Config{
		Name:        orchestrator,
		Description: "Coordinates.",
		Instruction: "Coordinate.",
		Model:       m,
		SubAgents:   []adkagent.Agent{sub},
		Mode:        llmagent.ModeChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	rn, err := runner.NewInMemory("approval-test", root)
	if err != nil {
		t.Fatal(err)
	}
	return rec, &Session{Runner: rn, UserID: "u", SessionID: t.Name(), Approver: ap}
}

func userMsg(s string) *genai.Content { return genai.NewContentFromText(s, genai.RoleUser) }

func delegate(id string) []*genai.Part {
	return []*genai.Part{{FunctionCall: &genai.FunctionCall{
		ID: id, Name: executor, Args: map[string]any{"request": "scale it"}}}}
}

func write(id, deployment string, replicas int) *genai.Part {
	return &genai.Part{FunctionCall: &genai.FunctionCall{
		ID: id, Name: writeTool, Args: map[string]any{
			"namespace": "prod", "deployment": deployment, "replicas": replicas}}}
}

func done(text string) []*genai.Part { return []*genai.Part{{Text: text}} }

func finishTask() []*genai.Part {
	return []*genai.Part{{FunctionCall: &genai.FunctionCall{
		ID: "ft", Name: "finish_task", Args: map[string]any{"result": "reported"}}}}
}

func oneWrite() map[string][][]*genai.Part {
	return map[string][][]*genai.Part{
		orchestrator: {delegate("d1"), done("reported")},
		executor:     {{write("w1", "checkout-api", 5)}, finishTask()},
	}
}

func twoWrites() map[string][][]*genai.Part {
	return map[string][][]*genai.Part{
		orchestrator: {delegate("d1"), done("reported")},
		executor:     {{write("w1", "checkout-api", 5), write("w2", "billing-api", 3)}, finishTask()},
	}
}

// insistentWrite models the failure the bound exists for: a specialist that
// answers every denial by asking again.
func insistentWrite() map[string][][]*genai.Part {
	script := map[string][][]*genai.Part{orchestrator: {delegate("d1"), done("reported")}}
	for i := range 10 {
		script[executor] = append(script[executor], []*genai.Part{
			write(fmt.Sprintf("w%d", i), "checkout-api", 5)})
	}
	return script
}

func noWrite() map[string][][]*genai.Part {
	return map[string][][]*genai.Part{
		orchestrator: {done("prod looks healthy")},
	}
}

// scriptedModel replays a fixed sequence of turns per agent.
//
// It keys on the name ADK stamps into every system instruction, because an
// LLMRequest carries no identity of its own — a single stub otherwise cannot
// tell the orchestrator's request from the specialist's.
type scriptedModel struct {
	script map[string][][]*genai.Part

	mu    sync.Mutex
	calls map[string]int
}

func (*scriptedModel) Name() string { return "scripted" }

func (m *scriptedModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	name := agentOf(req)
	m.mu.Lock()
	if m.calls == nil {
		m.calls = map[string]int{}
	}
	n := m.calls[name]
	m.calls[name]++
	m.mu.Unlock()

	parts := done("out of script for " + name)
	if turns := m.script[name]; n < len(turns) {
		parts = turns[n]
	} else if name == executor {
		// A Task agent that ends its turn without finish_task ends the caller's
		// turn too, which would fail these tests for the wrong reason.
		parts = finishTask()
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: parts},
			TurnComplete: true,
		}, nil)
	}
}

func agentOf(req *model.LLMRequest) string {
	if req.Config == nil || req.Config.SystemInstruction == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range req.Config.SystemInstruction.Parts {
		sb.WriteString(p.Text)
	}
	_, rest, ok := strings.Cut(sb.String(), `Your internal name is "`)
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(rest, `"`)
	return name
}

// finished reports whether the orchestrator got its turn back and answered.
func finished(t *Turn) bool {
	for _, ev := range t.Events {
		if ev == nil || ev.Content == nil || ev.Author != orchestrator || ev.Partial {
			continue
		}
		for _, p := range ev.Content.Parts {
			if strings.TrimSpace(p.Text) != "" {
				return true
			}
		}
	}
	return false
}

func transcript(t *Turn) string {
	var b strings.Builder
	for _, ev := range t.Events {
		if ev == nil || ev.Content == nil || ev.Partial {
			continue
		}
		for _, p := range ev.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				fmt.Fprintf(&b, "  %s call %s(%v)\n", ev.Author, p.FunctionCall.Name, p.FunctionCall.Args)
			case p.FunctionResponse != nil:
				fmt.Fprintf(&b, "  %s resp %s -> %v\n", ev.Author, p.FunctionResponse.Name, p.FunctionResponse.Response)
			case strings.TrimSpace(p.Text) != "":
				fmt.Fprintf(&b, "  %s text %q\n", ev.Author, strings.TrimSpace(p.Text))
			}
		}
	}
	if b.Len() == 0 {
		return "  (nothing)"
	}
	return b.String()
}
