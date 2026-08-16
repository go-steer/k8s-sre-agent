package kuberead

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

// description is what the model reads when it decides whether to call this.
//
// Three things it has to say, each of which was a failure mode in the tier-2
// runs that produced this tool. That the other read tools need a
// `<Kind>/<namespace>/<name>` target, because the agent's problem was never
// that it lacked checks — it had nine — but that it could not name anything to
// point them at. That a clean health scan is not an empty namespace, because
// the fixture that failed is one where nothing is unhealthy. And that this is
// the first call for an unfamiliar namespace, because guessing object names is
// what the agent did instead, nineteen times in one run.
const description = "List the objects that exist in a namespace: workloads, pods, Services, " +
	"Endpoints, Ingresses, ConfigMaps, Secrets (names only), PVCs, HPAs, PDBs and quotas. " +
	"Returns one line per object as <Kind>/<namespace>/<name> — the exact target form the " +
	"other read tools take — followed by that object's headline status. " +
	"Call this first when you are asked about a namespace you have not enumerated yet. " +
	"The health scans (k8s_cluster_health, k8s_triage_delta) report only what is abnormal " +
	"and name nothing when a namespace is clean, so they cannot tell you what is in one; " +
	"this can, and a healthy object may still be misconfigured. " +
	"This is an inventory, not a diagnosis: take the names it gives you to k8s_state_edges, " +
	"k8s_resource_spec or k8s_triage_workload rather than concluding from it alone. " +
	"Never guess an object's name — list the namespace instead."

// listArgs is the tool's parameter set.
type listArgs struct {
	Namespace string   `json:"namespace" jsonschema:"the namespace to enumerate"`
	Kinds     []string `json:"kinds,omitempty" jsonschema:"restrict the listing to these resource kinds, as kubectl spells them (pods, deployments, services, replicasets, ...); omit for the default set, which covers every namespaced kind an incident normally involves except ReplicaSets"`
}

// Listing is what the tool returns.
type Listing struct {
	Namespace string `json:"namespace"`
	Count     int    `json:"count" jsonschema:"how many objects are listed"`
	Objects   string `json:"objects,omitempty" jsonschema:"one line per object: <Kind>/<namespace>/<name> followed by its headline status"`
	Note      string `json:"note,omitempty" jsonschema:"anything qualifying the listing, such as truncation"`
	Error     string `json:"error,omitempty" jsonschema:"why the listing could not be produced, when it could not"`
}

// defaultKinds is what a bare call enumerates.
//
// Ordered workloads first, then the routing objects, then configuration, so
// that a truncated listing loses the least important things. ReplicaSets are
// out: a namespace accumulates one per Deployment revision, they are an
// implementation detail of a Deployment rather than something an operator
// reasons about, and they would routinely be the bulk of the output. The
// `kinds` argument can still ask for them.
//
// Every entry is a built-in namespaced resource, so the list cannot fail
// against a cluster that merely lacks a CRD.
var defaultKinds = []string{
	"deployments", "statefulsets", "daemonsets", "cronjobs", "jobs", "pods",
	"services", "endpoints", "ingresses",
	"configmaps", "secrets", "persistentvolumeclaims",
	"horizontalpodautoscalers", "poddisruptionbudgets",
	"serviceaccounts", "networkpolicies", "resourcequotas", "limitranges",
}

// lister holds what the tool needs to answer a call.
type lister struct {
	run Runner
	max int
}

// listTool wraps a handler in the tool declaration. Shared by Toolset and
// Offline so the two surfaces cannot drift.
func listTool(h func(adkagent.Context, listArgs) (Listing, error)) (tool.Tool, error) {
	return functiontool.New(functiontool.Config{Name: ToolName, Description: description}, h)
}

// A namespace or kind is interpolated into a kubectl argument vector, so both
// are validated against a charset that cannot produce a flag or a path. Nothing
// here is shelled out — exec.Command takes an argv — but a kind of "--all" is
// still a flag to kubectl, and the refusal costs nothing.
var (
	dnsName  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	kindName = regexp.MustCompile(`^[a-z][a-z0-9.-]*$`)
)

