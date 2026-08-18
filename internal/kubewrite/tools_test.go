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
	"slices"
	"strings"
	"testing"
)

// toolCase is a valid call and the kubectl invocation it must produce.
//
// These are goldens on the *arguments*, not on the rendered line, so the
// quoting in executor.render cannot mask a wrong flag. What the human reads is
// covered separately in executor_test.go.
type toolCase struct {
	tool  string
	args  map[string]any
	want  [][]string
	stdin string
	// effect is a fragment the approval prompt must contain — the one-line
	// plain-language summary a reviewer reads before the command.
	effect string
}

var toolCases = []toolCase{{
	tool: "kubectl_scale_deployment",
	args: map[string]any{"deployment_name": "web", "namespace": "prod", "replicas": 3},
	want: [][]string{{"scale", "deployment/web", "-n", "prod", "--replicas=3"}},
	// Zero replicas is a legitimate remediation (stop a workload that is
	// hammering a dependency), so it must not be confused with "unset".
	effect: "Scale Deployment prod/web to 3 replicas.",
}, {
	tool: "kubectl_scale_deployment",
	args: map[string]any{"deployment_name": "web", "namespace": "prod", "replicas": 0},
	want: [][]string{{"scale", "deployment/web", "-n", "prod", "--replicas=0"}},
}, {
	tool: "kubectl_scale_bulk",
	args: map[string]any{"targets": []any{
		map[string]any{"resource_name": "web", "namespace": "prod", "replicas": 3},
		map[string]any{"resource_type": "statefulset", "resource_name": "db", "namespace": "prod", "replicas": 1},
	}},
	want: [][]string{
		{"scale", "deployment/web", "-n", "prod", "--replicas=3"},
		{"scale", "statefulset/db", "-n", "prod", "--replicas=1"},
	},
	effect: "Scale 2 workload(s).",
}, {
	tool: "kubectl_patch_resource_limits",
	args: map[string]any{
		"deployment_name": "web", "namespace": "prod", "container_name": "app",
		"cpu_request": "250m", "memory_limit": "512Mi",
	},
	want: [][]string{{
		"patch", "deployment", "web", "-n", "prod", "--type=strategic", "-p",
		`{"spec":{"template":{"spec":{"containers":[{"name":"app","resources":` +
			`{"limits":{"memory":"512Mi"},"requests":{"cpu":"250m"}}}]}}}}`,
	}},
	effect: "request cpu=250m, limit memory=512Mi",
}, {
	tool: "kubectl_patch_hpa",
	args: map[string]any{
		"hpa_name": "web", "namespace": "prod",
		"min_replicas": 2, "max_replicas": 10, "target_cpu_utilization": 70,
	},
	want: [][]string{{
		"patch", "hpa", "web", "-n", "prod", "--type=merge", "-p",
		`{"spec":{"maxReplicas":10,"metrics":[{"resource":{"name":"cpu","target":` +
			`{"averageUtilization":70,"type":"Utilization"}},"type":"Resource"}],"minReplicas":2}}`,
	}},
	effect: "replacing the metric list",
}, {
	tool: "kubectl_patch_hpa",
	args: map[string]any{"hpa_name": "web", "namespace": "prod", "max_replicas": 20},
	want: [][]string{{
		"patch", "hpa", "web", "-n", "prod", "--type=merge", "-p", `{"spec":{"maxReplicas":20}}`,
	}},
}, {
	tool: "kubectl_patch_configmap",
	args: map[string]any{
		"configmap_name": "web-cfg", "namespace": "prod",
		"data": map[string]any{"LOG_LEVEL": "debug"},
	},
	want: [][]string{{
		"patch", "configmap", "web-cfg", "-n", "prod", "--type=merge", "-p",
		`{"data":{"LOG_LEVEL":"debug"}}`,
	}},
	effect: "will not see the new value until they restart",
}, {
	tool: "kubectl_delete_pod",
	args: map[string]any{"pod_name": "web-abc12", "namespace": "prod"},
	want: [][]string{{"delete", "pod", "web-abc12", "-n", "prod"}},
}, {
	tool: "kubectl_delete_pod",
	args: map[string]any{"pod_name": "web-abc12", "namespace": "prod", "force": true},
	want: [][]string{{"delete", "pod", "web-abc12", "-n", "prod", "--force", "--grace-period=0"}},
	// The effect line has to change with the flag, or a reviewer who has
	// approved ten ordinary pod deletions approves the eleventh by pattern.
	effect: "FORCE-delete",
}, {
	tool: "kubectl_delete_resources_bulk",
	args: map[string]any{"targets": []any{
		map[string]any{"resource_type": "Deployment", "resource_name": "web", "namespace": "prod"},
		map[string]any{"resource_type": "service", "resource_name": "web", "namespace": "prod"},
	}},
	want: [][]string{
		{"delete", "deployment", "web", "-n", "prod"},
		{"delete", "service", "web", "-n", "prod"},
	},
	effect: "Delete 2 object(s)",
}, {
	tool: "kubectl_delete_custom_resource",
	args: map[string]any{
		"group": "monitoring.coreos.com", "plural": "prometheuses",
		"name": "platform-1", "namespace": "prod",
	},
	want:   [][]string{{"delete", "prometheuses.monitoring.coreos.com", "platform-1", "-n", "prod"}},
	effect: "Anything its operator reconciles will be torn down with it.",
}, {
	tool: "kubectl_delete_custom_resource",
	args: map[string]any{"group": "example.com", "plural": "widgets", "name": "w1"},
	want: [][]string{{"delete", "widgets.example.com", "w1"}},
}, {
	tool: "kubectl_resize_pvc",
	args: map[string]any{"pvc_name": "data-db-0", "namespace": "prod", "new_size": "20Gi"},
	want: [][]string{{
		"patch", "pvc", "data-db-0", "-n", "prod", "--type=merge", "-p",
		`{"spec":{"resources":{"requests":{"storage":"20Gi"}}}}`,
	}},
	effect: "allow volume expansion",
}, {
	tool:   "kubectl_apply_manifest",
	args:   map[string]any{"namespace": "prod", "manifest_yaml": "kind: ConfigMap\n"},
	want:   [][]string{{"apply", "-n", "prod", "-f", "-"}},
	stdin:  "kind: ConfigMap\n",
	effect: "overwrites the fields of those that do",
}, {
	tool:   "kubectl_cordon_node",
	args:   map[string]any{"node_name": "gke-pool-1-abc"},
	want:   [][]string{{"cordon", "gke-pool-1-abc"}},
	effect: "Pods already on it keep running",
}, {
	tool: "kubectl_uncordon_node",
	args: map[string]any{"node_name": "gke-pool-1-abc"},
	want: [][]string{{"uncordon", "gke-pool-1-abc"}},
}, {
	tool: "kubectl_rollout_restart",
	args: map[string]any{"resource_type": "daemonset", "resource_name": "fluentd", "namespace": "logging"},
	want: [][]string{{"rollout", "restart", "daemonset/fluentd", "-n", "logging"}},
}, {
	tool:   "kubectl_rollback_deployment",
	args:   map[string]any{"deployment_name": "web", "namespace": "prod"},
	want:   [][]string{{"rollout", "undo", "deployment/web", "-n", "prod"}},
	effect: "the previous revision",
}, {
	tool: "kubectl_rollback_deployment",
	args: map[string]any{"deployment_name": "web", "namespace": "prod", "revision": 4},
	want: [][]string{{"rollout", "undo", "deployment/web", "-n", "prod", "--to-revision=4"}},
}}

