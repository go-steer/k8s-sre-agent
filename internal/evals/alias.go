package evals

import "sort"

// Tool-name normalization.
//
// Three separate remappings are needed to score a trajectory, and they are
// kept separate so each stays auditable:
//
//   - datasetRepair fixes upstream dataset bug 3 — expected_tools naming
//     tools that never existed in the Python agent's own registry. Without
//     this, a correctly-implemented tool_coverage penalizes the Python agent
//     for failing to call tools it does not have.
//
//   - unreachableTools name capabilities no agent under test possesses.
//
//   - satisfies maps a lookout check onto the Python tool *intents* it
//     answers. Our read path is served by k8s-lookout's deterministic checks
//     rather than by thin kubectl wrappers, so the names legitimately differ,
//     and the mapping is one-to-many because several lookout checks
//     deliberately replace a handful of kubectl reads with one call.
//
// Canonical form is always the post-repair Python name.
//
// # Why one-to-many, and what it costs
//
// k8s_triage_workload describes itself as "one correlated snapshot ... instead
// of 4–5 separate reads". Scoring it as an alias for exactly one kubectl tool
// would mark an agent wrong for using the tool as designed. But crediting it
// for several intents does blunt the evaluator: an agent that reflexively
// calls that one check scores well on most examples without demonstrating
// judgment. That is a real limitation of set-based recall, not something the
// mapping can fix — it is measured explicitly by TestSubsumptionDoesNotMakeOneToolSufficient
// and stated in the eval output, rather than left for a reader to discover.

// datasetRepair maps a nonexistent expected_tools entry to the real Python
// tool that performs the job. Verified against tools/__init__.py: each key is
// absent from the registry and each value is present.
var datasetRepair = map[string]string{
	"kubectl_get_pvcs":         "kubectl_get_pvc",      // plural typo
	"kubectl_get_service":      "kubectl_get_services", // singular typo
	"kubectl_describe_service": "kubectl_get_services", // no describe_service tool exists
	"kubectl_describe_node":    "kubectl_get_nodes",    // no describe_node tool exists
	"kubectl_describe_hpa":     "kubectl_get_hpa",      // no describe_hpa tool exists
	"kubectl_describe_ingress": "kubectl_get_ingress",  // no describe_ingress tool exists
}

// unreachableTools name capabilities the Python agent has no tool for at all,
// so there is nothing to repair them to. Examples requiring one of these are
// scored with the tool excluded from the denominator, and the exclusion is
// reported — silently dropping it would inflate the score.
var unreachableTools = map[string]bool{
	// The Python agent exposes no Secret-reading tool in READ_TOOLS or
	// WRITE_TOOLS. Two examples expect kubectl_get_secrets regardless.
	"kubectl_get_secrets": true,
}

