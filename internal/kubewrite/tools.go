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
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolutils"
)

// The write surface.
//
// # Relationship to upstream
//
// These are upstream's thirteen change-executor tools plus
// kubectl_patch_configmap, which upstream defines and then omits from its
// interrupt list. The names are upstream's deliberately: the read path
// diverged because lookout's checks are genuinely different objects from
// kubectl's reads (see internal/evals/alias.go for what that cost), but a
// write is a write, and keeping the names means upstream's change-executor
// prompt, and any eval written against it, ports without a translation table.
//
// Three of upstream's tools have no counterpart here:
//
//   - kubectl_delete_resource — kubectl_delete_resources_bulk with one target
//     is the same call, and a second spelling of "delete something" is a second
//     thing to keep the guards in sync with.
//   - kubectl_apply_custom_resource — upstream needs it because its typed
//     client dispatches on group/version/plural. `kubectl apply` does not care
//     what API group an object belongs to, so kubectl_apply_manifest already
//     covers it.
//   - the `version` parameter of kubectl_delete_custom_resource — kubectl
//     resolves the served version itself, and a parameter that is accepted and
//     ignored teaches the model that its arguments are decorative.
//
// # What every description has to say
//
// Each tool's description states the mutation and that approval pauses the
// call. That second half is not politeness: a model that does not know a tool
// blocks will either avoid it or, worse, call it and then narrate the change
// as done while the approval is still pending.
const approvalNote = " This mutates the cluster and pauses for human approval: " +
	"the reviewer is shown the exact kubectl command before anything runs, and may decline."

// toolSpec is one write tool's identity plus its constructor. Kept as data so
// ToolNames can report the roster without a cluster connection, and so adding
// a tool is one entry in one list.
type toolSpec struct {
	name        string
	description string
	build       func(*executor, toolSpec) (tool.Tool, error)
}

var toolSpecs = []toolSpec{{
	name: "kubectl_scale_deployment",
	description: "Scale one Deployment to a given replica count." +
		approvalNote,
	build: planner(planScaleDeployment),
}, {
	name: "kubectl_scale_bulk",
	description: "Scale several workloads in one operation. One call is one approval, " +
		"so use this instead of repeating kubectl_scale_deployment when the change is a set." +
		approvalNote,
	build: planner(planScaleBulk),
}, {
	name: "kubectl_patch_resource_limits",
	description: "Set CPU and/or memory requests and limits on one container of a Deployment. " +
		"Values are Kubernetes quantities (500m, 512Mi). Only the values you supply are changed." +
		approvalNote,
	build: planner(planPatchLimits),
}, {
	name: "kubectl_patch_hpa",
	description: "Change a HorizontalPodAutoscaler's min replicas, max replicas, and/or CPU " +
		"target. Supplying a CPU target replaces the HPA's entire metric list with that one " +
		"CPU metric, so do not use it on an HPA driven by custom metrics." +
		approvalNote,
	build: planner(planPatchHPA),
}, {
	name: "kubectl_patch_configmap",
	description: "Set keys in a ConfigMap. Keys you do not name are left alone. Pods do not " +
		"pick up a ConfigMap change on their own — follow with kubectl_rollout_restart if the " +
		"value is consumed as an env var." +
		approvalNote,
	build: planner(planPatchConfigMap),
}, {
	name: "kubectl_delete_pod",
	description: "Delete one pod so its controller recreates it. Prefer kubectl_rollout_restart " +
		"for a whole workload — it is ordered and respects the update strategy, whereas deleting " +
		"pods is not." +
		approvalNote,
	build: planner(planDeletePod),
}, {
	name: "kubectl_delete_resources_bulk",
	description: "Delete several Kubernetes objects in one operation. The whole batch is " +
		"refused if any target is in a protected namespace or if there are more targets than " +
		"one approval may cover." +
		approvalNote,
	build: planner(planDeleteBulk),
}, {
	name: "kubectl_delete_custom_resource",
	description: "Delete one custom resource by plural and API group. For an operator-managed " +
		"application this is the object to delete — deleting the Deployments and Services the " +
		"operator reconciles just makes it recreate them." +
		approvalNote,
	build: planner(planDeleteCustomResource),
}, {
	name: "kubectl_resize_pvc",
	description: "Change a PersistentVolumeClaim's requested storage. Kubernetes only allows " +
		"growth, and only on a StorageClass with allowVolumeExpansion; shrinking is rejected " +
		"by the API server." +
		approvalNote,
	build: planner(planResizePVC),
}, {
	name: "kubectl_apply_manifest",
	description: "Apply a YAML manifest. The namespace is a separate required argument so the " +
		"change can be checked against the protected list before anyone is asked; a manifest " +
		"whose objects name a different namespace is rejected by kubectl. Split a manifest " +
		"that spans namespaces." +
		approvalNote,
	build: planner(planApplyManifest),
}, {
	name:        "kubectl_cordon_node",
	description: "Mark a node unschedulable. Existing pods keep running." + approvalNote,
	build:       planner(planCordon),
}, {
	name:        "kubectl_uncordon_node",
	description: "Mark a node schedulable again." + approvalNote,
	build:       planner(planUncordon),
}, {
	name: "kubectl_rollout_restart",
	description: "Restart a Deployment, StatefulSet or DaemonSet by rolling its pods. The " +
		"correct way to make a workload pick up a changed ConfigMap or Secret, and the " +
		"preferred alternative to deleting pods." +
		approvalNote,
	build: planner(planRolloutRestart),
}, {
	name: "kubectl_rollback_deployment",
	description: "Roll a Deployment back to its previous revision, or to a specific one." +
		approvalNote,
	build: planner(planRollback),
}}

