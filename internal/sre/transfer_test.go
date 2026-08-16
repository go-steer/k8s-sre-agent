package sre

import (
	"context"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-sre-agent/internal/lookout"
	"github.com/go-steer/core-sre-agent/internal/schema"
)

// A specialist must not be offered transfer_to_agent. See buildOne's
// "Why every Task specialist disallows transfer" — the only target ADK would
// ever give it is the orchestrator, and taking that exit kills the run.
//
// This asserts on the LLMRequest rather than on the config, because the
// declaration surface is what the model acts on and ADK builds it at request
// time from flags several layers below llmagent.Config.
func TestSpecialistsAreNotOfferedTransfer(t *testing.T) {
	offline, _, err := lookout.Offline()
	if err != nil {
		t.Fatal(err)
	}
	// The orchestrator delegates once; the specialist's declarations are
	// recorded on the request that delegation produces.
	orch := &scriptedModel{name: "orch", turns: [][]*genai.Part{
		{{FunctionCall: &genai.FunctionCall{ID: "d1", Name: "pod-inspector", Args: map[string]any{}}}},
		{{Text: "done"}},
	}}
	sub := &recordingModel{}
	root, err := Build(Config{Main: orch, Subagent: sub, Toolsets: []tool.Toolset{offline}})
	if err != nil {
		t.Fatal(err)
	}
	rn, err := runner.NewInMemory("sre-transfer", root)
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range rn.Run(context.Background(), "u", "s",
		genai.NewContentFromText("assess", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	declared := sub.declared()
	if len(declared) == 0 {
		t.Fatal("the specialist model was never called, so this test proves nothing")
	}
	if declared["finish_task"] == nil {
		t.Errorf("the specialist has no finish_task, so it has no way to report at all; "+
			"declared: %v", keys(declared))
	}
	if declared["transfer_to_agent"] != nil {
		t.Errorf("the specialist is offered transfer_to_agent; its only target is the " +
			"orchestrator and taking it aborts the run with ErrInvalidRunNodeContext")
	}
}

// The crash the flags prevent, reproduced end to end.
//
// This deliberately builds the agent the broken way — an ADK Task agent with
// transfer left enabled — and drives the model into the transfer. It documents
// the cost of the flags and will start failing if ADK ever fixes the context
// rebuild in workflow/agent_node.go, at which point the flags become a
// design choice rather than a crash fix and this test should be re-read, not
// deleted.
func TestTransferFromASpecialistIsFatal(t *testing.T) {
	offline, _, err := lookout.Offline()
	if err != nil {
		t.Fatal(err)
	}
	orch := &scriptedModel{name: "orch", turns: [][]*genai.Part{
		{{FunctionCall: &genai.FunctionCall{ID: "d1", Name: "pod-inspector", Args: map[string]any{}}}},
		{{Text: "done"}},
	}}
	sub := &scriptedModel{name: "sub", turns: [][]*genai.Part{
		{{FunctionCall: &genai.FunctionCall{ID: "x1", Name: "transfer_to_agent",
			Args: map[string]any{"agent_name": OrchestratorName}}}},
	}}
	root, err := buildTransferable(t, orch, sub, offline)
	if err != nil {
		t.Fatal(err)
	}
	rn, err := runner.NewInMemory("sre-transfer-fatal", root)
	if err != nil {
		t.Fatal(err)
	}
	var runErr error
	for _, err := range rn.Run(context.Background(), "u", "s",
		genai.NewContentFromText("assess", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			runErr = err
			break
		}
	}
	if runErr == nil {
		t.Fatal("a specialist transferred to the orchestrator and the run survived; " +
			"ADK may have fixed the context rebuild — re-read buildOne's transfer section")
	}
	if !strings.Contains(runErr.Error(), "RunNode called outside a dynamic node") {
		t.Errorf("transfer failed for an unexpected reason: %v", runErr)
	}
}

// buildTransferable assembles the same orchestrator/specialist shape Build
// produces, minus the DisallowTransfer flags. Written out here rather than
// exposed as a knob on Config, so the only way to get a transferable
// specialist is to ask for one in a test.
func buildTransferable(t *testing.T, orch, sub adkmodel.LLM, offline tool.Toolset) (adkagent.Agent, error) {
	t.Helper()
	specialist, err := llmagent.New(llmagent.Config{
		Name:         "pod-inspector",
		Description:  "inspects pods",
		Instruction:  "Inspect pods.",
		Model:        sub,
		Toolsets:     []tool.Toolset{offline},
		Mode:         llmagent.ModeTask,
		OutputSchema: schema.ReportSchema(),
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
