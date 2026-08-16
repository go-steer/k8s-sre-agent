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

package kubewrite

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
)

// fakeRunner stands in for kubectl.
//
// Every test in this package uses it, and none of them can reach a cluster
// even if the guards were wrong — which is the point. A test suite for a
// package whose job is mutation has to be safe to run on a laptop with 64 kube
// contexts, or it will not be run.
type fakeRunner struct {
	cluster string
	calls   []fakeCall
	// failOn makes the invocation fail when it returns true, so the
	// applied/partial/failed classification can be exercised.
	failOn func(args []string) bool
	output string
}

type fakeCall struct {
	args  []string
	stdin string
}

func (r *fakeRunner) Cluster() string { return r.cluster }

func (r *fakeRunner) Run(_ context.Context, stdin string, args ...string) (string, error) {
	r.calls = append(r.calls, fakeCall{args: slices.Clone(args), stdin: stdin})
	out := r.output
	if out == "" {
		out = strings.Join(args, " ") + " ok"
	}
	if r.failOn != nil && r.failOn(args) {
		return out + "\nError from server: rejected", fmt.Errorf("exit status 1")
	}
	return out, nil
}

func (r *fakeRunner) args() [][]string {
	out := make([][]string, 0, len(r.calls))
	for _, c := range r.calls {
		out = append(out, c.args)
	}
	return out
}

// gateContext is an agent.Context that implements the confirmation half of the
// protocol exactly as ADK's commonContext does (agent/common_context.go), so
// what these tests assert on is what ADK would actually see: an entry in
// Actions().RequestedToolConfirmations, which is the sole input to the
// interrupt event ADK synthesises.
type gateContext struct {
	adkagent.ContextMock

	functionCallID string
	acts           *session.EventActions
	confirmation   *toolconfirmation.ToolConfirmation
}

func newGateContext() *gateContext {
	return &gateContext{
		functionCallID: "toolu_test",
		acts:           &session.EventActions{},
	}
}

// approved returns a context representing the resumed call after a human said
// yes; rejected, after they said no.
func approved() *gateContext {
	c := newGateContext()
	c.confirmation = &toolconfirmation.ToolConfirmation{Confirmed: true}
	return c
}

func rejected() *gateContext {
	c := newGateContext()
	c.confirmation = &toolconfirmation.ToolConfirmation{Confirmed: false}
	return c
}

func (c *gateContext) Actions() *session.EventActions { return c.acts }

func (c *gateContext) ToolConfirmation() *toolconfirmation.ToolConfirmation {
	return c.confirmation
}

func (c *gateContext) RequestConfirmation(hint string, payload any) error {
	if c.functionCallID == "" {
		return fmt.Errorf("error function call id not set when requesting confirmation for tool")
	}
	if c.acts.RequestedToolConfirmations == nil {
		c.acts.RequestedToolConfirmations = make(map[string]toolconfirmation.ToolConfirmation)
	}
	c.acts.RequestedToolConfirmations[c.functionCallID] = toolconfirmation.ToolConfirmation{
		Hint:      hint,
		Confirmed: false,
		Payload:   payload,
	}
	c.acts.SkipSummarization = true
	return nil
}

// pending returns the confirmation this context is holding, if any.
func (c *gateContext) pending() (toolconfirmation.ToolConfirmation, bool) {
	got, ok := c.acts.RequestedToolConfirmations[c.functionCallID]
	return got, ok
}

// kit builds the toolset over a fake runner.
//
// Tools are handed back as runnableTool — the interface ADK invokes them
// through — so every test goes through the real JSON-schema decode rather than
// calling a planner directly. A schema that cannot express a tool's arguments
// fails here rather than in front of a model.
func kit(t *testing.T, cfg Config) (*fakeRunner, map[string]runnableTool) {
	t.Helper()
	run := &fakeRunner{cluster: "kind-sre-eval-test"}
	cfg.Runner = run
	cfg.Context = run.cluster
	tools, err := Tools(cfg)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	byName := make(map[string]runnableTool, len(tools))
	for _, tl := range tools {
		r, ok := tl.(runnableTool)
		if !ok {
			t.Fatalf("tool %q is not runnable (%T)", tl.Name(), tl)
		}
		byName[tl.Name()] = r
	}
	return run, byName
}

