package sre

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/mast/pkg/specialists"

	"github.com/go-steer/core-sre-agent/internal/lookout"
)

// The write path is a capability the caller grants, not a flag it clears.
//
// Both halves matter. Without writes the specialist must not exist at all —
// building an empty shell would leave the orchestrator with a delegation target
// its own spec tells it to route mutations to, and a specialist that answers
// "I have no tools" is indistinguishable in a report from one that tried. With
// writes it must exist, or the tools are held by nothing and the whole gate is
// unreachable.
func TestTheWriteSpecialistExistsOnlyWhenWritesAreGranted(t *testing.T) {
	offline, _, err := lookout.Offline()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("no writes, no specialist", func(t *testing.T) {
		root, err := Build(Config{Main: stubModel{}, Toolsets: []tool.Toolset{offline}})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if got := names(root.SubAgents()); slices.Contains(got, WriteAgentName) {
			t.Errorf("%s was built with no write tools configured: %v", WriteAgentName, got)
		}
	})

	t.Run("writes, specialist", func(t *testing.T) {
		root, err := Build(Config{
			Main:     stubModel{},
			Toolsets: []tool.Toolset{offline},
			Writes:   []tool.Tool{fakeWrite(t, "kubectl_scale_deployment")},
		})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if got := names(root.SubAgents()); !slices.Contains(got, WriteAgentName) {
			t.Errorf("write tools were configured but %s was not built: %v", WriteAgentName, got)
		}
	})
}

// Writes with nowhere to go is a misconfiguration, not a read-only agent.
// Silently dropping them would produce exactly the deployment an operator
// thought they were avoiding — one that looks like it can remediate and
// cannot — so this fails at Build rather than at the first mutation.
func TestWriteToolsWithNoSpecToHoldThemIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeSpec(t, dir, OrchestratorName+".tmpl", `---
name: `+OrchestratorName+`
description: coordinates
mode: Task
model: main
---
Coordinate.
`)
	writeSpec(t, dir, "pod-inspector.tmpl", `---
name: pod-inspector
description: inspects pods
mode: Task
model: subagent
---
Inspect pods.
`)

	_, err := Build(Config{
		Main:    stubModel{},
		SpecDir: dir,
		Writes:  []tool.Tool{fakeWrite(t, "kubectl_scale_deployment")},
	})
	if err == nil {
		t.Fatalf("write tools were accepted by a spec set with no %q", WriteAgentName)
	}
	if !strings.Contains(err.Error(), WriteAgentName) {
		t.Errorf("error does not name the missing spec: %v", err)
	}
}

// The spec allowlist is the operator's knob on the write roster, and it turns
// in one direction only. Subtracting is a legitimate deployment choice;
// adding would make the code-side binding advisory, since the spec directory
// is editable and replaceable wholesale via Config.SpecDir.
func TestASpecCanOnlySubtractWriteTools(t *testing.T) {
	scale := fakeWrite(t, "kubectl_scale_deployment")
	del := fakeWrite(t, "kubectl_delete_pod")
	offered := []tool.Tool{scale, del}

	t.Run("no allowlist means the whole roster", func(t *testing.T) {
		got, err := writeTools(allowlistSpec(nil), offered)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Errorf("got %d tools, want 2", len(got))
		}
	})

	t.Run("an allowlist narrows it", func(t *testing.T) {
		got, err := writeTools(allowlistSpec([]string{"kubectl_scale_deployment"}), offered)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Name() != "kubectl_scale_deployment" {
			t.Errorf("got %v, want just kubectl_scale_deployment", toolNames(got))
		}
	})

	t.Run("a name outside the roster is an error", func(t *testing.T) {
		// Including one that exists in the real kubewrite surface but was not
		// granted here: the allowlist resolves against what the caller offered,
		// not against what the package can build.
		_, err := writeTools(allowlistSpec([]string{"kubectl_cordon_node"}), offered)
		if err == nil {
			t.Fatal("a spec granted itself a tool the caller did not offer")
		}
		if !strings.Contains(err.Error(), "kubectl_cordon_node") {
			t.Errorf("error does not name the offending tool: %v", err)
		}
	})

	t.Run("a typo is an error, not a silent drop", func(t *testing.T) {
		if _, err := writeTools(allowlistSpec([]string{"kubectl_scale_deploymnet"}), offered); err == nil {
			t.Fatal("a misspelled allowlist entry silently removed a write tool")
		}
	})
}