// list enumerates one namespace.
//
// A refusal or a kubectl failure comes back as a Listing carrying Error rather
// than as a Go error, for the reason kubewrite returns refusals the same way: a
// tool error reads to a model as an accident worth retrying, and "that
// namespace does not parse" is a fact it should report instead.
func (l *lister) list(ctx adkagent.Context, a listArgs) (Listing, error) {
	ns := strings.TrimSpace(a.Namespace)
	if !dnsName.MatchString(ns) {
		return Listing{Namespace: a.Namespace,
			Error: fmt.Sprintf("%q is not a valid namespace name; nothing was listed", a.Namespace)}, nil
	}
	kinds := defaultKinds
	if len(a.Kinds) > 0 {
		kinds = make([]string, 0, len(a.Kinds))
		for _, k := range a.Kinds {
			k = strings.ToLower(strings.TrimSpace(k))
			if !kindName.MatchString(k) {
				return Listing{Namespace: ns,
					Error: fmt.Sprintf("%q is not a valid resource kind; nothing was listed", k)}, nil
			}
			kinds = append(kinds, k)
		}
	}

	// One invocation for every kind. kubectl resolves singular, plural and
	// short names itself, which is a table this package would otherwise have to
	// carry and keep current. --ignore-not-found keeps a namespace that holds
	// none of a kind from being an error.
	stdout, stderr, runErr := l.run.Output(ctx,
		"get", strings.Join(kinds, ","), "-n", ns, "-o", "json", "--ignore-not-found")
	if runErr != nil && strings.TrimSpace(stdout) == "" {
		msg := oneLine(stderr)
		if msg == "" {
			msg = runErr.Error()
		}
		return Listing{Namespace: ns, Error: "kubectl could not list the namespace: " + msg}, nil
	}

	items, err := decodeItems(stdout)
	if err != nil {
		return Listing{Namespace: ns, Error: err.Error()}, nil
	}

	out := Listing{Namespace: ns, Count: len(items)}
	var notes []string
	if stderr != "" {
		// kubectl can list most kinds and be refused one — an RBAC role without
		// secrets, typically, in which case it prints the refusal on stderr, the
		// rest on stdout, and exits non-zero. Reporting it matters more than it
		// looks: the difference between "there is no Secret here" and "I was not
		// allowed to look" is the difference between a finding and a blind spot.
		notes = append(notes, "kubectl reported: "+oneLine(stderr))
	}

	shown := items
	if len(shown) > l.max {
		shown = shown[:l.max]
		notes = append(notes, fmt.Sprintf("truncated: %d of %d objects shown, pass `kinds` to narrow the listing",
			len(shown), len(items)))
	}
	lines := make([]string, 0, len(shown))
	for _, it := range shown {
		lines = append(lines, render(it, ns))
	}
	out.Objects = strings.Join(lines, "\n")

	if len(items) == 0 {
		notes = append(notes, "no objects of these kinds; the namespace is either empty or does not exist")
	}
	out.Note = strings.Join(notes, "; ")
	return out, nil
}

// object is one decoded item, kept as a map because the point is to read a
// handful of status fields out of eighteen unrelated schemas.
type object struct {
	kind string
	name string
	raw  map[string]any
}

// decodeItems reads kubectl's JSON.
//
// `kubectl get a,b,c -o json` returns a v1.List whose items carry their own
// kind; `kubectl get a -o json` returns an aList whose items may not. Deriving
// the kind from the envelope when the item omits it covers both, which matters
// because the single-kind shape is what a caller passing one entry in `kinds`
// gets.
func decodeItems(stdout string) ([]object, error) {
	if strings.TrimSpace(stdout) == "" {
		return nil, nil
	}
	var envelope struct {
		Kind  string           `json:"kind"`
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		return nil, fmt.Errorf("could not parse kubectl's output: %v", err)
	}
	fallback := strings.TrimSuffix(envelope.Kind, "List")
	out := make([]object, 0, len(envelope.Items))
	for _, it := range envelope.Items {
		kind := strAt(it, "kind")
		if kind == "" {
			kind = fallback
		}
		if kind == "" {
			kind = "Object"
		}
		out = append(out, object{kind: kind, name: strAt(it, "metadata", "name"), raw: it})
	}
	return out, nil
}

// render turns one object into its line.
func render(o object, ns string) string {
	line := o.kind + "/" + ns + "/" + o.name
	if f := formatters[o.kind]; f != nil {
		if status := f(o.raw); status != "" {
			line += " " + status
		}
	}
	return line
}

