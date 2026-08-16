// Package sre assembles the SRE agent from specialist specs.
//
// The wiring lives here; the behaviour lives in internal/sre/specs/*.tmpl.
// (They sit under the package rather than at the repo root because go:embed
// cannot reach outside its own directory, and a shipped binary must not need
// sidecar files.) That split
// is the point of this repo — the nine upstream subagents are meant to become
// config, so anything that would need a new Go type per specialist belongs in
// the spec file instead.
package sre

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	mastagent "github.com/go-steer/mast/pkg/agent"
	"github.com/go-steer/mast/pkg/specialists"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/k8s-sre-agent/internal/schema"
)

// specFS embeds the shipped specs so a built binary needs no sidecar files.
// Config.SpecDir overrides it for operators who maintain their own.
//
//go:embed all:specs
var specFS embed.FS

// OrchestratorName is the spec that owns the top-level loop. It is the agent
// Build returns; everything else is reachable only through it.
const OrchestratorName = "sre-orchestrator"

// WriteAgentName is the one specialist that may hold mutating tools.
//
// The binding is in Go and not in the spec allowlist, which is a deliberate
// exception to this package's "specialists are the config surface" rule. The
// allowlist matches MCP toolsets by server name, so if writes were granted
// that way any spec could grant itself writes by naming the server — and the
// spec directory is the operator-editable surface, overridable wholesale with
// Config.SpecDir. Upstream's structural guarantee is that the main agent holds
// only read tools and every mutation goes through one interrupting subagent;
// a guarantee that a config file can revoke is not one.
//
// A spec named here may still *subtract*: see writeTools.
const WriteAgentName = "change-executor"

// Model tiers a spec may name in its `model:` frontmatter. Specs name a tier,
// not a model ID, so re-pointing a tier is a one-line change here rather than
// an edit across every spec.
const (
	TierMain     = "main"
	TierSubagent = "subagent"
)

// Config binds a spec set to concrete runtime resources.
type Config struct {
	// Main and Subagent are the two model tiers. Main is required;
	// an empty Subagent falls back to Main.
	Main     adkmodel.LLM
	Subagent adkmodel.LLM

	// Toolsets are offered to every specialist and filtered by each spec's
	// MCP allowlist. Normally the single lookout toolset.
	Toolsets []tool.Toolset

	// Writes are the mutating tools, normally kubewrite.Tools. They go to the
	// WriteAgentName specialist and to nothing else.
	//
	// Empty means the agent has no write path at all: the change-executor spec
	// is not built, so the orchestrator has no delegation target for a
	// mutation and cannot be talked into one. That is the right default for a
	// monitoring run, and it is why this is a capability the caller grants
	// rather than a flag the caller clears.
	Writes []tool.Tool

	// SpecDir loads specs from disk instead of the embedded set.
	SpecDir string
}

