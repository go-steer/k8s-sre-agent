package sre

import (
	"context"
	"iter"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/specialists"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/k8s-sre-agent/internal/kuberead"
	"github.com/go-steer/k8s-sre-agent/internal/lookout"
)

// expectedSpecs is the specialist roster. Listed here rather than derived from
// the directory so that deleting a spec file fails a test instead of quietly
// shrinking the agent's capability.
//
// This is the roster on *disk*, which is not the same as the roster Build
// wires up: change-executor exists only when the caller grants write tools.
// See readSpecialists.
var expectedSpecs = []string{
	OrchestratorName,
	WriteAgentName,
	"config-auditor",
	"job-inspector",
	"log-analyzer",
	"performance-analyzer",
	"pod-inspector",
	"reliability-auditor",
	"scaling-analyzer",
	"security-auditor",
}

// readSpecialists is the roster Build produces when no write tools are granted:
// everything except the write-holding specialist, which is deliberately left
// unbuilt so a monitoring deployment has no delegation target for a mutation
// at all. Most tests in this package build that way.
func readSpecialists(t *testing.T) []string {
	t.Helper()
	all, err := SpecialistNames("")
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(all, func(n string) bool { return n == WriteAgentName })
}

func TestEmbeddedSpecsLoad(t *testing.T) {
	specs, err := loadSpecs("")
	if err != nil {
		t.Fatalf("loadSpecs: %v", err)
	}

	var got []string
	for _, s := range specs {
		got = append(got, s.Name)
		if s.Description == "" {
			t.Errorf("%s: no description — it is what the orchestrator routes on", s.Name)
		}
		if strings.TrimSpace(s.Instruction) == "" {
			t.Errorf("%s: empty instruction", s.Name)
		}
	}
	sort.Strings(got)

	want := append([]string(nil), expectedSpecs...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("specs = %v, want %v", got, want)
	}
}

// Every specialist must name a server the harness actually supplies and tools
// that server actually serves. Both halves are silent when wrong: allow()
// skips a server it was not offered and FilterToolset yields nothing for a name
// that does not exist, so the specialist runs with no tools and reports that it
// found nothing wrong.
func TestSpecAllowlistsNameRealTools(t *testing.T) {
	specs, err := loadSpecs("")
	if err != nil {
		t.Fatalf("loadSpecs: %v", err)
	}

	ctx := readonlyCtx{context.Background()}
	lookoutTS, _, err := lookout.Offline()
	if err != nil {
		t.Fatalf("offline toolset: %v", err)
	}
	// Both offline surfaces, keyed the way allow() keys them. Adding a toolset
	// to the harness without adding it here makes this test the thing that
	// notices.
	served := map[string]map[string]bool{}
	for _, ts := range []tool.Toolset{lookoutTS, kuberead.Offline("")} {
		tools, err := ts.Tools(ctx)
		if err != nil {
			t.Fatalf("list %s tools: %v", ts.Name(), err)
		}
		names := map[string]bool{}
		for _, tl := range tools {
			names[tl.Name()] = true
		}
		served[ts.Name()] = names
	}

	for _, s := range specs {
		for _, al := range s.Tools.MCP {
			names, ok := served[al.Server]
			if !ok {
				t.Errorf("%s: allowlists server %q, which no toolset provides", s.Name, al.Server)
				continue
			}
			for _, name := range al.Tools {
				if !names[name] {
					t.Errorf("%s: allowlists %q, which %s does not serve", s.Name, name, al.Server)
				}
			}
		}
	}
}

// TestEverySpecialistCanEnumerateANamespace.
//
// The read path had no enumeration primitive at all, and the cost was measured:
// fault-badselector failed both tier-2 runs because a specialist could not
// learn the name of the one Deployment in the namespace it was asked about, and
// the run died when it asked a human to run `kubectl get all` instead. Every
// detail-returning tool takes a `<Kind>/<namespace>/<name>` target, so a
// specialist without this one can only inspect objects something else already
// named.
//
// change-executor is the deliberate exception: it is handed one specific change
// with the object named, and a write agent that goes looking for other objects
// is not doing its job.
func TestEverySpecialistCanEnumerateANamespace(t *testing.T) {
	specs, err := loadSpecs("")
	if err != nil {
		t.Fatalf("loadSpecs: %v", err)
	}
	for _, s := range specs {
		grants := slices.ContainsFunc(s.Tools.MCP, func(al specialists.MCPAllowlist) bool {
			return al.Server == kuberead.ToolsetName &&
				(len(al.Tools) == 0 || slices.Contains(al.Tools, kuberead.ToolName))
		})
		if s.Name == WriteAgentName {
			if grants {
				t.Errorf("%s holds %s; it is handed the object to change, it does not go looking",
					s.Name, kuberead.ToolName)
			}
			continue
		}
		if !grants {
			t.Errorf("%s cannot enumerate a namespace; it can only inspect objects something else named",
				s.Name)
		}
	}
}

// The allowlist has to actually filter. If allow() returned the full toolset
// for a spec that named four tools, every specialist would silently hold all
// 22 checks and the config surface would be decorative.
func TestAllowlistFilters(t *testing.T) {
	specs, err := loadSpecs("")
	if err != nil {
		t.Fatalf("loadSpecs: %v", err)
	}
	offline, _, err := lookout.Offline()
	if err != nil {
		t.Fatalf("offline toolset: %v", err)
	}
	ctx := readonlyCtx{context.Background()}
	all, err := offline.Tools(ctx)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	total := len(all)

	var byName = map[string]specialists.Spec{}
	for _, s := range specs {
		byName[s.Name] = s
	}

	pod, ok := byName["pod-inspector"]
	if !ok {
		t.Fatal("pod-inspector spec missing")
	}
	sets := allow(pod, []tool.Toolset{offline})
	var n int
	for _, ts := range sets {
		got, err := ts.Tools(ctx)
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		n += len(got)
	}
	if want := len(pod.Tools.MCP[0].Tools); n != want {
		t.Errorf("pod-inspector sees %d tools, want %d (of %d offered)", n, want, total)
	}

	// The orchestrator names a server with no tool list, which means all of it.
	orch := byName[OrchestratorName]
	n = 0
	for _, ts := range allow(orch, []tool.Toolset{offline}) {
		got, err := ts.Tools(ctx)
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		n += len(got)
	}
	if n != total {
		t.Errorf("orchestrator sees %d tools, want all %d", n, total)
	}
}