// The structural guarantee, asserted where the model can see it: exactly one
// agent in the whole tree is offered a write tool.
//
// This reads the LLMRequest rather than the config for the same reason
// TestSpecialistsAreNotOfferedTransfer does — the declaration surface is what
// the model acts on, ADK assembles it at request time, and this repo has twice
// shipped wiring that looked correct in Go and was inert on the wire.
func TestOnlyTheWriteSpecialistIsOfferedWriteTools(t *testing.T) {
	offline, _, err := lookout.Offline()
	if err != nil {
		t.Fatal(err)
	}
	const writeName = "kubectl_scale_deployment"

	roster, err := SpecialistNames("")
	if err != nil {
		t.Fatal(err)
	}
	// Delegate to every specialist at once, so no agent passes this by never
	// having been asked anything.
	fanout := make([]*genai.Part, 0, len(roster))
	for i, name := range roster {
		fanout = append(fanout, &genai.Part{FunctionCall: &genai.FunctionCall{
			ID: string(rune('a' + i)), Name: name, Args: map[string]any{"request": "go"},
		}})
	}

	m := &rosterModel{turns: map[string][][]*genai.Part{
		OrchestratorName: {
			fanout,
			{{FunctionCall: &genai.FunctionCall{ID: "r", Name: ReportToolName, Args: map[string]any{
				"overall_severity": "ok", "summary": "no issues", "findings": []any{},
			}}}},
			{{Text: "done"}},
		},
	}}
	root, err := Build(Config{
		Main:     m,
		Subagent: m,
		Toolsets: []tool.Toolset{offline},
		Writes:   []tool.Tool{fakeWrite(t, writeName)},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rn, err := runner.NewInMemory("sre-write-wiring", root)
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range rn.Run(context.Background(), "u", "s",
		genai.NewContentFromText("scale web to 5", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	seen := m.agents()
	for _, want := range append(roster, OrchestratorName) {
		if !slices.Contains(seen, want) {
			t.Errorf("%q never reached the model, so it was not checked", want)
		}
	}
	for _, name := range seen {
		held := m.declared(name)[writeName] != nil
		switch {
		case name == WriteAgentName && !held:
			t.Errorf("%s does not declare %s; the write tools are held by nobody and "+
				"the approval gate is unreachable", name, writeName)
		case name != WriteAgentName && held:
			t.Errorf("%s declares %s — every mutation is supposed to go through %s",
				name, writeName, WriteAgentName)
		}
	}
}

func allowlistSpec(builtin []string) specialists.Spec {
	s := specialists.Spec{}
	s.Name = WriteAgentName
	s.Tools.Builtin = builtin
	return s
}

func toolNames(ts []tool.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

func writeSpec(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeWrite stands in for a kubewrite tool. The real ones are HITL-gated and
// live in another package; nothing here is testing what they do, only who is
// allowed to see them.
func fakeWrite(t *testing.T, name string) tool.Tool {
	t.Helper()
	type args struct {
		Deployment string `json:"deployment"`
	}
	tl, err := functiontool.New(
		functiontool.Config{Name: name, Description: "mutates the cluster"},
		func(adkagent.Context, args) (string, error) { return "applied", nil })
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

// rosterModel answers as whichever agent is calling and records the tool
// declarations each one was offered.
//
// It keys on the name ADK stamps into every system instruction, because that
// is the only per-agent discriminator an LLMRequest carries — the struct itself
// has no identity field, so a single shared model stub otherwise cannot tell
// the orchestrator's request from a specialist's. Any agent with no script
// finishes its task immediately, which is what keeps a fan-out to nine
// specialists from needing nine scripts.
type rosterModel struct {
	turns map[string][][]*genai.Part

	mu    sync.Mutex
	calls map[string]int
	tools map[string]map[string]*genai.FunctionDeclaration
	order []string
}

func (*rosterModel) Name() string { return "roster" }

func (m *rosterModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	name := agentOf(req)

	m.mu.Lock()
	if m.tools == nil {
		m.tools = map[string]map[string]*genai.FunctionDeclaration{}
		m.calls = map[string]int{}
	}
	if _, ok := m.tools[name]; !ok {
		m.tools[name] = map[string]*genai.FunctionDeclaration{}
		m.order = append(m.order, name)
	}
	if req.Config != nil {
		for _, t := range req.Config.Tools {
			for _, fd := range t.FunctionDeclarations {
				m.tools[name][fd.Name] = fd
			}
		}
	}
	n := m.calls[name]
	m.calls[name]++
	m.mu.Unlock()

	parts := []*genai.Part{{FunctionCall: &genai.FunctionCall{
		ID: name + "-done", Name: "finish_task", Args: map[string]any{
			"overall_severity": "ok", "summary": "nothing to report", "findings": []any{},
		}}}}
	if script, ok := m.turns[name]; ok {
		if n >= len(script) {
			parts = []*genai.Part{{Text: "out of script"}}
		} else {
			parts = script[n]
		}
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: parts},
			TurnComplete: true,
		}, nil)
	}
}

func (m *rosterModel) declared(agent string) map[string]*genai.FunctionDeclaration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tools[agent]
}

func (m *rosterModel) agents() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.order...)
}

// agentOf reads back the identity ADK writes into every system instruction
// ("You are an agent. Your internal name is %q." —
// internal/llminternal/identity_request_processor.go).
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
