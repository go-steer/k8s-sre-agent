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
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// A session that has been persisted and reloaded hands the confirmation back as
// a map rather than as the struct ADK attached in process. Both shapes reach
// Pending in production — the second one as soon as the scheduler runs on
// session/database durability — and a caller that only handles the first fails
// exactly when a run is resumed after a restart, which is the case the whole
// interrupt mechanism exists for.
func TestPendingReadsBothTheLiveAndThePersistedShape(t *testing.T) {
	orig := &genai.FunctionCall{ID: "w1", Name: "kubectl_scale_deployment",
		Args: map[string]any{"deployment": "checkout-api"}}

	live := interruptEvent("change-executor", "c1", map[string]any{
		"originalFunctionCall": orig,
		"toolConfirmation": &toolconfirmation.ToolConfirmation{
			Hint: "kubectl scale …", Payload: map[string]any{"cluster": "kind-sre-eval-a1"},
		},
	})
	persisted := interruptEvent("change-executor", "c2", map[string]any{
		"originalFunctionCall": map[string]any{
			"id": "w1", "name": "kubectl_scale_deployment",
			"args": map[string]any{"deployment": "checkout-api"},
		},
		"toolConfirmation": map[string]any{
			"hint": "kubectl scale …", "payload": map[string]any{"cluster": "kind-sre-eval-a1"},
		},
	})

	got := Pending([]*session.Event{live, persisted})
	if len(got) != 2 {
		t.Fatalf("%d requests, want 2: %+v", len(got), got)
	}
	for _, r := range got {
		if r.Tool != "kubectl_scale_deployment" {
			t.Errorf("%s: Tool = %q", r.ID, r.Tool)
		}
		if r.Args["deployment"] != "checkout-api" {
			t.Errorf("%s: Args = %v", r.ID, r.Args)
		}
		if r.Hint != "kubectl scale …" {
			t.Errorf("%s: Hint = %q", r.ID, r.Hint)
		}
		if r.Agent != "change-executor" {
			t.Errorf("%s: Agent = %q", r.ID, r.Agent)
		}
		p, err := PayloadAs[struct {
			Cluster string `json:"cluster"`
		}](r)
		if err != nil || p.Cluster != "kind-sre-eval-a1" {
			t.Errorf("%s: payload = %+v (%v)", r.ID, p, err)
		}
	}
}

// The discriminator is LongRunningToolIDs, not the function name, and streaming
// re-emits the same call once per chunk. Getting either wrong costs a human
// answer against a call that is not waiting for one.
func TestPendingIgnoresWhatIsNotWaiting(t *testing.T) {
	waiting := interruptEvent("change-executor", "c1", map[string]any{
		"originalFunctionCall": &genai.FunctionCall{ID: "w1", Name: "kubectl_scale_deployment"},
		"toolConfirmation":     &toolconfirmation.ToolConfirmation{Hint: "h"},
	})
	duplicate := interruptEvent("change-executor", "c1", waiting.Content.Parts[0].FunctionCall.Args)
	partial := interruptEvent("change-executor", "c2", waiting.Content.Parts[0].FunctionCall.Args)
	partial.Partial = true

	// An ordinary tool call: same shape, not listed as long-running.
	ordinary := &session.Event{Author: "change-executor", LLMResponse: model.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{ID: "r1", Name: "k8s_cluster_health"}},
		}}}}

	got := Pending([]*session.Event{ordinary, waiting, duplicate, partial})
	if len(got) != 1 || got[0].ID != "c1" {
		t.Errorf("Pending = %+v, want exactly the one settled interrupt", got)
	}
}