// Every spec except the orchestrator must become a tool on it. ADK's Agent
// interface exposes no tool list, so this asserts on partition — the function
// that decides the set — rather than on the assembled agent.
func TestEverySpecialistBecomesAnOrchestratorTool(t *testing.T) {
	specs, err := loadSpecs("")
	if err != nil {
		t.Fatalf("loadSpecs: %v", err)
	}
	orch, others, err := partition(specs)
	if err != nil {
		t.Fatalf("partition: %v", err)
	}
	if orch.Name != OrchestratorName {
		t.Fatalf("partition returned %q as the orchestrator", orch.Name)
	}
	if got, want := len(others), len(expectedSpecs)-1; got != want {
		t.Errorf("%d specialists become tools, want %d", got, want)
	}
	for _, s := range others {
		// The write specialist is the exception, and on purpose: it is the only
		// one whose mistakes land on a real cluster, and writes are rare enough
		// that the tier difference costs nothing measurable. Every specialist
		// that can only read belongs on the cheap tier, which is the whole
		// reason there are two.
		want := TierSubagent
		if s.Name == WriteAgentName {
			want = TierMain
		}
		if s.Model != want {
			t.Errorf("%s runs on tier %q, want %q", s.Name, s.Model, want)
		}
	}
}

func TestPartitionRejectsAMissingOrchestrator(t *testing.T) {
	if _, _, err := partition([]specialists.Spec{{Name: "pod-inspector"}}); err == nil {
		t.Fatal("a spec set with no orchestrator was accepted")
	}
}

// Build must produce a root the runner will accept, with every specialist
// wired as a real sub-agent.
//
// Both halves are load-bearing and neither is cosmetic. The runner rejects any
// root that is not a Chat LlmAgent, and ADK dispatches delegation to a
// Task-mode sub-agent only from a Chat coordinator's run loop — a Task-mode
// orchestrator installs the delegation tools and then never fires them. The
// superseded wiring reached specialists through agenttool instead, which
// compiled, produced delegation function calls on the wire, and never ran a
// specialist; see the comment on Build. So this asserts the wiring that makes
// delegation possible, and TestDelegationActuallyExecutes asserts that it does.
func TestBuildProducesAChatRootWithSpecialistSubAgents(t *testing.T) {
	offline, _, err := lookout.Offline()
	if err != nil {
		t.Fatalf("offline toolset: %v", err)
	}
	root, err := Build(Config{
		Main:     stubModel{},
		Subagent: stubModel{},
		Toolsets: []tool.Toolset{offline},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if root.Name() != OrchestratorName {
		t.Errorf("root is %q, want %q", root.Name(), OrchestratorName)
	}
	// Exercise the real gate rather than asserting the mode field. The check
	// lives inline in runner.Run, so the only way to prove the runner accepts
	// this root is to hand it to one. The stub model yields no events, so the
	// run drains immediately; all this asserts is that it got past validation.
	rn, err := runner.NewInMemory("sre-wiring", root)
	if err != nil {
		t.Fatalf("NewInMemory: %v", err)
	}
	for _, err := range rn.Run(context.Background(), "u", "s",
		genai.NewContentFromText("ping", genai.RoleUser), agent.RunConfig{}) {
		if err != nil && strings.Contains(err.Error(), "must be a chat LlmAgent") {
			t.Fatalf("the runner rejected this root: %v", err)
		}
	}

	// No Config.Writes, so the write specialist is not among them.
	want := readSpecialists(t)
	got := names(root.SubAgents())
	sort.Strings(want)
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Fatalf("root sub-agents = %v, want %v", got, want)
	}
}

// Every specialist reports in the HealthReport shape, so a subagent's output
// can be merged rather than re-parsed. resolveModel must also refuse an unknown
// tier instead of silently running a subagent on the expensive tier.
func TestUnknownModelTierIsAnError(t *testing.T) {
	spec := specialists.Spec{Name: "x", Model: "gpt-9"}
	if _, err := resolveModel(spec, Config{Main: stubModel{}}); err == nil {
		t.Fatal("unknown model tier was accepted")
	}
}

func names(as []agent.Agent) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Name())
	}
	return out
}

// stubModel satisfies model.LLM for wiring tests. Build must not call it.
type stubModel struct{}

func (stubModel) Name() string { return "stub" }
func (stubModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {}
}

// readonlyCtx is the minimum agent.ReadonlyContext needed to list a toolset.
// ADK's own constructor lives in an internal package.
type readonlyCtx struct{ context.Context }

func (readonlyCtx) UserContent() *genai.Content          { return nil }
func (readonlyCtx) InvocationID() string                 { return "test" }
func (readonlyCtx) AgentName() string                    { return "test" }
func (readonlyCtx) ReadonlyState() session.ReadonlyState { return nil }
func (readonlyCtx) UserID() string                       { return "test" }
func (readonlyCtx) AppName() string                      { return "test" }
func (readonlyCtx) SessionID() string                    { return "test" }
func (readonlyCtx) Branch() string                       { return "" }