// TestApprovedCallsRunTheExpectedCommand is the golden table.
//
// It runs through the tool's real JSON schema, so a field the schema cannot
// carry — a typed array of structs, a map of strings — fails here rather than
// silently arriving empty from a model.
func TestApprovedCallsRunTheExpectedCommand(t *testing.T) {
	for _, tc := range toolCases {
		t.Run(tc.tool+"/"+summarize(tc.args), func(t *testing.T) {
			run, tools := kit(t, Config{})
			res, err := call(t, tools, approved(), tc.tool, tc.args)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Status != StatusApplied {
				t.Fatalf("status = %q (%s)", res.Status, res.Error)
			}
			if got := run.args(); !slices.EqualFunc(got, tc.want, slices.Equal) {
				t.Errorf("ran\n  %v\nwant\n  %v", got, tc.want)
			}
			if tc.stdin != "" && run.calls[0].stdin != tc.stdin {
				t.Errorf("stdin = %q, want %q", run.calls[0].stdin, tc.stdin)
			}
		})
	}
}

// TestTheEffectLineDescribesTheChange.
//
// The commands are the contract, but a reviewer reads the sentence first —
// and for the tools whose command line hides the consequence (a merge patch
// that replaces an HPA's metric list, a --force that orphans a container, a
// custom resource whose deletion cascades), the sentence is the only place the
// consequence appears at all.
func TestTheEffectLineDescribesTheChange(t *testing.T) {
	for _, tc := range toolCases {
		if tc.effect == "" {
			continue
		}
		t.Run(tc.tool+"/"+summarize(tc.args), func(t *testing.T) {
			_, tools := kit(t, Config{})
			ctx := newGateContext()
			if _, err := call(t, tools, ctx, tc.tool, tc.args); err == nil {
				t.Fatal("no confirmation was requested")
			}
			got, ok := ctx.pending()
			if !ok {
				t.Fatal("no pending confirmation")
			}
			if !strings.Contains(got.Hint, tc.effect) {
				t.Errorf("hint is missing %q:\n%s", tc.effect, got.Hint)
			}
		})
	}
}

