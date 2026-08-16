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

package lookout

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"
)

// toolsJSON is lookout's advertised MCP tool surface, captured from a live
// `lookout mcp` handshake. It exists so the fixed eval tier can present the
// real tool surface — same names, same descriptions, same input schemas —
// without a cluster or even the lookout binary.
//
// Descriptions are the load-bearing part: they are what the model reads when
// choosing a tool, so a paraphrase would measure a tool surface we do not
// ship. Regenerate with `go run ./dev/captureschema` after changing lookout.
//
//go:embed tools.json
var toolsJSON []byte

// mcpToolSpec is one entry of the captured tools/list response.
type mcpToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`

	// Annotations carries readOnlyHint, which is a safety input rather than
	// documentation — see Surface. It is captured so the writer classification
	// can be tested against checked-in data, with no binary and no cluster;
	// the live check still asks the binary.
	Annotations *struct {
		ReadOnlyHint bool `json:"readOnlyHint"`
	} `json:"annotations,omitempty"`
}

// OfflineMessage is what every offline tool returns in place of telemetry.
//
// It states plainly that no cluster is attached and points the agent back at
// the scenario text. The alternative — returning an error — makes the model
// retry, burning turns and polluting the trajectory with duplicates of the
// same intent.
//
// It has to say that the absence is permanent, not just present. An earlier
// version stopped at "continue your analysis from the description", and a
// specialist read that as an invitation to list the telemetry it wanted and
// wait: it enumerated the spec, logs and events it could not see and asked for
// them. Nobody answers, so the specialist never reports, and a specialist that
// never reports takes the orchestrator's whole turn down with it — ADK ends a
// delegation that produced no output by ending the caller's turn. The run then
// yields no health report at all, which grades as a total failure of an agent
// that was in fact reasoning correctly about missing data.
//
// It also has to say, in the same breath, that the investigation should not be
// *shortened*. The version that fixed the stall said "record this tool as the
// one you would run, then complete your analysis" — which is an instruction to
// make exactly one call. The agent complied: mean calls per example dropped to
// 1.7 and `tool_coverage` fell 0.875 → 0.580, in the one tier whose entire
// signal is which checks the agent chooses. A substrate that tells the agent to
// stop investigating cannot measure investigation. Hence the dry-run framing:
// the absence of data is permanent, the sequence of checks is not curtailed by
// it.
const OfflineMessage = "OFFLINE SCENARIO: no cluster is attached, so this check returns no telemetry. " +
	"This is permanent for this run: no tool will return cluster data, and no one will supply it if " +
	"you ask — a request for more information will not be answered, so a turn spent asking is a turn " +
	"wasted. Treat this as a dry run, not as a shortened one: work through the same sequence of checks " +
	"you would run against a live cluster, calling each one in turn and reasoning about what it would " +
	"have shown, rather than stopping after the first check comes back empty. Draw your conclusions " +
	"from the situation described in the user's message, and note any evidence you would have wanted " +
	"as a gap in the report rather than as a question."

// Offline returns a toolset with lookout's exact tool surface whose tools
// perform no I/O, and a Calls accessor recording every invocation in order.
//
// This is the fixed (tier-1) eval substrate. The 31 upstream scenarios are
// self-contained prose — the reference answers describe an end state that no
// tool could reach anyway ("Pulled logs — container exits with ...") — so the
// gradeable signal is which checks the agent *chooses*, plus the severity and
// grounding of its report. Offline measures exactly that and nothing more,
// which is why the live tier exists separately.
func Offline() (tool.Toolset, *Recorder, error) {
	specs, err := capturedSpecs()
	if err != nil {
		return nil, nil, err
	}

	rec := &Recorder{}
	tools := make([]tool.Tool, 0, len(specs))
	for _, s := range specs {
		decl := &genai.FunctionDeclaration{Name: s.Name, Description: s.Description}
		if len(s.InputSchema) > 0 {
			var schema any
			if err := json.Unmarshal(s.InputSchema, &schema); err != nil {
				return nil, nil, fmt.Errorf("lookout: parse input schema for %s: %w", s.Name, err)
			}
			decl.ParametersJsonSchema = schema
		}
		tools = append(tools, &offlineTool{decl: decl, rec: rec})
	}
	return Named(ToolsetName, &staticToolset{name: ToolsetName, tools: tools}), rec, nil
}

// Captured returns the embedded surface as ToolInfo, for the checks that want
// the annotations rather than the tools. It is what the binary advertised the
// last time captureschema ran, which makes it the right input for a test and
// the wrong one for a runtime guard.
func Captured() ([]ToolInfo, error) {
	specs, err := capturedSpecs()
	if err != nil {
		return nil, err
	}
	out := make([]ToolInfo, 0, len(specs))
	for _, s := range specs {
		out = append(out, ToolInfo{
			Name:     s.Name,
			ReadOnly: s.Annotations != nil && s.Annotations.ReadOnlyHint,
		})
	}
	return out, nil
}

func capturedSpecs() ([]mcpToolSpec, error) {
	var specs []mcpToolSpec
	if err := json.Unmarshal(toolsJSON, &specs); err != nil {
		return nil, fmt.Errorf("lookout: parse captured tool schemas: %w", err)
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("lookout: captured tool schemas are empty")
	}
	return specs, nil
}

// Recorder is the trajectory the eval harness scores. Safe for concurrent
// use: ADK may run tool calls in parallel within a turn.
type Recorder struct {
	mu    sync.Mutex
	calls []Call
}

// Call is one recorded tool invocation.
type Call struct {
	Tool string `json:"tool"`
	Args any    `json:"args,omitempty"`
}

func (r *Recorder) add(c Call) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

// Calls returns the invocations in order.
func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// Names returns the invoked tool names in order, duplicates included.
// tool_coverage is set-based, but the duplicates are worth keeping: a run
// that calls the same check eight times is a different failure from one that
// calls it once, and only the ordered list shows that.
func (r *Recorder) Names() []string {
	calls := r.Calls()
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Tool
	}
	return out
}

// Reset clears the recorder between examples.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

type staticToolset struct {
	name  string
	tools []tool.Tool
}

func (s *staticToolset) Name() string { return s.name }

func (s *staticToolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) { return s.tools, nil }

// offlineTool mirrors mcptoolset's own tool shape — Declaration plus
// ProcessRequest via toolutils.PackTool — so ADK treats it identically to the
// live tool it stands in for.
type offlineTool struct {
	decl *genai.FunctionDeclaration
	rec  *Recorder
}

func (t *offlineTool) Name() string        { return t.decl.Name }
func (t *offlineTool) Description() string { return t.decl.Description }
func (t *offlineTool) IsLongRunning() bool { return false }

func (t *offlineTool) Declaration() *genai.FunctionDeclaration { return t.decl }

func (t *offlineTool) ProcessRequest(_ agent.Context, req *model.LLMRequest) error {
	return toolutils.PackTool(req, t)
}

func (t *offlineTool) Run(_ agent.Context, args any) (map[string]any, error) {
	t.rec.add(Call{Tool: t.decl.Name, Args: args})
	return map[string]any{"result": OfflineMessage}, nil
}

// compile-time proof that offlineTool satisfies everything ADK needs of a
// callable tool; the runnable interface itself is unexported in package tool.
var _ interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(agent.Context, any) (map[string]any, error)
	ProcessRequest(agent.Context, *model.LLMRequest) error
} = (*offlineTool)(nil)