// formatters give each kind its headline status.
//
// The rule for what belongs here is deliberately mechanical: the fields
// `kubectl get <kind>` prints in its own default table, and nothing else. That
// is what keeps this an inventory. It also settles the awkward cases by rule
// rather than by taste — an Endpoints object's address count is in, because
// `kubectl get endpoints` prints it, and a Service's selector is out, because
// `kubectl get svc` does not.
//
// A kind with no entry prints its name alone, which is the right answer for a
// ServiceAccount or a NetworkPolicy: existence *is* the fact.
var formatters = map[string]func(map[string]any) string{
	"Pod": func(o map[string]any) string {
		cs := sliceAt(o, "status", "containerStatuses")
		total := len(sliceAt(o, "spec", "containers"))
		ready, restarts, state := 0, 0, ""
		for _, c := range cs {
			m, _ := c.(map[string]any)
			if b, _ := m["ready"].(bool); b {
				ready++
			}
			restarts += intAt(m, "restartCount")
			if state == "" {
				if r := strAt(m, "state", "waiting", "reason"); r != "" {
					state = r
				} else if r := strAt(m, "state", "terminated", "reason"); r != "" {
					state = r
				}
			}
		}
		kv := newKV()
		kv.add("phase", strAt(o, "status", "phase"))
		kv.addf("ready", "%d/%d", ready, max(total, len(cs)))
		kv.addf("restarts", "%d", restarts)
		kv.add("state", state)
		kv.add("node", strAt(o, "spec", "nodeName"))
		return kv.String()
	},
	"Deployment":  replicaStatus("status", "readyReplicas"),
	"StatefulSet": replicaStatus("status", "readyReplicas"),
	"ReplicaSet":  replicaStatus("status", "readyReplicas"),
	"DaemonSet": func(o map[string]any) string {
		kv := newKV()
		kv.addf("ready", "%d/%d", intAt(o, "status", "numberReady"), intAt(o, "status", "desiredNumberScheduled"))
		return kv.String()
	},
	"Job": func(o map[string]any) string {
		want := 1
		if n := intAt(o, "spec", "completions"); n > 0 {
			want = n
		}
		kv := newKV()
		kv.addf("completions", "%d/%d", intAt(o, "status", "succeeded"), want)
		if failed := intAt(o, "status", "failed"); failed > 0 {
			kv.addf("failed", "%d", failed)
		}
		return kv.String()
	},
	"CronJob": func(o map[string]any) string {
		kv := newKV()
		kv.add("schedule", strAt(o, "spec", "schedule"))
		if b, _ := mapAt(o, "spec")["suspend"].(bool); b {
			kv.add("suspend", "true")
		}
		kv.addf("active", "%d", len(sliceAt(o, "status", "active")))
		return kv.String()
	},
	"Service": func(o map[string]any) string {
		var ports []string
		for _, p := range sliceAt(o, "spec", "ports") {
			m, _ := p.(map[string]any)
			proto := strAt(m, "protocol")
			if proto == "" {
				proto = "TCP"
			}
			ports = append(ports, fmt.Sprintf("%d/%s", intAt(m, "port"), proto))
		}
		kv := newKV()
		kv.add("type", strAt(o, "spec", "type"))
		kv.add("ports", strings.Join(ports, ","))
		return kv.String()
	},
	"Endpoints": func(o map[string]any) string {
		n := 0
		for _, s := range sliceAt(o, "subsets") {
			m, _ := s.(map[string]any)
			n += len(sliceAt(m, "addresses"))
		}
		kv := newKV()
		kv.addf("addresses", "%d", n)
		return kv.String()
	},
	"Ingress": func(o map[string]any) string {
		var hosts []string
		for _, r := range sliceAt(o, "spec", "rules") {
			m, _ := r.(map[string]any)
			if h := strAt(m, "host"); h != "" {
				hosts = append(hosts, h)
			}
		}
		kv := newKV()
		kv.add("class", strAt(o, "spec", "ingressClassName"))
		kv.add("hosts", strings.Join(hosts, ","))
		return kv.String()
	},
	"ConfigMap": func(o map[string]any) string {
		kv := newKV()
		kv.addf("keys", "%d", len(mapAt(o, "data"))+len(mapAt(o, "binaryData")))
		return kv.String()
	},
	// Secrets are counted, never read. lookout's checks are secret-safe by
	// design and an enumeration tool that leaks a value would undo that in one
	// line; the key count is what `kubectl get secrets` prints and is enough to
	// tell an empty Secret from a populated one.
	"Secret": func(o map[string]any) string {
		kv := newKV()
		kv.add("type", strAt(o, "type"))
		kv.addf("keys", "%d", len(mapAt(o, "data"))+len(mapAt(o, "stringData")))
		return kv.String()
	},
	"PersistentVolumeClaim": func(o map[string]any) string {
		kv := newKV()
		kv.add("phase", strAt(o, "status", "phase"))
		kv.add("capacity", strAt(o, "status", "capacity", "storage"))
		kv.add("class", strAt(o, "spec", "storageClassName"))
		return kv.String()
	},
	"HorizontalPodAutoscaler": func(o map[string]any) string {
		kv := newKV()
		kv.addf("replicas", "%d", intAt(o, "status", "currentReplicas"))
		kv.addf("min", "%d", intAt(o, "spec", "minReplicas"))
		kv.addf("max", "%d", intAt(o, "spec", "maxReplicas"))
		kv.add("target", strAt(o, "spec", "scaleTargetRef", "kind")+"/"+strAt(o, "spec", "scaleTargetRef", "name"))
		return kv.String()
	},
	"PodDisruptionBudget": func(o map[string]any) string {
		kv := newKV()
		kv.addf("allowed_disruptions", "%d", intAt(o, "status", "disruptionsAllowed"))
		kv.addf("healthy", "%d/%d", intAt(o, "status", "currentHealthy"), intAt(o, "status", "desiredHealthy"))
		return kv.String()
	},
}