// TestEveryToolRefusesFlagShapedArguments.
//
// Nothing here goes through a shell, so this is not about shell injection. It
// is about kubectl's own flags: `kubectl delete pod --all -n prod` is a real
// command, and "--all" is a name-shaped string. Every argument this package
// interpolates is validated by a pattern that cannot start with a dash, and
// this sweeps all fourteen tools rather than trusting that each planner
// remembered to call the guard.
func TestEveryToolRefusesFlagShapedArguments(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range toolCases {
		if seen[tc.tool] {
			continue
		}
		seen[tc.tool] = true
		t.Run(tc.tool, func(t *testing.T) {
			run, tools := kit(t, Config{})
			ctx := newGateContext()
			res, err := call(t, tools, ctx, tc.tool, hostile(tc.args).(map[string]any))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Status != StatusRefused {
				t.Errorf("status = %q, want %q", res.Status, StatusRefused)
			}
			if _, asked := ctx.pending(); asked {
				t.Error("a flag-shaped argument reached a human for approval")
			}
			if len(run.calls) != 0 {
				t.Errorf("kubectl ran: %v", run.args())
			}
		})
	}
	if len(seen) != len(ToolNames()) {
		t.Errorf("the golden table covers %d of %d tools", len(seen), len(ToolNames()))
	}
}

// hostile replaces every string in an argument tree with a kubectl flag.
func hostile(v any) any {
	switch t := v.(type) {
	case string:
		return "--all"
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = hostile(val)
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, val := range t {
			out = append(out, hostile(val))
		}
		return out
	default:
		return v
	}
}

// TestRefusalsAreStructured spot-checks the planner-level validation that the
// hostile sweep does not reach: the cases where the argument is well-formed
// and the request still makes no sense.
func TestRefusalsAreStructured(t *testing.T) {
	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{{
		name: "a scale with no ceiling",
		tool: "kubectl_scale_deployment",
		args: map[string]any{"deployment_name": "web", "namespace": "prod", "replicas": 5000},
		want: "ceiling",
	}, {
		name: "a patch that changes nothing",
		tool: "kubectl_patch_resource_limits",
		args: map[string]any{"deployment_name": "web", "namespace": "prod", "container_name": "app"},
		want: "would change nothing",
	}, {
		name: "an HPA floor above its ceiling",
		tool: "kubectl_patch_hpa",
		args: map[string]any{"hpa_name": "web", "namespace": "prod", "min_replicas": 10, "max_replicas": 2},
		want: "above max_replicas",
	}, {
		name: "a CPU target that is not a percentage",
		tool: "kubectl_patch_hpa",
		args: map[string]any{"hpa_name": "web", "namespace": "prod", "target_cpu_utilization": 700},
		want: "percentage",
	}, {
		name: "an empty ConfigMap patch",
		tool: "kubectl_patch_configmap",
		args: map[string]any{"configmap_name": "c", "namespace": "prod", "data": map[string]any{}},
		want: "would change nothing",
	}, {
		name: "a resource type this agent does not delete",
		tool: "kubectl_delete_resources_bulk",
		args: map[string]any{"targets": []any{
			map[string]any{"resource_type": "node", "resource_name": "n1", "namespace": "prod"},
		}},
		want: "not one this agent changes",
	}, {
		name: "a bulk call with no targets",
		tool: "kubectl_delete_resources_bulk",
		args: map[string]any{"targets": []any{}},
		want: "no targets",
	}, {
		name: "a size that is not a quantity",
		tool: "kubectl_resize_pvc",
		args: map[string]any{"pvc_name": "d", "namespace": "prod", "new_size": "20 gigabytes"},
		want: "not a Kubernetes quantity",
	}, {
		name: "an empty manifest",
		tool: "kubectl_apply_manifest",
		args: map[string]any{"namespace": "prod", "manifest_yaml": "  \n"},
		want: "manifest is empty",
	}, {
		name: "a restart of something that does not roll",
		tool: "kubectl_rollout_restart",
		args: map[string]any{"resource_type": "pod", "resource_name": "p", "namespace": "prod"},
		want: "not one this agent changes",
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run, tools := kit(t, Config{})
			ctx := newGateContext()
			res, err := call(t, tools, ctx, tc.tool, tc.args)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Status != StatusRefused {
				t.Fatalf("status = %q, want %q", res.Status, StatusRefused)
			}
			if !strings.Contains(res.Error, tc.want) {
				t.Errorf("refusal %q does not mention %q", res.Error, tc.want)
			}
			if _, asked := ctx.pending(); asked {
				t.Error("a refused call asked for approval")
			}
			if len(run.calls) != 0 {
				t.Errorf("kubectl ran: %v", run.args())
			}
		})
	}
}

