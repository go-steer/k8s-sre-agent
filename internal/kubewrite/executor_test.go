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
	"errors"
	"slices"
	"strings"
	"testing"

	"google.golang.org/adk/v2/tool"
)

var scaleOne = map[string]any{
	"deployment_name": "checkout-api",
	"namespace":       "prod",
	"replicas":        5,
}

// TestFirstCallAsksAndChangesNothing is the load-bearing test of this package.
//
// Everything else here is about getting the command right; this is about the
// command not running. If it ever passes trivially — because the runner was
// not consulted for a reason unrelated to the gate — the assertion on the
// pending confirmation catches it.
func TestFirstCallAsksAndChangesNothing(t *testing.T) {
	run, tools := kit(t, Config{})
	ctx := newGateContext()

	_, err := call(t, tools, ctx, "kubectl_scale_deployment", scaleOne)
	if !errors.Is(err, tool.ErrConfirmationRequired) {
		t.Fatalf("err = %v, want ErrConfirmationRequired", err)
	}
	if len(run.calls) != 0 {
		t.Fatalf("kubectl ran before anyone approved: %v", run.args())
	}

	got, ok := ctx.pending()
	if !ok {
		t.Fatal("no pending confirmation recorded; ADK synthesises the interrupt event " +
			"from Actions().RequestedToolConfirmations and would have emitted nothing")
	}
	if got.Confirmed {
		t.Error("pending confirmation is already Confirmed")
	}
	if !ctx.acts.SkipSummarization {
		t.Error("SkipSummarization is false; the agent loop would run on past the interrupt")
	}
}

// TestTheHintIsTheCommand is why this package raises the confirmation itself
// instead of setting functiontool's RequireConfirmation flag. ADK's built-in
// hint describes the protocol and not the change.
func TestTheHintIsTheCommand(t *testing.T) {
	_, tools := kit(t, Config{})
	ctx := newGateContext()

	if _, err := call(t, tools, ctx, "kubectl_scale_deployment", scaleOne); !errors.Is(err, tool.ErrConfirmationRequired) {
		t.Fatalf("err = %v, want ErrConfirmationRequired", err)
	}
	got, _ := ctx.pending()

	for _, want := range []string{
		"kind-sre-eval-test", // which cluster, first
		"Scale Deployment prod/checkout-api to 5 replicas.",
		"kubectl --context kind-sre-eval-test scale deployment/checkout-api -n prod --replicas=5",
		"Rejecting runs nothing.",
	} {
		if !strings.Contains(got.Hint, want) {
			t.Errorf("hint is missing %q:\n%s", want, got.Hint)
		}
	}

	p, ok := got.Payload.(plan)
	if !ok {
		t.Fatalf("payload is %T, want plan", got.Payload)
	}
	if p.Cluster != "kind-sre-eval-test" || len(p.Commands) != 1 {
		t.Errorf("payload = %+v", p)
	}
}

// TestApprovalRuns closes the loop: the resumed call runs exactly the command
// the human was shown, and nothing else.
func TestApprovalRuns(t *testing.T) {
	run, tools := kit(t, Config{})

	res, err := call(t, tools, approved(), "kubectl_scale_deployment", scaleOne)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != StatusApplied {
		t.Errorf("status = %q, want %q (%s)", res.Status, StatusApplied, res.Error)
	}
	want := [][]string{{"scale", "deployment/checkout-api", "-n", "prod", "--replicas=5"}}
	if got := run.args(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("ran %v, want %v", got, want)
	}
	if len(res.Commands) != 1 || !strings.Contains(res.Commands[0], "--replicas=5") {
		t.Errorf("commands = %v", res.Commands)
	}
	if !strings.Contains(res.Output, "$ kubectl --context kind-sre-eval-test scale") {
		t.Errorf("output does not echo the command:\n%s", res.Output)
	}
}

// TestRejectionRunsNothingAndIsNotAnError.
//
// The status matters as much as the absence of the call. A rejection returned
// as a Go error reads to a model as an accident worth another attempt; the
// whole point of a human declining is that the next move is to report it.
func TestRejectionRunsNothingAndIsNotAnError(t *testing.T) {
	run, tools := kit(t, Config{})

	res, err := call(t, tools, rejected(), "kubectl_scale_deployment", scaleOne)
	if err != nil {
		t.Fatalf("Run returned an error for a rejection: %v", err)
	}
	if res.Status != StatusRejected {
		t.Errorf("status = %q, want %q", res.Status, StatusRejected)
	}
	if len(run.calls) != 0 {
		t.Fatalf("kubectl ran after a rejection: %v", run.args())
	}
	for _, want := range []string{"declined", "Do not retry"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("rejection error is missing %q: %q", want, res.Error)
		}
	}
	if len(res.Commands) == 0 {
		t.Error("a rejection should still report what was declined")
	}
}