// call invokes a tool and decodes its Result.
func call(t *testing.T, tools map[string]runnableTool, ctx adkagent.Context, name string, args map[string]any) (Result, error) {
	t.Helper()
	tl, ok := tools[name]
	if !ok {
		t.Fatalf("no tool named %q", name)
	}
	raw, err := tl.Run(ctx, args)
	if err != nil {
		return Result{}, err
	}
	blob, mErr := json.Marshal(raw)
	if mErr != nil {
		t.Fatalf("marshal result: %v", mErr)
	}
	var res Result
	if uErr := json.Unmarshal(blob, &res); uErr != nil {
		t.Fatalf("decode result from %v: %v", raw, uErr)
	}
	return res, nil
}

// TestToolNamesMatchTheBuiltToolset pins the roster.
//
// Two things it protects. A write tool cannot appear in the agent's hands
// without someone editing this list in the same change — the roster is a
// review surface, not an implementation detail. And ToolNames is used to bind
// the toolset to change-executor and nothing else, so a name that drifts out
// of sync would silently grant or withhold a mutation.
func TestToolNamesMatchTheBuiltToolset(t *testing.T) {
	want := []string{
		"kubectl_scale_deployment",
		"kubectl_scale_bulk",
		"kubectl_patch_resource_limits",
		"kubectl_patch_hpa",
		"kubectl_patch_configmap",
		"kubectl_delete_pod",
		"kubectl_delete_resources_bulk",
		"kubectl_delete_custom_resource",
		"kubectl_resize_pvc",
		"kubectl_apply_manifest",
		"kubectl_cordon_node",
		"kubectl_uncordon_node",
		"kubectl_rollout_restart",
		"kubectl_rollback_deployment",
	}
	if got := ToolNames(); !slices.Equal(got, want) {
		t.Errorf("ToolNames() = %v, want %v", got, want)
	}

	_, tools := kit(t, Config{})
	if len(tools) != len(want) {
		t.Errorf("built %d tools, want %d", len(tools), len(want))
	}
	for _, n := range want {
		if _, ok := tools[n]; !ok {
			t.Errorf("tool %q named by ToolNames but not built", n)
		}
	}
}

// TestTheGateSurvivesRequestPacking.
//
// gatedTool declares itself through the inner function tool and then has to
// put *itself* back into the request's tool map, or ADK calls the inner tool
// and the rejection path reverts to ADK's. That substitution is invisible
// until someone declines a change and watches it happen anyway, so it is
// asserted on the request the model would actually be sent.
func TestTheGateSurvivesRequestPacking(t *testing.T) {
	_, tools := kit(t, Config{})
	req := &model.LLMRequest{}
	ctx := newGateContext()

	for name, tl := range tools {
		pr, ok := tl.(interface {
			ProcessRequest(adkagent.Context, *model.LLMRequest) error
		})
		if !ok {
			t.Fatalf("%s does not implement ProcessRequest", name)
		}
		if err := pr.ProcessRequest(ctx, req); err != nil {
			t.Fatalf("%s: ProcessRequest: %v", name, err)
		}
	}
	for name := range tools {
		packed, ok := req.Tools[name]
		if !ok {
			t.Errorf("%s is not on the wire", name)
			continue
		}
		if _, ok := packed.(*gatedTool); !ok {
			t.Errorf("%s packed as %T, not *gatedTool: a rejection would take ADK's path", name, packed)
		}
	}
}

// TestEveryToolSaysItPausesForApproval keeps the description contract.
//
// A model that does not know a tool blocks either avoids it or, worse, calls
// it and narrates the change as done while the approval is still pending.
func TestEveryToolSaysItPausesForApproval(t *testing.T) {
	_, tools := kit(t, Config{})
	for name, tl := range tools {
		if !strings.Contains(tl.Description(), "pauses for human approval") {
			t.Errorf("%s: description does not say the call pauses for approval:\n%s", name, tl.Description())
		}
	}
}