// TestABatchTouchingAProtectedNamespaceIsRefusedWhole.
//
// Upstream's rule, and the reason it is a pre-pass rather than a per-target
// check: half a delete batch is worse than none, and a reviewer should see
// "this batch touches kube-system" once rather than one retry at a time.
func TestABatchTouchingAProtectedNamespaceIsRefusedWhole(t *testing.T) {
	run, tools := kit(t, Config{})
	res, err := call(t, tools, newGateContext(), "kubectl_delete_resources_bulk", map[string]any{
		"targets": []any{
			map[string]any{"resource_type": "pod", "resource_name": "web-1", "namespace": "prod"},
			map[string]any{"resource_type": "pod", "resource_name": "coredns-1", "namespace": "kube-system"},
			map[string]any{"resource_type": "pod", "resource_name": "web-2", "namespace": "prod"},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != StatusRefused {
		t.Fatalf("status = %q, want %q", res.Status, StatusRefused)
	}
	if !strings.Contains(res.Error, "kube-system") || !strings.Contains(res.Error, "none of it ran") {
		t.Errorf("refusal = %q", res.Error)
	}
	if len(run.calls) != 0 {
		t.Errorf("part of the batch ran: %v", run.args())
	}
}

// TestBulkCeilingIsConfigurable, and refuses past it.
func TestBulkCeilingIsConfigurable(t *testing.T) {
	run, tools := kit(t, Config{BulkMax: 2})
	targets := []any{}
	for _, n := range []string{"a", "b", "c"} {
		targets = append(targets, map[string]any{
			"resource_type": "pod", "resource_name": n, "namespace": "prod",
		})
	}
	res, err := call(t, tools, newGateContext(), "kubectl_delete_resources_bulk",
		map[string]any{"targets": targets})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != StatusRefused || !strings.Contains(res.Error, "2-target ceiling") {
		t.Errorf("status = %q, error = %q", res.Status, res.Error)
	}
	if len(run.calls) != 0 {
		t.Errorf("kubectl ran: %v", run.args())
	}
}

// TestRejectionNamesWhatWasDeclined. A rejection the change-executor cannot
// describe is a rejection it cannot report, and reporting it is the only thing
// it is supposed to do next.
func TestRejectionNamesWhatWasDeclined(t *testing.T) {
	run, tools := kit(t, Config{})
	res, err := call(t, tools, rejected(), "kubectl_delete_resources_bulk", map[string]any{
		"targets": []any{
			map[string]any{"resource_type": "pod", "resource_name": "web-1", "namespace": "prod"},
			map[string]any{"resource_type": "pod", "resource_name": "web-2", "namespace": "prod"},
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != StatusRejected {
		t.Fatalf("status = %q, want %q", res.Status, StatusRejected)
	}
	if len(res.Commands) != 2 {
		t.Errorf("commands = %v, want the two declined deletions", res.Commands)
	}
	if len(run.calls) != 0 {
		t.Errorf("kubectl ran: %v", run.args())
	}
}

// summarize builds a stable subtest name from an argument map.
func summarize(args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}