// satisfies maps each lookout check to the canonical Python tool intents it
// answers. Derived from the tool descriptions lookout advertises over MCP
// (internal/lookout/tools.json), not from guesswork — the quoted phrases are
// from those descriptions.
//
// Checks with no kubectl counterpart in this dataset — k8s_blast_radius,
// k8s_admission_webhooks, k8s_gitops_drift, k8s_workload_identity,
// k8s_net_probe, k8s_perf_probe, the k8s_cloud_* family — are deliberately
// absent. They are capability upstream lacks entirely; crediting them against
// a Python tool name would be scoring a comparison that does not exist.
//
// k8s_triage_status is also absent, and that is not an oversight: it writes
// or reads back a triage *record* ("diagnosis, action taken, and your
// severity judgment"). It is the closing move of an incident, not a read of
// cluster state, so it answers no kubectl read intent.
var satisfies = map[string][]string{
	// "one correlated snapshot of a workload — sanitized spec, everything
	// abnormal, broken dependency edges, blast radius, distilled logs ...
	// instead of 4–5 separate reads".
	//
	// Two intents it does NOT answer, despite bundling five sections:
	// kubectl_get_events, because the delta section is derived from object
	// status (phase, restarts, exit_code, conditions) and not from the event
	// stream — that is k8s_event_timeline; and kubectl_get_pods, because the
	// bundle takes a workload target and so cannot be the call that discovers
	// which workload is broken — that is k8s_triage_delta.
	"k8s_triage_workload": {
		"kubectl_describe_pod",
		"kubectl_get_pod_logs",
		"kubectl_get_deployments",
	},
	// "Read ONE resource's spec: kubectl describe, but token-dense".
	//
	// The listed intents include get-shaped names because datasetRepair
	// collapses kubectl_describe_hpa into kubectl_get_hpa: upstream has no
	// describe tool for those kinds, so the get name carries the describe
	// intent for them.
	"k8s_resource_spec": {
		"kubectl_describe_pod",
		"kubectl_get_deployments",
		"kubectl_get_hpa",
		"kubectl_get_daemonsets",
	},
	// "kubectl logs, distilled".
	"k8s_triage_logs": {"kubectl_get_pod_logs"},
	// "Every abnormal object in one scan ... broken/pending pods, stalled
	// rollouts, node pressure ... quotas at their limits".
	"k8s_triage_delta": {
		"kubectl_get_pods",
		"kubectl_get_nodes",
		"kubectl_get_resource_quotas",
	},
	// "a ten-category scorecard (control-plane, nodes, ...)".
	"k8s_cluster_health": {"get_cluster_summary", "kubectl_get_nodes"},
	// "kubectl get events, but collapsed by (object, reason family)".
	"k8s_event_timeline": {"kubectl_get_events"},
	// "kubectl top, but judged ... -A adds node usage vs allocatable".
	"k8s_resource_top": {"kubectl_top_pods", "kubectl_top_nodes"},
	// "What changed around one workload ... rollouts, config/secret updates".
	"k8s_recent_changes": {"kubectl_rollout_history"},
	// "Verify every dependency edge ... Service selectors and endpoints,
	// Ingress backends".
	"k8s_state_edges": {"kubectl_get_services", "kubectl_get_ingress"},
	// "join VolumeAttachment + PV/PVC + pods".
	"k8s_volume_conflicts": {"kubectl_get_pvc"},
	// "list everything that will block the drain".
	"k8s_drain_blockers": {"kubectl_get_nodes"},
	// internal/kuberead, not lookout: "one line per object as
	// <Kind>/<namespace>/<name> ... followed by that object's headline status".
	//
	// This is the only entry in the table that is genuinely a `kubectl get`, so
	// it is also the one most in danger of being credited for the whole dataset
	// — upstream's read surface is largely per-kind list wrappers, and one
	// enumeration answers all of them at once. The rule that bounds it: an
	// intent is earned only where the formatter prints what `kubectl get <kind>`
	// prints.
	//
	// That keeps three near-misses out. kubectl_get_nodes, because Nodes are
	// cluster-scoped and this lists a namespace. kubectl_get_resource_quotas,
	// because a ResourceQuota comes back as a bare name — the used/hard numbers
	// that make the intent worth answering are k8s_triage_delta's.
	// kubectl_get_custom_resources, because the default kind set is built-ins.
	//
	// And it keeps out two the tool does list. kubectl_get_services and
	// kubectl_get_ingress are each the canonical form of a *describe* intent
	// too — datasetRepair collapses kubectl_describe_service and
	// kubectl_describe_ingress into them, and the describe spelling is the
	// majority of both names' occurrences in the dataset. Crediting a one-line
	// inventory with a describe is the same mistake the k8s_triage_status trim
	// removed, and k8s_state_edges already claims both names while actually
	// answering them. The cost of the exclusion is measured, not assumed: with
	// the two in, a three-call reflex scored 0.842 against a real agent's 0.881,
	// which is tool_coverage no longer discriminating.
	"k8s_list_resources": {
		"kubectl_get_pods",
		"kubectl_get_deployments",
		"kubectl_get_daemonsets",
		"kubectl_get_hpa",
		"kubectl_get_pvc",
		// The one intent an enumeration answers better than any check:
		// "this namespace has no NetworkPolicy" is only knowable from a listing.
		"kubectl_get_network_policies",
	},
}

// CanonicalTool reduces a tool name to the canonical Python name used for
// scoring. Unknown names pass through unchanged.
func CanonicalTool(name string) string {
	if v, ok := datasetRepair[name]; ok {
		return v
	}
	return name
}

// CanonicalTools normalizes and de-duplicates a list of tool names, preserving
// nothing about order (scoring is set-based).
func CanonicalTools(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		c := CanonicalTool(n)
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// SatisfiedIntents is the set of canonical Python tool intents a trajectory
// answers: each call contributes itself plus, for a lookout check, every
// intent that check subsumes.
func SatisfiedIntents(trajectory []string) map[string]bool {
	out := make(map[string]bool, len(trajectory)*2)
	for _, name := range trajectory {
		out[CanonicalTool(name)] = true
		for _, intent := range satisfies[name] {
			out[intent] = true
		}
	}
	return out
}

// IsUnreachable reports whether a canonical tool names a capability no agent
// under test possesses.
func IsUnreachable(canonical string) bool { return unreachableTools[canonical] }
