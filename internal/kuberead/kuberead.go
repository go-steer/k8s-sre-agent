// Package kuberead is the enumeration primitive the read path was missing.
//
// internal/lookout is the rest of the read path and covers far more than this
// does — blast radius, deltas, time-travel graph queries, distilled logs. What
// it has no tool for is the plainest question an operator asks first: *what is
// in this namespace*. Every lookout tool that returns object detail
// (k8s_state_edges, k8s_resource_spec, k8s_triage_workload) is scoped to one
// workload and wants a `<Kind>/<namespace>/<name>` target, and the two broad
// scans (k8s_cluster_health, k8s_triage_delta) report only what is *unhealthy*
// and name nothing when the namespace is clean.
//
// The consequence was measured rather than guessed. The tier-2 fixture
// fault-badselector is a Service whose selector matches nothing, in front of a
// Deployment that is perfectly healthy — so the health scans correctly report
// the namespace clean and name no objects, and the agent had no way to learn
// that the Deployment is called `frontend`. In both runs it guessed: `api`,
// `web`, `app`, `backend`, fifteen to nineteen calls against names that do not
// exist, and then it asked the user to run `kubectl get all` and paste the
// output — which kills the run outright. An agent that cannot enumerate can
// only diagnose faults that announce themselves in a health scan.
//
// # Why this is ours and not lookout's
//
// lookout is consumed over MCP as a subprocess and belongs to another repo on
// another ADK major; growing a tool there to unblock a fixture here is a slow
// loop through a dependency we deliberately do not link. This is eighty lines
// of `kubectl get -o json` and it shares the cluster pin the write path
// already needed (internal/kubectl). If lookout ever ships an equivalent, this
// package is one deletion and one spec edit.
//
// # What it deliberately does not do
//
// It reports an inventory, not a diagnosis. For each object it prints the
// fields `kubectl get <kind>` prints in its own default table — a Pod's phase
// and restarts, a Service's type and ports, an Endpoints object's address
// count — and nothing that required a judgement. Notably it does not print a
// Service's selector or compare it to anything: naming the selector mismatch
// would be answering fault-badselector inside the enumeration tool, and the
// point is to unblock k8s_state_edges, which is the check that is supposed to
// find it. A tool built to make one fixture pass measures the fixture.
package kuberead

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/k8s-sre-agent/internal/kubectl"
)

// ToolsetName is how a specialist spec grants this toolset
// (`tools: mcp: - server: kuberead`).
//
// Granted through the spec allowlist rather than bound in Go, unlike the write
// tools. The reason the write binding cannot live in a spec is that a config
// file must not be able to revoke a safety guarantee; there is no guarantee
// here to revoke. An operator narrowing which specialists may enumerate a
// namespace is making an ordinary scoping decision, and the worst case is an
// agent that has to guess — which is exactly the state this package fixes and
// not a new hazard.
const ToolsetName = "kuberead"

// ToolName is the single tool this package exposes.
const ToolName = "k8s_list_resources"

// DefaultMaxObjects caps how many objects one listing returns.
//
// A bound is needed because the caller is a language model with a context
// window, and a busy namespace has thousands of objects. Two hundred is above
// any namespace whose contents a person would reason about at once and far
// below the point where the listing crowds out the diagnosis. Overflow is
// reported in the listing's note rather than silently dropped — a truncated
// inventory that does not say it is truncated is worse than no inventory,
// because the absence of an object reads as evidence.
const DefaultMaxObjects = 200

// Config binds the enumeration tool to exactly one cluster.
type Config struct {
	// Kubeconfig is the credential file kubectl may use. Required: there is no
	// fallback to ~/.kube/config.
	Kubeconfig string

	// Context names the kube context every invocation passes as --context. It
	// must also be the Kubeconfig's current-context. Required.
	Context string

	// Binary is the kubectl executable. Empty resolves kubectl on PATH.
	Binary string

	// Timeout bounds one invocation. Zero uses kubectl.DefaultTimeout.
	Timeout time.Duration

	// MaxObjects overrides DefaultMaxObjects.
	MaxObjects int

	// Runner substitutes the kubectl subprocess. Tests set this; nothing in
	// production should.
	Runner Runner
}

// Runner executes one kubectl invocation and keeps the streams apart.
//
// Narrower than internal/kubewrite's Runner of the same name, and separate on
// purpose: this is the read half's view of *kubectl.Client. A read has to
// parse stdout, so it cannot accept the interleaving the write half wants; and
// a package that only reads should not be able to satisfy an interface that
// can write.
type Runner interface {
	Output(ctx context.Context, args ...string) (stdout, stderr string, err error)
}

// Toolset builds the live enumeration toolset.
func Toolset(cfg Config) (tool.Toolset, error) {
	run := cfg.Runner
	if run == nil {
		c, err := kubectl.New(kubectl.Config{
			Kubeconfig: cfg.Kubeconfig,
			Context:    cfg.Context,
			Binary:     cfg.Binary,
			Timeout:    cfg.Timeout,
		})
		if err != nil {
			return nil, fmt.Errorf("kuberead: %w", err)
		}
		run = c
	}
	max := cfg.MaxObjects
	if max == 0 {
		max = DefaultMaxObjects
	}

	l := &lister{run: run, max: max}
	t, err := listTool(l.list)
	if err != nil {
		return nil, fmt.Errorf("kuberead: build tool: %w", err)
	}
	return &staticToolset{tools: []tool.Tool{t}}, nil
}

// Offline builds the same tool surface with no cluster behind it, for the
// fixed eval tier.
//
// The declaration is produced by the same constructor the live tool uses, so
// the two cannot drift: tier 1 measures the tool surface we actually ship, and
// a change to the arguments or the description shows up in both without anyone
// remembering to copy it. The caller supplies the message because the wording
// belongs to the tier — lookout.OfflineMessage has to say that the absence of
// data is permanent and that asking for it will not be answered, and that is a
// property of the eval substrate rather than of this package.
func Offline(message string) tool.Toolset {
	t, err := listTool(func(_ agent.Context, a listArgs) (Listing, error) {
		// The message goes in Note rather than Error. An offline run is not a
		// malfunction, and a model that reads "error" retries — which is the
		// behaviour the message exists to prevent.
		return Listing{Namespace: a.Namespace, Note: message}, nil
	})
	if err != nil {
		// Unreachable: the declaration is derived from a struct literal in this
		// package, so a failure here is a compile-time mistake that got through.
		panic("kuberead: offline tool: " + err.Error())
	}
	return &staticToolset{tools: []tool.Tool{t}}
}

type staticToolset struct{ tools []tool.Tool }

func (s *staticToolset) Name() string { return ToolsetName }

func (s *staticToolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) { return s.tools, nil }