// Build assembles the orchestrator and returns a runnable root agent.
//
// Non-orchestrator specs become Task-mode sub-agents of the orchestrator —
// ADK's equivalent of upstream's task(agent="pod-inspector") delegation. The
// orchestrator carries a submit_health_report tool, so its result is a decoded
// struct rather than prose the monitor would have to parse.
//
// # Why the orchestrator is Chat mode and the specialists are sub-agents
//
// ADK sanctions exactly two ways to reach a Task-mode agent
// (workflow/validation.go's validateNoTaskModeGraphNodes): as a sub-agent of
// an LlmAgent coordinator, or dynamically via workflow.RunNode. agenttool is
// neither, and it fails in the least visible way possible: it spins up its own
// inner runner with the Task agent as root (tool/agenttool/agent_tool.go),
// the runner rejects any root that is not a Chat LlmAgent, and the refusal
// comes back as an ordinary FunctionResponse carrying an error string. The
// orchestrator reads that, shrugs, and does the work itself with its own
// lookout tools — so delegation looks like it happened on the wire while no
// specialist ever ran. TestDelegationActuallyExecutes exists because a
// delegation *count* cannot tell those apart.
//
// Sub-agent delegation is dispatched by runChat in ADK's LlmAgent wrapper, and
// only by runChat — runTask installs the delegation tools but never dispatches
// them. So the orchestrator has to be Chat mode, which also makes it a legal
// runner root directly and retires the one-node workflow wrapper this package
// used to need. The same runChat path is the one that propagates
// workflow.ErrNodeInterrupted out of a paused sub-agent, which is what the
// HITL write gate will be built on.
func Build(cfg Config) (adkagent.Agent, error) {
	if cfg.Main == nil {
		return nil, fmt.Errorf("sre: Config.Main model is required")
	}
	if cfg.Subagent == nil {
		cfg.Subagent = cfg.Main
	}

	specs, err := loadSpecs(cfg.SpecDir)
	if err != nil {
		return nil, err
	}

	orchestrator, others, err := partition(specs)
	if err != nil {
		return nil, err
	}

	subAgents := make([]adkagent.Agent, 0, len(others))
	sawWriteSpec := false
	for _, s := range others {
		var extra []tool.Tool
		if s.Name == WriteAgentName {
			sawWriteSpec = true
			// No writes configured: leave the specialist unbuilt rather than
			// building an empty shell the orchestrator is nonetheless told to
			// route mutations to. An agent that can only recommend should not
			// have a delegation target that promises otherwise.
			if len(cfg.Writes) == 0 {
				continue
			}
			extra, err = writeTools(s, cfg.Writes)
			if err != nil {
				return nil, err
			}
		}
		sub, err := buildOne(s, cfg, extra)
		if err != nil {
			return nil, err
		}
		subAgents = append(subAgents, sub)
	}
	if len(cfg.Writes) > 0 && !sawWriteSpec {
		return nil, fmt.Errorf("sre: %d write tools were configured but no spec is named %q, "+
			"so nothing can reach them", len(cfg.Writes), WriteAgentName)
	}

	return buildOrchestrator(*orchestrator, cfg, subAgents)
}

// writeTools narrows the write roster to what the spec allows.
//
// The spec's `tools.builtin` list can only *subtract*, because every name it
// gives has to resolve against the tools the caller supplied — an operator can
// take kubectl_apply_manifest away from this deployment without touching Go,
// and cannot add anything that was not granted in code. An unrecognised name
// is an error rather than a silent drop: a typo that quietly removes a write
// tool looks exactly like a model that chose not to use it.
func writeTools(spec specialists.Spec, offered []tool.Tool) ([]tool.Tool, error) {
	if len(spec.Tools.Builtin) == 0 {
		return offered, nil
	}
	byName := make(map[string]tool.Tool, len(offered))
	names := make([]string, 0, len(offered))
	for _, t := range offered {
		byName[t.Name()] = t
		names = append(names, t.Name())
	}
	out := make([]tool.Tool, 0, len(spec.Tools.Builtin))
	for _, n := range spec.Tools.Builtin {
		t, ok := byName[n]
		if !ok {
			return nil, fmt.Errorf("sre: spec %q allows unknown write tool %q; the roster is %s",
				spec.Name, n, strings.Join(names, ", "))
		}
		out = append(out, t)
	}
	return out, nil
}

// buildOrchestrator builds the coordinator that owns the top-level loop.
//
// It bypasses both specialists.Build and mast's NewCoordinator for the same
// reason buildOne bypasses specialists.Build: neither can pass an OutputSchema,
// and the structured HealthReport is the contract this repo ships. mast's spec
// loader also has no Chat mode to declare (it accepts Task and SingleTurn
// only), so the orchestrator's mode is a property of its role rather than of
// its frontmatter — partition already singles it out by name.
//
// A Chat agent has no finish_task, and llmagent.Config.OutputSchema is not a
// substitute: ADK only honours it for Gemini models and silently degrades to
// prose for everything else. The orchestrator therefore carries an explicit
// report tool instead — see ReportToolName for the measurement that forced it.
// OutputSchema is deliberately left unset rather than set-and-ignored, so
// there is exactly one carrier and no second one that would appear if the
// model tier ever changed vendor.
func buildOrchestrator(spec specialists.Spec, cfg Config, subAgents []adkagent.Agent) (adkagent.Agent, error) {
	model, err := resolveModel(spec, cfg)
	if err != nil {
		return nil, err
	}
	return llmagent.New(llmagent.Config{
		Name:        spec.Name,
		Description: spec.Description,
		Instruction: spec.Instruction,
		Model:       model,
		Tools:       []tool.Tool{reportTool()},
		Toolsets:    allow(spec, cfg.Toolsets),
		SubAgents:   subAgents,
		Mode:        llmagent.ModeChat,
		// Works around an ADK defect that loses an ordinary tool call batched
		// into the same turn as a delegation. See serializeDelegations.
		AfterModelCallbacks: []llmagent.AfterModelCallback{
			serializeDelegations(agentNames(subAgents)),
		},
	})
}