func (e *executor) tools() ([]tool.Tool, error) {
	out := make([]tool.Tool, 0, len(toolSpecs))
	for _, s := range toolSpecs {
		t, err := s.build(e, s)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.name, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// planner adapts a plan function into a gated ADK tool.
//
// The split is what keeps every tool honest: a plan function is pure — it
// validates, and it returns an effect line and the kubectl steps — and it has
// no way to reach a cluster. Execution and the approval gate live in
// executor.apply and are written once.
//
// A refusal comes back as a Result, not as a Go error. A refused write is a
// fact the change-executor has to report ("I could not do that, and here is
// why"), and a tool error reads to a model as an accident worth retrying.
func planner[A any](plan func(*executor, A) (string, []step, error)) func(*executor, toolSpec) (tool.Tool, error) {
	return func(e *executor, spec toolSpec) (tool.Tool, error) {
		inner, err := functiontool.New(
			functiontool.Config{Name: spec.name, Description: spec.description},
			func(ctx adkagent.Context, a A) (Result, error) {
				effect, steps, err := plan(e, a)
				if err != nil {
					return Result{Status: StatusRefused, Error: err.Error()}, nil
				}
				return e.apply(ctx, spec.name, effect, steps)
			})
		if err != nil {
			return nil, err
		}
		runnable, ok := inner.(runnableTool)
		if !ok {
			return nil, fmt.Errorf("function tool is not runnable (%T)", inner)
		}
		return &gatedTool{
			runnableTool: runnable,
			rejected:     func(args map[string]any) map[string]any { return rejectionOf(e, plan, args) },
		}, nil
	}
}

// runnableTool is ADK's unexported tool.runnableTool, restated because
// wrapping a function tool requires satisfying it.
type runnableTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(ctx adkagent.Context, args any) (map[string]any, error)
}

// gatedTool exists for one branch: the human said no.
//
// functiontool.Run checks ToolConfirmation *before* it decodes arguments or
// calls the handler, and on a rejection returns tool.ErrConfirmationRejected
// unconditionally — the flag that governs whether confirmation was requested
// does not govern this. So a package that raises its own confirmation, as this
// one does to control the hint, still cannot shape its own rejection. The
// handler is simply never reached.
//
// What ADK produces is `{"error": "error tool \"kubectl_delete_pod\" call is
// rejected"}`. That does not end the run — base_flow turns a tool error into
// an ordinary FunctionResponse — but it is the wrong shape twice over. It
// reads as a malfunction rather than as a decision, and a malfunction invites
// another attempt; and it arrives on the `error` channel, so a
// change-executor asked to report what it did has nothing structured to report.
//
// This wrapper substitutes a Result. It is the only behaviour it changes;
// everything else, including the declaration and the approved path, is the
// inner tool's.
type gatedTool struct {
	runnableTool
	rejected func(args map[string]any) map[string]any
}

func (g *gatedTool) Run(ctx adkagent.Context, args any) (map[string]any, error) {
	if c := ctx.ToolConfirmation(); c != nil && !c.Confirmed {
		m, _ := args.(map[string]any)
		return g.rejected(m), nil
	}
	return g.runnableTool.Run(ctx, args)
}

// ProcessRequest mirrors ADK's own confirmationTool wrapper (tool/tool.go):
// let the inner tool declare itself, then substitute this wrapper in the
// request's tool map so the wrapper's Run is the one that executes. Without
// the substitution the declaration would be right and the gate would be
// bypassed — the failure mode that is invisible until someone rejects a
// change and watches it happen anyway.
func (g *gatedTool) ProcessRequest(ctx adkagent.Context, req *model.LLMRequest) error {
	if rp, ok := g.runnableTool.(interface {
		ProcessRequest(ctx adkagent.Context, req *model.LLMRequest) error
	}); ok {
		_, existedBefore := req.Tools[g.Name()]
		if err := rp.ProcessRequest(ctx, req); err != nil {
			return err
		}
		if !existedBefore && req.Tools != nil && req.Tools[g.Name()] != nil {
			req.Tools[g.Name()] = g
			return nil
		}
	}
	return toolutils.PackTool(req, g)
}

// rejectionMessage is what the model reads after a human declines.
//
// Both halves are load-bearing. "Do not retry" because the call itself is not
// what failed; "do not attempt an equivalent change by another route" because
// the obvious reading of a refused `kubectl_delete_pod` is that
// kubectl_rollout_restart is still available, and a human who declined the
// change declined the change, not the spelling.
const rejectionMessage = "a human reviewed this change and declined it. Do not retry it and do " +
	"not attempt an equivalent change by another route; report the rejection."

// rejectionOf builds the rejection Result, re-planning the call so the report
// can name what was declined. A decode failure here costs the command list and
// nothing else — the rejection still stands.
func rejectionOf[A any](e *executor, plan func(*executor, A) (string, []step, error), args map[string]any) map[string]any {
	res := Result{Status: StatusRejected, Error: rejectionMessage}
	var a A
	if blob, err := json.Marshal(args); err == nil {
		if err := json.Unmarshal(blob, &a); err == nil {
			if _, steps, err := plan(e, a); err == nil {
				for _, s := range steps {
					res.Commands = append(res.Commands, e.render(s))
				}
			}
		}
	}
	out := map[string]any{}
	blob, err := json.Marshal(res)
	if err != nil {
		// Unreachable for a struct of strings, and a rejection that cannot be
		// serialised must still not read as an approval.
		return map[string]any{"status": StatusRejected, "error": rejectionMessage}
	}
	if err := json.Unmarshal(blob, &out); err != nil {
		return map[string]any{"status": StatusRejected, "error": rejectionMessage}
	}
	return out
}

// Workload kinds each verb accepts. Narrower than kubectl's, on purpose: the
// value is interpolated into a command line, and an allowlist is a stronger
// statement than a regex about what this agent is able to touch.
var (
	scalableKinds  = []string{"deployment", "statefulset", "replicaset", "replicationcontroller"}
	restartKinds   = []string{"deployment", "statefulset", "daemonset"}
	deletableKinds = []string{
		"deployment", "statefulset", "daemonset", "replicaset", "pod", "service",
		"configmap", "secret", "ingress", "hpa", "pvc", "job", "cronjob",
		"networkpolicy", "poddisruptionbudget", "serviceaccount",
	}
)

// kindOf normalises and checks a resource type against an allowlist. A
// plural.group form (foos.example.com) is accepted only where the tool says so.
func (g guard) kindOf(v string, allowed []string) (string, error) {
	k := strings.ToLower(strings.TrimSpace(v))
	if err := g.resourceType(k); err != nil {
		return "", err
	}
	if !slices.Contains(allowed, k) {
		return "", refusef("resource type %q is not one this agent changes; it handles %s.",
			v, strings.Join(allowed, ", "))
	}
	return k, nil
}

// ---------------------------------------------------------------- scaling

type scaleArgs struct {
	DeploymentName string `json:"deployment_name" jsonschema:"the Deployment to scale"`
	Namespace      string `json:"namespace" jsonschema:"the namespace the Deployment is in"`
	Replicas       int    `json:"replicas" jsonschema:"the desired replica count; 0 stops the workload entirely"`
}

func planScaleDeployment(e *executor, a scaleArgs) (string, []step, error) {
	if err := e.guard.namespace(a.Namespace); err != nil {
		return "", nil, err
	}
	if err := e.guard.name("deployment_name", a.DeploymentName); err != nil {
		return "", nil, err
	}
	if err := e.guard.replicas(a.Replicas); err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("Scale Deployment %s/%s to %d replicas.", a.Namespace, a.DeploymentName, a.Replicas),
		[]step{{args: []string{
			"scale", "deployment/" + a.DeploymentName,
			"-n", a.Namespace,
			fmt.Sprintf("--replicas=%d", a.Replicas),
		}}}, nil
}

type scaleTarget struct {
	ResourceType string `json:"resource_type,omitempty" jsonschema:"deployment (the default), statefulset, replicaset or replicationcontroller"`
	ResourceName string `json:"resource_name" jsonschema:"the workload's name"`
	Namespace    string `json:"namespace" jsonschema:"the workload's namespace"`
	Replicas     int    `json:"replicas" jsonschema:"the desired replica count for this workload"`
}

type scaleBulkArgs struct {
	Targets []scaleTarget `json:"targets" jsonschema:"the workloads to scale; the whole list is covered by one approval, so do not call this twice for one request"`
}

func planScaleBulk(e *executor, a scaleBulkArgs) (string, []step, error) {
	if err := e.guard.bulk(len(a.Targets)); err != nil {
		return "", nil, err
	}
	if err := e.guard.protectedIn(namespacesOf(a.Targets, func(t scaleTarget) string { return t.Namespace })); err != nil {
		return "", nil, err
	}
	steps := make([]step, 0, len(a.Targets))
	for i, t := range a.Targets {
		kind := t.ResourceType
		if strings.TrimSpace(kind) == "" {
			kind = "deployment"
		}
		kind, err := e.guard.kindOf(kind, scalableKinds)
		if err != nil {
			return "", nil, fmt.Errorf("target %d: %w", i+1, err)
		}
		if err := e.guard.namespace(t.Namespace); err != nil {
			return "", nil, fmt.Errorf("target %d: %w", i+1, err)
		}
		if err := e.guard.name("resource_name", t.ResourceName); err != nil {
			return "", nil, fmt.Errorf("target %d: %w", i+1, err)
		}
		if err := e.guard.replicas(t.Replicas); err != nil {
			return "", nil, fmt.Errorf("target %d: %w", i+1, err)
		}
		steps = append(steps, step{args: []string{
			"scale", kind + "/" + t.ResourceName,
			"-n", t.Namespace,
			fmt.Sprintf("--replicas=%d", t.Replicas),
		}})
	}
	return fmt.Sprintf("Scale %d workload(s).", len(steps)), steps, nil
}

// ---------------------------------------------------------------- patching

type limitsArgs struct {
	DeploymentName string `json:"deployment_name" jsonschema:"the Deployment whose container is being resized"`
	Namespace      string `json:"namespace" jsonschema:"the Deployment's namespace"`
	ContainerName  string `json:"container_name" jsonschema:"the container within the pod template"`
	CPURequest     string `json:"cpu_request,omitempty" jsonschema:"e.g. 250m; omit to leave unchanged"`
	CPULimit       string `json:"cpu_limit,omitempty" jsonschema:"e.g. 500m; omit to leave unchanged"`
	MemoryRequest  string `json:"memory_request,omitempty" jsonschema:"e.g. 256Mi; omit to leave unchanged"`
	MemoryLimit    string `json:"memory_limit,omitempty" jsonschema:"e.g. 512Mi; omit to leave unchanged"`
}

func planPatchLimits(e *executor, a limitsArgs) (string, []step, error) {
	if err := e.guard.namespace(a.Namespace); err != nil {
		return "", nil, err
	}
	if err := e.guard.name("deployment_name", a.DeploymentName); err != nil {
		return "", nil, err
	}
	if err := e.guard.name("container_name", a.ContainerName); err != nil {
		return "", nil, err
	}

	requests := map[string]string{}
	limits := map[string]string{}
	for _, f := range []struct {
		what  string
		value string
		into  map[string]string
		key   string
	}{
		{"cpu_request", a.CPURequest, requests, "cpu"},
		{"memory_request", a.MemoryRequest, requests, "memory"},
		{"cpu_limit", a.CPULimit, limits, "cpu"},
		{"memory_limit", a.MemoryLimit, limits, "memory"},
	} {
		if f.value == "" {
			continue
		}
		if err := e.guard.quantity(f.what, f.value); err != nil {
			return "", nil, err
		}
		f.into[f.key] = f.value
	}
	resources := map[string]any{}
	if len(requests) > 0 {
		resources["requests"] = requests
	}
	if len(limits) > 0 {
		resources["limits"] = limits
	}
	if len(resources) == 0 {
		return "", nil, refusef("no requests or limits were given, so this call would change nothing.")
	}

	patch, err := encodePatch(map[string]any{"spec": map[string]any{
		"template": map[string]any{"spec": map[string]any{
			"containers": []any{map[string]any{
				"name":      a.ContainerName,
				"resources": resources,
			}},
		}},
	}})
	if err != nil {
		return "", nil, err
	}
	// Strategic merge, not merge: the containers list is keyed by name in the
	// Kubernetes schema, so strategic merges this container's resources and
	// leaves the deployment's other containers alone. A plain merge patch would
	// replace the whole list and delete them.
	return fmt.Sprintf("Set resources on container %q of Deployment %s/%s: %s.",
			a.ContainerName, a.Namespace, a.DeploymentName, describeResources(requests, limits)),
		[]step{{args: []string{
			"patch", "deployment", a.DeploymentName,
			"-n", a.Namespace,
			"--type=strategic", "-p", patch,
		}}}, nil
}

func describeResources(requests, limits map[string]string) string {
	var parts []string
	for _, k := range []string{"cpu", "memory"} {
		if v, ok := requests[k]; ok {
			parts = append(parts, fmt.Sprintf("request %s=%s", k, v))
		}
	}
	for _, k := range []string{"cpu", "memory"} {
		if v, ok := limits[k]; ok {
			parts = append(parts, fmt.Sprintf("limit %s=%s", k, v))
		}
	}
	return strings.Join(parts, ", ")
}

type hpaArgs struct {
	HPAName              string `json:"hpa_name" jsonschema:"the HorizontalPodAutoscaler to change"`
	Namespace            string `json:"namespace" jsonschema:"the HPA's namespace"`
	MinReplicas          int    `json:"min_replicas,omitempty" jsonschema:"new floor; omit to leave unchanged"`
	MaxReplicas          int    `json:"max_replicas,omitempty" jsonschema:"new ceiling; omit to leave unchanged"`
	TargetCPUUtilization int    `json:"target_cpu_utilization,omitempty" jsonschema:"new average CPU utilization target as a percentage, 1-100; omit to leave the metrics alone"`
}

func planPatchHPA(e *executor, a hpaArgs) (string, []step, error) {
	if err := e.guard.namespace(a.Namespace); err != nil {
		return "", nil, err
	}
	if err := e.guard.name("hpa_name", a.HPAName); err != nil {
		return "", nil, err
	}
	if a.MinReplicas == 0 && a.MaxReplicas == 0 && a.TargetCPUUtilization == 0 {
		return "", nil, refusef("no new floor, ceiling or CPU target was given, so this call would change nothing.")
	}
	if a.MinReplicas < 0 || a.MaxReplicas < 0 {
		return "", nil, refusef("replica counts cannot be negative.")
	}
	if a.MaxReplicas != 0 {
		if err := e.guard.replicas(a.MaxReplicas); err != nil {
			return "", nil, err
		}
	}
	if a.MinReplicas != 0 && a.MaxReplicas != 0 && a.MinReplicas > a.MaxReplicas {
		return "", nil, refusef("min_replicas %d is above max_replicas %d.", a.MinReplicas, a.MaxReplicas)
	}
	if a.TargetCPUUtilization < 0 || a.TargetCPUUtilization > 100 {
		return "", nil, refusef("target_cpu_utilization %d is not a percentage between 1 and 100.", a.TargetCPUUtilization)
	}

	spec := map[string]any{}
	var described []string
	if a.MinReplicas != 0 {
		spec["minReplicas"] = a.MinReplicas
		described = append(described, fmt.Sprintf("min=%d", a.MinReplicas))
	}
	if a.MaxReplicas != 0 {
		spec["maxReplicas"] = a.MaxReplicas
		described = append(described, fmt.Sprintf("max=%d", a.MaxReplicas))
	}
	if a.TargetCPUUtilization != 0 {
		spec["metrics"] = []any{map[string]any{
			"type": "Resource",
			"resource": map[string]any{
				"name": "cpu",
				"target": map[string]any{
					"type":               "Utilization",
					"averageUtilization": a.TargetCPUUtilization,
				},
			},
		}}
		described = append(described, fmt.Sprintf("cpu target=%d%% (replacing the metric list)", a.TargetCPUUtilization))
	}
	patch, err := encodePatch(map[string]any{"spec": spec})
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("Change HPA %s/%s: %s.", a.Namespace, a.HPAName, strings.Join(described, ", ")),
		[]step{{args: []string{
			"patch", "hpa", a.HPAName, "-n", a.Namespace, "--type=merge", "-p", patch,
		}}}, nil
}

type configMapArgs struct {
	ConfigMapName string            `json:"configmap_name" jsonschema:"the ConfigMap to change"`
	Namespace     string            `json:"namespace" jsonschema:"the ConfigMap's namespace"`
	Data          map[string]string `json:"data" jsonschema:"keys to set and their new values; keys not listed keep their current value"`
}

func planPatchConfigMap(e *executor, a configMapArgs) (string, []step, error) {
	if err := e.guard.namespace(a.Namespace); err != nil {
		return "", nil, err
	}
	if err := e.guard.name("configmap_name", a.ConfigMapName); err != nil {
		return "", nil, err
	}
	if len(a.Data) == 0 {
		return "", nil, refusef("no keys were given, so this call would change nothing.")
	}
	keys := make([]string, 0, len(a.Data))
	for k := range a.Data {
		if err := e.guard.dataKey(k); err != nil {
			return "", nil, err
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	patch, err := encodePatch(map[string]any{"data": a.Data})
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("Set %s in ConfigMap %s/%s. Running pods will not see the new value "+
			"until they restart.", strings.Join(keys, ", "), a.Namespace, a.ConfigMapName),
		[]step{{args: []string{
			"patch", "configmap", a.ConfigMapName, "-n", a.Namespace, "--type=merge", "-p", patch,
		}}}, nil
}

// ---------------------------------------------------------------- deletion

type deletePodArgs struct {
	PodName   string `json:"pod_name" jsonschema:"the pod to delete"`
	Namespace string `json:"namespace" jsonschema:"the pod's namespace"`
	Force     bool   `json:"force,omitempty" jsonschema:"skip the graceful shutdown period; use only for a pod stuck Terminating, since it can leave the workload running on an unreachable node"`
}

func planDeletePod(e *executor, a deletePodArgs) (string, []step, error) {
	if err := e.guard.namespace(a.Namespace); err != nil {
		return "", nil, err
	}
	if err := e.guard.name("pod_name", a.PodName); err != nil {
		return "", nil, err
	}
	args := []string{"delete", "pod", a.PodName, "-n", a.Namespace}
	effect := fmt.Sprintf("Delete pod %s/%s. Its controller, if it has one, will recreate it.",
		a.Namespace, a.PodName)
	if a.Force {
		args = append(args, "--force", "--grace-period=0")
		effect = fmt.Sprintf("FORCE-delete pod %s/%s, skipping graceful shutdown. The API "+
			"server forgets the pod immediately even if the container is still running.",
			a.Namespace, a.PodName)
	}
	return effect, []step{{args: args}}, nil
}

type deleteTarget struct {
	ResourceType string `json:"resource_type" jsonschema:"deployment, statefulset, daemonset, replicaset, pod, service, configmap, secret, ingress, hpa, pvc, job, cronjob, networkpolicy, poddisruptionbudget or serviceaccount"`
	ResourceName string `json:"resource_name" jsonschema:"the object's name"`
	Namespace    string `json:"namespace" jsonschema:"the object's namespace"`
}

type deleteBulkArgs struct {
	Targets []deleteTarget `json:"targets" jsonschema:"the objects to delete; the whole list is covered by one approval, so do not call this twice for one request"`
}

func planDeleteBulk(e *executor, a deleteBulkArgs) (string, []step, error) {
	if err := e.guard.bulk(len(a.Targets)); err != nil {
		return "", nil, err
	}
	// Protected namespaces are checked across the whole batch first and named
	// together, so a reviewer sees "this batch touches kube-system" once rather
	// than discovering it one retry at a time. Upstream's rule, and the reason
	// for it holds here too: a partially-applied destructive batch is worse
	// than none.
	if err := e.guard.protectedIn(namespacesOf(a.Targets, func(t deleteTarget) string { return t.Namespace })); err != nil {
		return "", nil, err
	}
	steps := make([]step, 0, len(a.Targets))
	described := make([]string, 0, len(a.Targets))
	for i, t := range a.Targets {
		kind, err := e.guard.kindOf(t.ResourceType, deletableKinds)
		if err != nil {
			return "", nil, fmt.Errorf("target %d: %w", i+1, err)
		}
		if err := e.guard.namespace(t.Namespace); err != nil {
			return "", nil, fmt.Errorf("target %d: %w", i+1, err)
		}
		if err := e.guard.name("resource_name", t.ResourceName); err != nil {
			return "", nil, fmt.Errorf("target %d: %w", i+1, err)
		}
		steps = append(steps, step{args: []string{"delete", kind, t.ResourceName, "-n", t.Namespace}})
		described = append(described, fmt.Sprintf("%s %s/%s", kind, t.Namespace, t.ResourceName))
	}
	return fmt.Sprintf("Delete %d object(s): %s.", len(steps), strings.Join(described, ", ")), steps, nil
}

type deleteCustomResourceArgs struct {
	Group     string `json:"group" jsonschema:"the API group, e.g. langchain.com"`
	Plural    string `json:"plural" jsonschema:"the resource's plural name, e.g. langgraphplatforms"`
	Name      string `json:"name" jsonschema:"the custom resource's name"`
	Namespace string `json:"namespace,omitempty" jsonschema:"omit for a cluster-scoped custom resource"`
}

func planDeleteCustomResource(e *executor, a deleteCustomResourceArgs) (string, []step, error) {
	if err := e.guard.resourceType(a.Group); err != nil {
		return "", nil, err
	}
	if err := e.guard.resourceType(a.Plural); err != nil {
		return "", nil, err
	}
	if err := e.guard.name("name", a.Name); err != nil {
		return "", nil, err
	}
	target := strings.ToLower(a.Plural) + "." + strings.ToLower(a.Group)
	args := []string{"delete", target, a.Name}
	scope := "cluster-scoped"
	if a.Namespace != "" {
		if err := e.guard.namespace(a.Namespace); err != nil {
			return "", nil, err
		}
		args = append(args, "-n", a.Namespace)
		scope = "in " + a.Namespace
	}
	return fmt.Sprintf("Delete custom resource %s %q (%s). Anything its operator reconciles "+
			"will be torn down with it.", target, a.Name, scope),
		[]step{{args: args}}, nil
}

// ---------------------------------------------------------------- storage

type resizePVCArgs struct {
	PVCName   string `json:"pvc_name" jsonschema:"the PersistentVolumeClaim to resize"`
	Namespace string `json:"namespace" jsonschema:"the PVC's namespace"`
	NewSize   string `json:"new_size" jsonschema:"the new requested size, e.g. 20Gi; must be larger than the current size"`
}

func planResizePVC(e *executor, a resizePVCArgs) (string, []step, error) {
	if err := e.guard.namespace(a.Namespace); err != nil {
		return "", nil, err
	}
	if err := e.guard.name("pvc_name", a.PVCName); err != nil {
		return "", nil, err
	}
	if err := e.guard.quantity("new_size", a.NewSize); err != nil {
		return "", nil, err
	}
	patch, err := encodePatch(map[string]any{"spec": map[string]any{
		"resources": map[string]any{"requests": map[string]any{"storage": a.NewSize}},
	}})
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("Grow PVC %s/%s to %s. The StorageClass must allow volume expansion, "+
			"and some volume types only finish the resize after the pod restarts.",
			a.Namespace, a.PVCName, a.NewSize),
		[]step{{args: []string{
			"patch", "pvc", a.PVCName, "-n", a.Namespace, "--type=merge", "-p", patch,
		}}}, nil
}

// ---------------------------------------------------------------- manifests

type applyManifestArgs struct {
	Namespace    string `json:"namespace" jsonschema:"the namespace to apply into; required, and it must match any namespace the manifest itself names"`
	ManifestYAML string `json:"manifest_yaml" jsonschema:"the manifest, as YAML or JSON"`
}

func planApplyManifest(e *executor, a applyManifestArgs) (string, []step, error) {
	if err := e.guard.namespace(a.Namespace); err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(a.ManifestYAML) == "" {
		return "", nil, refusef("the manifest is empty.")
	}
	return fmt.Sprintf("Apply a manifest to namespace %s. Read it below — apply creates objects "+
			"that do not exist and overwrites the fields of those that do.", a.Namespace),
		[]step{{
			args:  []string{"apply", "-n", a.Namespace, "-f", "-"},
			stdin: a.ManifestYAML,
		}}, nil
}

// ---------------------------------------------------------------- nodes

type nodeArgs struct {
	NodeName string `json:"node_name" jsonschema:"the node"`
}

func planCordon(e *executor, a nodeArgs) (string, []step, error) {
	if err := e.guard.name("node_name", a.NodeName); err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("Mark node %s unschedulable. Pods already on it keep running; nothing "+
			"new lands there until it is uncordoned.", a.NodeName),
		[]step{{args: []string{"cordon", a.NodeName}}}, nil
}

func planUncordon(e *executor, a nodeArgs) (string, []step, error) {
	if err := e.guard.name("node_name", a.NodeName); err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("Mark node %s schedulable again.", a.NodeName),
		[]step{{args: []string{"uncordon", a.NodeName}}}, nil
}

// ---------------------------------------------------------------- rollouts

type rolloutRestartArgs struct {
	ResourceType string `json:"resource_type" jsonschema:"deployment, statefulset or daemonset"`
	ResourceName string `json:"resource_name" jsonschema:"the workload's name"`
	Namespace    string `json:"namespace" jsonschema:"the workload's namespace"`
}

func planRolloutRestart(e *executor, a rolloutRestartArgs) (string, []step, error) {
	kind, err := e.guard.kindOf(a.ResourceType, restartKinds)
	if err != nil {
		return "", nil, err
	}
	if err := e.guard.namespace(a.Namespace); err != nil {
		return "", nil, err
	}
	if err := e.guard.name("resource_name", a.ResourceName); err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("Roll %s %s/%s, replacing its pods under its own update strategy.",
			kind, a.Namespace, a.ResourceName),
		[]step{{args: []string{"rollout", "restart", kind + "/" + a.ResourceName, "-n", a.Namespace}}}, nil
}

type rollbackArgs struct {
	DeploymentName string `json:"deployment_name" jsonschema:"the Deployment to roll back"`
	Namespace      string `json:"namespace" jsonschema:"the Deployment's namespace"`
	Revision       int    `json:"revision,omitempty" jsonschema:"the revision to roll back to; omit for the previous one"`
}

func planRollback(e *executor, a rollbackArgs) (string, []step, error) {
	if err := e.guard.namespace(a.Namespace); err != nil {
		return "", nil, err
	}
	if err := e.guard.name("deployment_name", a.DeploymentName); err != nil {
		return "", nil, err
	}
	if a.Revision < 0 {
		return "", nil, refusef("revision %d is negative.", a.Revision)
	}
	args := []string{"rollout", "undo", "deployment/" + a.DeploymentName, "-n", a.Namespace}
	target := "the previous revision"
	if a.Revision > 0 {
		args = append(args, fmt.Sprintf("--to-revision=%d", a.Revision))
		target = fmt.Sprintf("revision %d", a.Revision)
	}
	return fmt.Sprintf("Roll Deployment %s/%s back to %s.", a.Namespace, a.DeploymentName, target),
		[]step{{args: args}}, nil
}

// ---------------------------------------------------------------- helpers

// protectedIn refuses a batch that touches any protected namespace, naming all
// of them at once.
func (g guard) protectedIn(namespaces []string) error {
	var blocked []string
	for _, ns := range namespaces {
		if slices.Contains(g.protected, ns) && !slices.Contains(blocked, ns) {
			blocked = append(blocked, ns)
		}
	}
	if len(blocked) == 0 {
		return nil
	}
	return refusef("the batch targets protected namespace(s) %s, so none of it ran. "+
		"Protected namespaces are %s.", strings.Join(blocked, ", "), strings.Join(g.protected, ", "))
}

func namespacesOf[T any](targets []T, ns func(T) string) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		out = append(out, ns(t))
	}
	return out
}

// encodePatch renders a patch body. Marshalled rather than fmt-ed so a value
// containing a quote produces valid JSON instead of a command kubectl rejects
// halfway through parsing.
func encodePatch(v any) (string, error) {
	blob, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("kubewrite: encode patch: %w", err)
	}
	return string(blob), nil
}
