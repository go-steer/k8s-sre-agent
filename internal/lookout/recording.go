package lookout

import (
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"
)

// Recording wraps a toolset so every call lands in rec.
//
// The offline toolset records natively because we wrote its tools. The live
// one comes from ADK's mcptoolset, whose tools are unexported, so the
// trajectory has to be captured by interposition.
//
// Worth having on the live tier even though tier 2 does not score the
// trajectory: when an agent misses an injected fault, the only way to tell
// "looked in the wrong place" from "looked in the right place and misread it"
// is the list of checks it ran.
func Recording(ts tool.Toolset, rec *Recorder) tool.Toolset {
	return &recordingToolset{inner: ts, rec: rec}
}

type recordingToolset struct {
	inner tool.Toolset
	rec   *Recorder
}

func (r *recordingToolset) Name() string { return r.inner.Name() }

func (r *recordingToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	inner, err := r.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tool.Tool, 0, len(inner))
	for _, t := range inner {
		rt, ok := t.(runnableTool)
		if !ok {
			// Not runnable through the interface we can interpose on. Pass it
			// through unwrapped rather than dropping it: an unrecorded tool is
			// a gap in the trajectory, but a missing tool is a gap in the
			// agent's capability, and the second is much worse.
			out = append(out, t)
			continue
		}
		out = append(out, &recordingTool{runnableTool: rt, rec: r.rec})
	}
	return out, nil
}

// runnableTool mirrors ADK's own unexported interface of the same name
// (tool/tool.go). Redeclared because it is not exported, not because the shape
// is in any doubt — ADK's confirmationTool wraps tools through exactly this.
type runnableTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(ctx agent.Context, args any) (map[string]any, error)
}

type recordingTool struct {
	runnableTool
	rec *Recorder
}

func (t *recordingTool) Declaration() *genai.FunctionDeclaration {
	return t.runnableTool.Declaration()
}

// ProcessRequest follows confirmationTool's shape exactly. The inner tool packs
// *itself* into req.Tools, so a wrapper that only delegated would be bypassed
// at call time and would record nothing while appearing to work.
func (t *recordingTool) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	if rp, ok := t.runnableTool.(interface {
		ProcessRequest(ctx agent.Context, req *model.LLMRequest) error
	}); ok {
		_, existedBefore := req.Tools[t.Name()]
		if err := rp.ProcessRequest(ctx, req); err != nil {
			return err
		}
		if !existedBefore && req.Tools != nil && req.Tools[t.Name()] != nil {
			req.Tools[t.Name()] = t
			return nil
		}
	}
	return toolutils.PackTool(req, t)
}

// Run records before delegating. Recorded before rather than after so that a
// check which hangs or errors still appears in the trajectory — those are
// exactly the runs whose trajectory you want to read.
func (t *recordingTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	t.rec.add(Call{Tool: t.Name(), Args: args})
	return t.runnableTool.Run(ctx, args)
}

var _ interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(agent.Context, any) (map[string]any, error)
	ProcessRequest(agent.Context, *model.LLMRequest) error
} = (*recordingTool)(nil)

// The offline tool must stay interposable too. The two tiers are meant to be
// swappable, and Recording silently passes through anything it cannot wrap —
// so without this the offline trajectory could go quiet with no build error.
var _ runnableTool = (*offlineTool)(nil)