// agentNames lists agent names in order.
func agentNames(as []adkagent.Agent) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Name())
	}
	return out
}

// SpecialistNames lists the specialists the orchestrator can delegate to.
// Exported for the eval harness, which needs the roster to tell a delegation
// call apart from an ordinary tool call — they have the same shape on the wire.
func SpecialistNames(specDir string) ([]string, error) {
	specs, err := loadSpecs(specDir)
	if err != nil {
		return nil, err
	}
	_, others, err := partition(specs)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(others))
	for _, s := range others {
		out = append(out, s.Name)
	}
	return out, nil
}

// partition splits the spec set into the orchestrator and the specialists that
// become tools on it. Separated from Build because it is the decision that
// determines the agent's capability surface, and ADK's Agent interface exposes
// no tool list — so this is the only place the wiring can be asserted on.
func partition(specs []specialists.Spec) (orchestrator *specialists.Spec, others []specialists.Spec, err error) {
	for i, s := range specs {
		if s.Name == OrchestratorName {
			if orchestrator != nil {
				return nil, nil, fmt.Errorf("sre: two specs named %q", OrchestratorName)
			}
			orchestrator = &specs[i]
			continue
		}
		others = append(others, s)
	}
	if orchestrator == nil {
		return nil, nil, fmt.Errorf("sre: no spec named %q among %d specs", OrchestratorName, len(specs))
	}
	return orchestrator, others, nil
}

// buildOne turns one spec into an agent.
//
// specialists.Build is bypassed for Task-mode specs, and for one reason only:
// it can pass an OutputSchema but only the one a spec declares in frontmatter,
// and this repo's contract is schema.ReportSchema() — a Go value shared with
// the orchestrator's report tool. Restating it as YAML in eight spec files
// would fork the contract into nine copies. Everything else this branch used to
// need from llmagent.New is now on TaskAgentConfig: the transfer flags came
// with mast#126 and the callback seam with mast#128, so what is left is a
// NewTaskAgent call with an OutputSchema.
//
// # Why every Task specialist disallows transfer
//
// ADK offers a specialist two ways out: finish_task, which returns a structured
// report to the orchestrator, and transfer_to_agent, which hands the
// conversation somewhere else. The second one is always wrong here and is
// usually fatal.
//
// Always wrong, because delegation in this design is one-way. The orchestrator
// asks a question and merges the answer; a specialist that transfers laterally
// or upward abandons the question with no report, and the merge has nothing to
// merge. Nothing in the eight specs ever wants that.
//
// Usually fatal, because of how the transfer is executed. transferTargets
// (internal/llminternal/agent_transfer.go:185) skips Task-mode agents via
// isUntransferableMode, so a specialist's peers are not targets and it has no
// sub-agents — the *only* target it is ever offered is the Chat-mode
// orchestrator. ADK forwards that transfer in-process, so the orchestrator's
// runChat runs under the specialist's node context; workflow/agent_node.go:104
// rebuilds that context with no SubScheduler, and runChat's first act is to
// re-dispatch the still-unresolved delegation FC through workflow.RunNode,
// which returns ErrInvalidRunNodeContext. The whole run dies:
//
//	workflow: dynamic child sre-orchestrator/pod-inspector@toolu_…:
//	workflow: dynamic child failed: workflow: RunNode called outside a dynamic node
//
// That is not hypothetical. It killed 2 of 7 tier-2 fixtures on each of two
// runs — different fixtures each time, which is what a model-dependent choice
// looks like from the outside and is why it read as an ADK flake for a while.
// TestSpecialistsCannotTransfer pins the declaration surface;
// TestTransferFromASpecialistIsFatal reproduces the crash against a scripted
// model, so it will start failing if ADK ever fixes the context rebuild.
//
// Setting both flags also removes the transfer instruction block and its tool
// declaration from every specialist request, which is a small token win and a
// smaller prompt surface for the model to get ideas from.
func buildOne(spec specialists.Spec, cfg Config, extraTools []tool.Tool) (adkagent.Agent, error) {
	model, err := resolveModel(spec, cfg)
	if err != nil {
		return nil, err
	}

	switch spec.Mode {
	case specialists.ModeTask, "":
		// An empty Instruction falls back to mastagent.DefaultTaskInstruction
		// inside NewTaskAgent; a non-empty one is used verbatim.
		return mastagent.NewTaskAgent(mastagent.TaskAgentConfig{
			Name:        spec.Name,
			Description: spec.Description,
			Instruction: spec.Instruction,
			Model:       model,
			Tools:       extraTools,
			Toolsets:    allow(spec, cfg.Toolsets),
			// Every Task specialist reports in the HealthReport shape,
			// subagents included: the orchestrator's job is to merge
			// findings, and a common shape is what makes merging
			// mechanical instead of another parsing problem.
			OutputSchema: schema.ReportSchema(),
			// See this function's doc comment. specialists.Build sets these
			// two as well, since the hazard is a property of any Task
			// specialist under a Chat coordinator rather than of this repo;
			// they are named here only because this branch does not go
			// through it.
			DisallowTransferToParent: true,
			DisallowTransferToPeers:  true,
			// The third way a specialist can end the run, and the only one
			// left: stop without reporting. The guard is mast's and the
			// payload is ours — see stallReport for which half is which.
			AfterModelCallbacks: []llmagent.AfterModelCallback{
				mastagent.FinishOnStall(spec.Name, stallReport),
			},
		})
	default:
		// SingleTurn and any future mode: hand back to mast, which owns the
		// mode table. Only Task mode needs the OutputSchema detour.
		return specialists.Build(spec, specialists.BuildOptions{
			Model:    model,
			Tools:    extraTools,
			Toolsets: cfg.Toolsets,
		})
	}
}