// replicaStatus is the ready=n/m shape the three replica-carrying workload
// kinds share.
func replicaStatus(readyPath ...string) func(map[string]any) string {
	return func(o map[string]any) string {
		want := 1
		if _, ok := mapAt(o, "spec")["replicas"]; ok {
			want = intAt(o, "spec", "replicas")
		}
		kv := newKV()
		kv.addf("ready", "%d/%d", intAt(o, readyPath...), want)
		return kv.String()
	}
}

// kv accumulates logfmt pairs, skipping the empty ones.
//
// Skipping matters for density: a Pod that was never scheduled has no node and
// no waiting reason, and `node= state=` on every Pending pod is noise the model
// pays for by the token.
type kv struct{ parts []string }

func newKV() *kv { return &kv{} }

func (k *kv) add(key, value string) {
	if value == "" || value == "/" {
		return
	}
	if strings.ContainsAny(value, " \t\"") {
		value = strconv.Quote(value)
	}
	k.parts = append(k.parts, key+"="+value)
}

func (k *kv) addf(key, format string, args ...any) { k.add(key, fmt.Sprintf(format, args...)) }

func (k *kv) String() string { return strings.Join(k.parts, " ") }

// oneLine collapses kubectl's multi-line stderr so a note stays a note.
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

// ---- accessors over decoded JSON -------------------------------------------
//
// Unmarshalling into eighteen typed structs would be the alternative, and it
// would mean vendoring k8s.io/api — a very large dependency for reading two
// fields per kind. These walk the map instead and return a zero value for
// anything absent, which is the right behaviour here: a Deployment mid-rollout
// genuinely has no status.readyReplicas, and 0 is what that means.

func mapAt(m map[string]any, path ...string) map[string]any {
	for _, p := range path {
		next, ok := m[p].(map[string]any)
		if !ok {
			return nil
		}
		m = next
	}
	return m
}

func strAt(m map[string]any, path ...string) string {
	v := valueAt(m, path...)
	s, _ := v.(string)
	return s
}

func intAt(m map[string]any, path ...string) int {
	// JSON numbers decode to float64; ints are what every field here holds.
	f, _ := valueAt(m, path...).(float64)
	return int(f)
}

func sliceAt(m map[string]any, path ...string) []any {
	s, _ := valueAt(m, path...).([]any)
	return s
}

func valueAt(m map[string]any, path ...string) any {
	if len(path) == 0 {
		return nil
	}
	parent := mapAt(m, path[:len(path)-1]...)
	if parent == nil {
		return nil
	}
	return parent[path[len(path)-1]]
}