// TestRefusalNeverBecomesAnApprovalPrompt is the ordering guarantee in guard's
// doc comment, checked rather than asserted in prose: an operator is never
// shown a command this package was going to decline anyway.
func TestRefusalNeverBecomesAnApprovalPrompt(t *testing.T) {
	run, tools := kit(t, Config{})
	ctx := newGateContext()

	res, err := call(t, tools, ctx, "kubectl_scale_deployment", map[string]any{
		"deployment_name": "coredns",
		"namespace":       "kube-system",
		"replicas":        0,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != StatusRefused {
		t.Fatalf("status = %q, want %q", res.Status, StatusRefused)
	}
	if _, ok := ctx.pending(); ok {
		t.Error("a refused call asked a human for approval")
	}
	if len(run.calls) != 0 {
		t.Errorf("kubectl ran: %v", run.args())
	}
	if !strings.Contains(res.Error, "protected") || !strings.Contains(res.Error, "Nothing was changed") {
		t.Errorf("refusal = %q", res.Error)
	}
}

// TestBulkIsOneApprovalForEveryCommand. Upstream's rule, and the reason a bulk
// tool exists at all: the human sees the whole batch or none of it.
func TestBulkIsOneApprovalForEveryCommand(t *testing.T) {
	run, tools := kit(t, Config{})
	args := map[string]any{"targets": []any{
		map[string]any{"resource_type": "pod", "resource_name": "web-1", "namespace": "prod"},
		map[string]any{"resource_type": "pod", "resource_name": "web-2", "namespace": "prod"},
		map[string]any{"resource_type": "configmap", "resource_name": "web-cfg", "namespace": "prod"},
	}}

	ctx := newGateContext()
	if _, err := call(t, tools, ctx, "kubectl_delete_resources_bulk", args); !errors.Is(err, tool.ErrConfirmationRequired) {
		t.Fatalf("err = %v, want ErrConfirmationRequired", err)
	}
	got, _ := ctx.pending()
	for _, want := range []string{"web-1", "web-2", "web-cfg"} {
		if !strings.Contains(got.Hint, want) {
			t.Errorf("hint hides target %q:\n%s", want, got.Hint)
		}
	}
	if len(ctx.acts.RequestedToolConfirmations) != 1 {
		t.Errorf("recorded %d confirmations for one batch", len(ctx.acts.RequestedToolConfirmations))
	}

	res, err := call(t, tools, approved(), "kubectl_delete_resources_bulk", args)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != StatusApplied || len(run.calls) != 3 {
		t.Errorf("status = %q after %d calls, want applied after 3", res.Status, len(run.calls))
	}
}

// TestPartialBatchSaysSo. A bulk operation is the only way to reach a state
// where some of the cluster changed and some did not, and a model that reads
// "failed" would reasonably re-run the whole batch.
func TestPartialBatchSaysSo(t *testing.T) {
	run, tools := kit(t, Config{})
	run.failOn = func(args []string) bool { return slices.Contains(args, "web-2") }

	res, err := call(t, tools, approved(), "kubectl_delete_resources_bulk", map[string]any{
		"targets": []any{
			map[string]any{"resource_type": "pod", "resource_name": "web-1", "namespace": "prod"},
			map[string]any{"resource_type": "pod", "resource_name": "web-2", "namespace": "prod"},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != StatusPartial {
		t.Fatalf("status = %q, want %q", res.Status, StatusPartial)
	}
	for _, want := range []string{"1 of 2", "web-2", "do not re-run the whole batch"} {
		if !strings.Contains(res.Error, want) {
			t.Errorf("partial error is missing %q: %q", want, res.Error)
		}
	}
}

// TestEveryCommandFailingIsFailedNotPartial.
func TestEveryCommandFailingIsFailedNotPartial(t *testing.T) {
	run, tools := kit(t, Config{})
	run.failOn = func([]string) bool { return true }

	res, err := call(t, tools, approved(), "kubectl_scale_deployment", scaleOne)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != StatusFailed {
		t.Errorf("status = %q, want %q", res.Status, StatusFailed)
	}
	if !strings.Contains(res.Output, "Error from server") {
		t.Errorf("output drops kubectl's diagnosis:\n%s", res.Output)
	}
}

// TestOutputIsBounded. kubectl echoes a rejected object, and a bulk operation
// multiplies it; an unbounded result is an unbounded context.
func TestOutputIsBounded(t *testing.T) {
	run, tools := kit(t, Config{})
	run.output = strings.Repeat("x", 3*maxOutputPerStep)

	res, err := call(t, tools, approved(), "kubectl_scale_deployment", scaleOne)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Output) > maxOutput {
		t.Errorf("output is %d bytes, over the %d cap", len(res.Output), maxOutput)
	}
	if !strings.Contains(res.Output, "truncated") {
		t.Error("truncation is silent; the model cannot tell the output was cut")
	}
}

// TestStdinIsShownBeforeItIsApplied. apply is the one tool whose real payload
// is not on the command line, so a hint that stops at the command line would
// be asking for approval of `kubectl apply -f -`.
func TestStdinIsShownBeforeItIsApplied(t *testing.T) {
	run, tools := kit(t, Config{})
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: web-cfg\n"
	args := map[string]any{"namespace": "prod", "manifest_yaml": manifest}

	ctx := newGateContext()
	if _, err := call(t, tools, ctx, "kubectl_apply_manifest", args); !errors.Is(err, tool.ErrConfirmationRequired) {
		t.Fatalf("err = %v, want ErrConfirmationRequired", err)
	}
	got, _ := ctx.pending()
	if !strings.Contains(got.Hint, "name: web-cfg") {
		t.Errorf("hint does not show the manifest:\n%s", got.Hint)
	}

	if _, err := call(t, tools, approved(), "kubectl_apply_manifest", args); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(run.calls) != 1 || run.calls[0].stdin != manifest {
		t.Errorf("manifest did not reach stdin: %+v", run.calls)
	}
}

// TestConfirmationErrorsPropagate. RequestConfirmation fails when ADK has no
// function call ID to hang the confirmation on. Swallowing that would run the
// change with nobody watching.
func TestConfirmationErrorsPropagate(t *testing.T) {
	run, tools := kit(t, Config{})
	ctx := newGateContext()
	ctx.functionCallID = ""

	_, err := call(t, tools, ctx, "kubectl_scale_deployment", scaleOne)
	if err == nil || errors.Is(err, tool.ErrConfirmationRequired) {
		t.Fatalf("err = %v, want the RequestConfirmation failure", err)
	}
	if len(run.calls) != 0 {
		t.Errorf("kubectl ran: %v", run.args())
	}
}
