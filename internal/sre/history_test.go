package sre

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/k8s-sre-agent/internal/lookout"
)

// Does a parallel delegation leave a dangling tool call in the history?
//
// Anthropic rejects a request in which an assistant message holds N tool_use
// blocks and the *immediately following* message does not carry all N matching
// tool_results. mast's provider maps genai Contents to Anthropic messages one
// for one (pkg/providers/anthropic/convert.go), so that invariant has to hold
// in the genai history ADK assembles, or the request 400s before it is sent.
//
// It did not. 16 of 31 tier-1 examples died on
//
//	messages.4: `tool_use` ids were found without `tool_result` blocks
//	immediately after: toolu_...
//
// once delegation started working and the orchestrator began issuing a
// specialist call and a lookout call in the same turn. This test reproduces it
// without a provider: it drives the real Build() wiring with a scripted model
// and asserts the pairing invariant on the genai history itself.
func TestParallelDelegationKeepsCallsAndResponsesPaired(t *testing.T) {
	offline, _, err := lookout.Offline()
	if err != nil {
		t.Fatalf("offline toolset: %v", err)
	}

	orch := &scriptedModel{name: "orch", turns: [][]*genai.Part{
		// Turn 1: a delegation and an ordinary cluster read, together. This is
		// the shape the orchestrator settles into once it can actually
		// delegate — fan out and read at the same time.
		{
			{FunctionCall: &genai.FunctionCall{ID: "c1", Name: "pod-inspector",
				Args: map[string]any{"request": "inspect production"}}},
			{FunctionCall: &genai.FunctionCall{ID: "c2", Name: "k8s_cluster_health",
				Args: map[string]any{}}},
		},
		// Turn 2: finish, so the run terminates.
		{{FunctionCall: &genai.FunctionCall{ID: "c3", Name: ReportToolName, Args: map[string]any{
			"overall_severity": "ok",
			"summary":          "no issues",
			"findings":         []any{},
		}}}},
		{{Text: "done"}},
	}}
	sub := &scriptedModel{name: "sub", turns: [][]*genai.Part{
		{{FunctionCall: &genai.FunctionCall{ID: "s1", Name: "finish_task", Args: map[string]any{
			"overall_severity": "ok",
			"summary":          "pods healthy",
			"findings":         []any{},
		}}}},
	}}

	root, err := Build(Config{Main: orch, Subagent: sub, Toolsets: []tool.Toolset{offline}})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rn, err := runner.NewInMemory("sre-history", root)
	if err != nil {
		t.Fatalf("NewInMemory: %v", err)
	}
	for _, err := range rn.Run(context.Background(), "u", "s",
		genai.NewContentFromText("assess production", genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	histories := orch.histories()
	if len(histories) < 2 {
		t.Fatalf("the orchestrator was called %d time(s); the second call is the one "+
			"that carries the paired history", len(histories))
	}
	// The last request is the one that replays both results.
	for i, h := range histories {
		if problems := unpairedToolCalls(h); len(problems) > 0 {
			t.Errorf("orchestrator request %d would be rejected by Anthropic:\n  %s\nhistory:\n%s",
				i, strings.Join(problems, "\n  "), renderHistory(h))
		}
	}
}

// unpairedToolCalls applies Anthropic's rule to a genai history: every function
// call in a message must be answered by a function response in the message
// immediately after it.
func unpairedToolCalls(contents []*genai.Content) []string {
	var problems []string
	for i, c := range contents {
		var called []string
		for _, p := range c.Parts {
			if p.FunctionCall != nil {
				called = append(called, p.FunctionCall.Name)
			}
		}
		if len(called) == 0 {
			continue
		}
		answered := map[string]int{}
		if i+1 < len(contents) {
			for _, p := range contents[i+1].Parts {
				if p.FunctionResponse != nil {
					answered[p.FunctionResponse.Name]++
				}
			}
		}
		for _, name := range called {
			if answered[name] == 0 {
				problems = append(problems, fmt.Sprintf(
					"contents[%d] calls %q with no response in contents[%d]", i, name, i+1))
				continue
			}
			answered[name]--
		}
	}
	return problems
}

func renderHistory(contents []*genai.Content) string {
	var b strings.Builder
	for i, c := range contents {
		fmt.Fprintf(&b, "  [%d] %s:", i, c.Role)
		for _, p := range c.Parts {
			switch {
			case p.FunctionCall != nil:
				fmt.Fprintf(&b, " call(%s)", p.FunctionCall.Name)
			case p.FunctionResponse != nil:
				fmt.Fprintf(&b, " resp(%s)", p.FunctionResponse.Name)
			case p.Text != "":
				fmt.Fprintf(&b, " text(%.30q)", p.Text)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// scriptedModel replays a fixed sequence of model turns and records the
// history it was handed each time.
type scriptedModel struct {
	name  string
	turns [][]*genai.Part

	mu    sync.Mutex
	calls int
	hist  [][]*genai.Content
}

func (m *scriptedModel) Name() string { return m.name }

func (m *scriptedModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.mu.Lock()
	m.hist = append(m.hist, req.Contents)
	n := m.calls
	m.calls++
	m.mu.Unlock()

	return func(yield func(*model.LLMResponse, error) bool) {
		if n >= len(m.turns) {
			yield(&model.LLMResponse{
				Content:      genai.NewContentFromText("out of script", genai.RoleModel),
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: m.turns[n]},
			TurnComplete: true,
		}, nil)
	}
}

func (m *scriptedModel) histories() [][]*genai.Content {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]*genai.Content(nil), m.hist...)
}

// The workaround must be surgical: it drops exactly the batched non-delegation
// calls and nothing else. Dropping a thinking part in particular would break
// the next Anthropic request in a different and more confusing way, since
// thinking blocks have to be replayed with their signatures intact.
func TestSerializeDelegationsDropsOnlyBatchedToolCalls(t *testing.T) {
	cb := serializeDelegations([]string{"pod-inspector", "log-analyzer"})

	call := func(name string) *genai.Part {
		return &genai.Part{FunctionCall: &genai.FunctionCall{Name: name}}
	}
	resp := func(parts ...*genai.Part) *model.LLMResponse {
		return &model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: parts}}
	}

	t.Run("mixed batch loses the ordinary calls", func(t *testing.T) {
		thought := &genai.Part{Text: "reasoning", Thought: true}
		in := resp(thought, call("pod-inspector"), call("k8s_cluster_health"), call("log-analyzer"))
		out, err := cb(nil, in, nil)
		if err != nil || out == nil {
			t.Fatalf("callback did not rewrite the batch: out=%v err=%v", out, err)
		}
		var got []string
		for _, p := range out.Content.Parts {
			if p.FunctionCall != nil {
				got = append(got, p.FunctionCall.Name)
			}
		}
		want := []string{"pod-inspector", "log-analyzer"}
		if !slices.Equal(got, want) {
			t.Errorf("kept calls %v, want %v", got, want)
		}
		if len(out.Content.Parts) != 3 || !out.Content.Parts[0].Thought {
			t.Errorf("the thinking part did not survive: %+v", out.Content.Parts)
		}
		// The original must be untouched: an event built from it may already
		// have been yielded.
		if len(in.Content.Parts) != 4 {
			t.Errorf("the input response was mutated: %d parts left", len(in.Content.Parts))
		}
	})

	for _, tc := range []struct {
		name string
		in   *model.LLMResponse
	}{
		{"no delegation", resp(call("k8s_cluster_health"), call("k8s_triage_workload"))},
		{"delegations only", resp(call("pod-inspector"), call("log-analyzer"))},
		{"no calls at all", resp(&genai.Part{Text: "hello"})},
	} {
		t.Run(tc.name+" is left alone", func(t *testing.T) {
			out, err := cb(nil, tc.in, nil)
			if out != nil || err != nil {
				t.Errorf("rewrote a response it should not have: out=%v err=%v", out, err)
			}
		})
	}
}