// allow applies the spec's MCP allowlist to the offered toolsets.
//
// mast's filterToolsets is unexported, so this reimplements its semantics:
// no allowlist means everything; an allowlist names servers by toolset name,
// and an empty tool list under a server means all of that server's tools.
func allow(spec specialists.Spec, offered []tool.Toolset) []tool.Toolset {
	if len(spec.Tools.MCP) == 0 {
		return offered
	}
	byServer := make(map[string]specialists.MCPAllowlist, len(spec.Tools.MCP))
	for _, al := range spec.Tools.MCP {
		byServer[al.Server] = al
	}
	var out []tool.Toolset
	for _, ts := range offered {
		al, ok := byServer[ts.Name()]
		if !ok {
			continue
		}
		if len(al.Tools) == 0 {
			out = append(out, ts)
			continue
		}
		out = append(out, tool.FilterToolset(ts, tool.AllowedToolsPredicate(al.Tools)))
	}
	return out
}

// resolveModel maps a spec's model tier onto a concrete LLM. An unknown tier
// is an error rather than a silent fallback: quietly running a subagent on
// the expensive tier is the kind of mistake that shows up on a bill.
func resolveModel(spec specialists.Spec, cfg Config) (adkmodel.LLM, error) {
	switch spec.Model {
	case TierMain, "":
		return cfg.Main, nil
	case TierSubagent:
		return cfg.Subagent, nil
	default:
		return nil, fmt.Errorf("sre: spec %q names unknown model tier %q (want %q or %q)",
			spec.Name, spec.Model, TierMain, TierSubagent)
	}
}

// loadSpecs reads specs from dir, or from the embedded set when dir is empty.
func loadSpecs(dir string) ([]specialists.Spec, error) {
	if dir != "" {
		specs, err := specialists.LoadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("sre: load specs from %s: %w", dir, err)
		}
		return specs, nil
	}

	// specialists.LoadDir takes a filesystem path, so the embedded specs are
	// staged into a temp dir. Cheap (a handful of small files, once at
	// startup) and it keeps a single code path for parsing.
	tmp, err := os.MkdirTemp("", "sre-specs-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	entries, err := specFS.ReadDir("specs")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		body, err := specFS.ReadFile(filepath.Join("specs", e.Name()))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(tmp, e.Name()), body, 0o600); err != nil {
			return nil, err
		}
	}
	specs, err := specialists.LoadDir(tmp)
	if err != nil {
		return nil, fmt.Errorf("sre: load embedded specs: %w", err)
	}
	return specs, nil
}