// An interrupt nobody knows how to answer must be reported, not skipped: the
// agent is blocked either way, and skipping makes a stuck run look finished.
func TestUnanswerableNamesTheInterruptWeCannotAnswer(t *testing.T) {
	confirmation := interruptEvent("change-executor", "c1", map[string]any{
		"originalFunctionCall": &genai.FunctionCall{ID: "w1", Name: "kubectl_scale_deployment"},
		"toolConfirmation":     &toolconfirmation.ToolConfirmation{Hint: "h"},
	})
	other := &session.Event{Author: "root", LongRunningToolIDs: []string{"x1"},
		LLMResponse: model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel,
			Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: "x1", Name: "adk_request_workflow_input"}}}}}}

	if got := Unanswerable([]*session.Event{confirmation}); len(got) != 0 {
		t.Errorf("a confirmation was reported as unanswerable: %v", got)
	}
	got := Unanswerable([]*session.Event{confirmation, other})
	if len(got) != 1 || got[0] != "adk_request_workflow_input" {
		t.Errorf("Unanswerable = %v, want the workflow input request", got)
	}
}

func TestAnswerIsOneUserMessageKeyedToTheInterrupt(t *testing.T) {
	got := Answer([]Decision{
		{Request: Request{ID: "c1"}, Approved: true},
		{Request: Request{ID: "c2"}, Approved: false},
	})
	if got.Role != genai.RoleUser {
		// The resume processor only inspects the most recent user-authored
		// event; any other role and the paused call is never re-dispatched.
		t.Errorf("Role = %q, want %q", got.Role, genai.RoleUser)
	}
	if len(got.Parts) != 2 {
		t.Fatalf("%d parts, want 2", len(got.Parts))
	}
	for i, want := range []struct {
		id string
		ok bool
	}{{"c1", true}, {"c2", false}} {
		fr := got.Parts[i].FunctionResponse
		if fr == nil || fr.ID != want.id {
			t.Fatalf("part %d = %+v, want a response to %q", i, got.Parts[i], want.id)
		}
		if fr.Name != toolconfirmation.FunctionCallName {
			t.Errorf("part %d name = %q, want %q", i, fr.Name, toolconfirmation.FunctionCallName)
		}
		if fr.Response["confirmed"] != want.ok {
			t.Errorf("part %d confirmed = %v, want %v", i, fr.Response["confirmed"], want.ok)
		}
	}
}

// Only an explicit yes approves. Everything else — including a reviewer who
// closes the terminal — declines.
func TestPromptApprovesOnlyOnAnExplicitYes(t *testing.T) {
	req := Request{ID: "c1", Tool: "kubectl_delete_pod", Agent: "change-executor",
		Hint: "Approve this change to cluster \"kind-sre-eval-a1\"?"}

	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"yes\n", true},
		{"y\n", true},
		{"  YES  \n", true},
		{"no\n", false},
		{"\n", false},
		{"maybe\n", false},
		{"", false}, // EOF
	} {
		var out strings.Builder
		got, err := Prompt(strings.NewReader(tc.in), &out).Approve(context.Background(), req)
		if err != nil {
			t.Errorf("Approve(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("Approve(%q) = %v, want %v", tc.in, got, tc.want)
		}
		if !strings.Contains(out.String(), req.Hint) {
			t.Errorf("Approve(%q) did not show the reviewer the hint: %q", tc.in, out.String())
		}
	}
}

// A tool that wrote no hint still has to be reviewable. Showing the raw call is
// worse than a written hint and much better than an empty prompt.
func TestPromptFallsBackToTheCallWhenThereIsNoHint(t *testing.T) {
	var out strings.Builder
	Prompt(strings.NewReader("no\n"), &out).Approve(context.Background(), Request{
		Tool: "kubectl_delete_pod", Args: map[string]any{"pod_name": "web-1"}})
	if !strings.Contains(out.String(), "kubectl_delete_pod") || !strings.Contains(out.String(), "web-1") {
		t.Errorf("the prompt says nothing about the call: %q", out.String())
	}
}

// interruptEvent builds the event ADK synthesises for a pending confirmation.
func interruptEvent(author, id string, args map[string]any) *session.Event {
	return &session.Event{
		Author:             author,
		LongRunningToolIDs: []string{id},
		LLMResponse: model.LLMResponse{Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{
				ID: id, Name: toolconfirmation.FunctionCallName, Args: args,
			}}},
		}},
	}
}
