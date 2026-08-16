package sre

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/go-steer/k8s-sre-agent/internal/schema"
)

// The checks in this file all police the same thing: a finding's three identity
// fields (kind / resource_name / reason) are the fingerprint internal/monitor
// diffs runs on, so a finding whose prose is perfect and whose fields are not
// usable is a paragraph rather than a finding. `unidentifiedFindings` in
// report.go covers the case where a field is *absent*; these cover the two ways
// a present field still fails to identify anything, both of which were measured
// against a real cluster rather than imagined.
//
// Both share three properties, and each one is a decision:
//
//   - They run at submission, not in the evaluator. Both lapses are
//     intermittent — the same agent on the same fixture files the fields
//     correctly on the next run — and an intermittent contract violation is
//     worse than a constant one, because it makes a score that moved for this
//     reason indistinguishable from one that moved because the diagnosis
//     changed. Catching it while the model still holds the evidence costs one
//     round trip and removes the variance.
//   - They fail *open*. An unrecognised reason token or an unmapped kind is
//     accepted, so an incomplete table under-enforces rather than over-rejects.
//     That is the same judgement call that keeps `namespace` unchecked, and the
//     one `genericReasons` got wrong twice in one session by scoring honest use
//     of kubectl's own vocabulary as a lie.
//   - They are hermetic. No cluster, no model: the input is a HealthReport.

// crossLayerReason rejects a reason token that names a failure mode the
// finding's own object cannot have.
//
// # The defect
//
// In every recorded occurrence the agent attaches a reason token to an object
// one layer *above* the layer that emits it:
//
//	PersistentVolumeClaim/ledger-data  reason=Unschedulable
//	Deployment/gemma4-vllm             reason=Unschedulable
//
// A PVC is not unschedulable; pods are. Nothing about `gemma4-vllm` was ever
// scheduled at all — no pod was created, and the ReplicaSet carries
// ReplicaFailure/FailedCreate. The prose is right in both cases and only the
// fingerprint field is wrong, which is what makes this invisible to every
// evaluator that reads the narrative.
//
// # Why this is not a prompt fix
//
// It was tried. An explicit orchestrator instruction ("take delta's reason as
// the failure mode of the object it is attached to — it is the control plane's
// own token") shipped, and fault-ledger came back 0.00 with reason=Unschedulable
// for the third consecutive run. Same lesson as stall.go: the prompt is
// necessary and not sufficient.
//
// # What the check is actually aimed at
//
// The positive control (2026-08-14, 17:44 against 18:24, same fixture and same
// model) narrows the diagnosis considerably. The run that scored 1.00 filed
// three findings and gave each object its own failure mode — the pod is
// Unschedulable, the PVC is *PVCPending*. The run that scored 0.00 filed two,
// collapsing the pod into the PVC and carrying the pod's reason onto the PVC's
// row. So the agent is not missing a vocabulary for a PVC's failure mode: it
// has one and used it unprompted. The borrow happens at the moment two findings
// are merged, and merging is otherwise good behaviour — an operator does not
// want three findings for one broken volume.
//
// Hence the wording of the rejection, which matters more than the rule. The
// obvious way to satisfy a layer check is to re-file the finding against the
// pod, and that is the object-choice instability the finding diff already has
// to survive (a fault fingerprinted as the Deployment on one run and as its own
// pod on the next reads as one incident resolved and another opened). The
// message therefore says *name this object's own failure mode* and never
// mentions the pod.
//
// # Scope: kinds
//
// Only the kinds that can never carry a workload's failure modes are mapped —
// see resourceLayers. Workload controllers are deliberately absent even though
// gemma4-vllm is the more reproducible miss, because "Deployment:
// Unschedulable" is at least arguable as a summary of its pods' state, and this
// half is not.
//
// # The second signal, and why it is not a per-kind allowlist
//
// The first live run after v1 shipped measured what a known-token table cannot
// do: `Service/session-store reason=PodsNotReady`, the PVC defect on a mapped
// kind, accepted because the agent *coined* the token and no table of tokens
// Kubernetes emits will ever contain one Kubernetes does not write. The obvious
// repair is to flip the polarity to a per-kind allowlist — "a Service may say
// these things" — and that was rejected, because it is closed-world in the one
// dimension that cannot be enumerated. The suite now contains the proof:
// fault-invoicing's only correct answer is a coined token on a Pod
// (`ConnectionRefused`, `Unreachable`, `DependencyFailure`), so an allowlist
// broad enough to be honest would not be an allowlist.
//
// What is enumerable is the set of *kinds*, and that is the signal this uses. A
// coined reason that borrows across a layer nearly always says whose state it
// borrowed, in its first word: `PodsNotReady` opens by naming pods. So a
// compound whose leading word is another object's kind noun is refused, and
// everything else is accepted. Both routes fail open and neither constrains
// vocabulary — see borrowedLayer.
//
// The restriction to the *leading* word is the whole of the conservatism, and
// the fixtures show why it is the right cut. `fault-badselector` accepts
// `NoMatchingPods` on a Service and `cascade` accepts `NoReadyEndpoints`; both
// mention another layer and both are honest, because they describe the
// Service's own relationship to those pods rather than the pods' condition.
// `PodsNotReady` is the same three words with the subject moved to the front,
// and that is exactly the difference between describing this object and
// reporting another one's state on its row.
func crossLayerReasons(h *schema.HealthReport) []string {
	var problems []string
	for i, f := range h.Findings {
		object, known := resourceLayers[normalizeToken(f.Kind)]
		if !known {
			continue
		}
		emitter, describe, known := borrowedLayer(f.Reason)
		if !known || emitter == object {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"finding %d (%q): reason %q is %s, but this finding names a %s. Name this "+
				"object's own failure mode — what is wrong with the %s itself. Keep the "+
				"finding on the object you chose; a different object failing is a separate "+
				"finding, not a different reason on this one",
			i+1, f.Title, f.Reason, describe, f.Kind, f.Kind))
	}
	return problems
}

// borrowedLayer resolves a reason to the layer it belongs to, by two routes
// that are both open-world, and reports how to describe it to the model.
//
// Route one is reasonLayers: the exact normalized token, for the vocabulary
// Kubernetes actually emits. Route two is the leading kind noun, for the
// vocabulary the agent invents. An unrecognised token takes neither and is
// accepted against every kind, which is the fail-open rule this whole file
// runs on.
//
// The order matters in exactly one place and the known token wins there:
// `PVCPending` on a PersistentVolumeClaim is a mapped volume token *and* opens
// with the noun "pvc", and both routes agree, but only the first stays right if
// a future noun table is spelled differently.
func borrowedLayer(reason string) (layer, string, bool) {
	norm := normalizeToken(reason)
	if l, ok := reasonLayers[norm]; ok {
		return l, layerDescriptions[l], true
	}
	if l, ok := leadingSubject(norm); ok {
		return l, subjectDescriptions[l], true
	}
	return 0, "", false
}

// leadingSubject returns the layer named by the kind noun a coined reason opens
// with, if it opens with one.
//
// Three restrictions, each of which keeps an honest token out of the net:
//
//   - Leading only. A noun anywhere else is usually the object of the sentence
//     rather than its subject — `NoMatchingPods` and `ServiceHasNoEndpoints`
//     are about the object filing them.
//   - Proper prefix. A reason that is nothing but a kind noun names no failure
//     mode at all, which is a different complaint than this one and not one
//     this check is calibrated to make.
//   - Reference failures are exempt. `<Kind>NotFound` and its spellings assert
//     something about a reference this object holds, which is this object's own
//     problem: `StorageClassNotFound` on a PVC and `ServiceNotFound` on an
//     Ingress are both the right answer, and both open with another kind's
//     noun. This also absorbs the near-miss the corpus turned up —
//     `PodDisruptionBudgetMissing` opens with "pod" and has nothing to do with
//     one.
func leadingSubject(norm string) (layer, bool) {
	for _, suffix := range referenceFailures {
		if strings.HasSuffix(norm, suffix) {
			return 0, false
		}
	}
	// A reason that is nothing but a kind noun, checked before the prefix scan
	// rather than by the proper-prefix guard below, because the guard cannot
	// see it: a plural is a proper prefix away from its own singular, so bare
	// "Pods" would match the noun "pod" and be read as a borrow.
	if _, whole := subjectNouns[norm]; whole {
		return 0, false
	}
	// Longest first, so "persistentvolumeclaim" is not read as "persistentvolume"
	// and "podsnotready" is not read as "pod" with a stray "s".
	for _, noun := range subjectNounsByLength {
		if len(norm) > len(noun) && strings.HasPrefix(norm, noun) {
			return subjectNouns[noun], true
		}
	}
	return 0, false
}

// referenceFailures are the suffixes that make a reason a statement about a
// reference rather than about the referenced object's state.
var referenceFailures = []string{
	"notfound", "missing", "notconfigured", "undefined", "unset", "notspecified",
}

// subjectNouns maps a Kubernetes kind noun to the layer it belongs to, for
// resolving the leading word of a coined reason.
//
// This is the table that replaces a per-kind allowlist, and the difference is
// what makes it safe to have: it enumerates kinds, which are finite and known,
// and says nothing at all about which failures each kind may have. A kind noun
// missing from here means a coined token opening with it is accepted, the same
// fail-open behaviour as an unmapped reason.
var subjectNouns = map[string]layer{
	"pod":           layerPod,
	"pods":          layerPod,
	"container":     layerContainer,
	"containers":    layerContainer,
	"initcontainer": layerContainer,
	"node":          layerNode,
	"nodes":         layerNode,
	"deployment":    layerController,
	"replicaset":    layerController,
	// A Job is a controller for this purpose: it is the object its own
	// backoff and deadline tokens are written onto.
	"statefulset": layerController,
	"daemonset":   layerController,
	"job":         layerController,
	"cronjob":     layerController,
	// "rollout" was here and was removed before it shipped. `Service:
	// RolloutIncomplete` is genuinely wrong and this would have caught it, but
	// a rollout is a concept rather than a kind, and the one property that
	// makes this table safe to have is that it enumerates something finite and
	// known. A concept noun is where that stops being true, and the compounds
	// that matter are caught anyway: DeploymentRolloutStuck opens with
	// "deployment".
	// Endpoints and EndpointSlices are a Service's own sub-objects, so a
	// Service naming them is describing itself.
	"endpoint":              layerService,
	"endpoints":             layerService,
	"endpointslice":         layerService,
	"service":               layerService,
	"ingress":               layerService,
	"volume":                layerVolume,
	"pvc":                   layerVolume,
	"persistentvolume":      layerVolume,
	"persistentvolumeclaim": layerVolume,
	"configmap":             layerConfig,
	"secret":                layerConfig,
}

// subjectNounsByLength is subjectNouns' keys, longest first, so prefix matching
// resolves the most specific noun.
var subjectNounsByLength = longestFirst(subjectNouns)

func longestFirst(m map[string]layer) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}

// subjectDescriptions says what a coined reason's leading noun makes it, in the
// words the rejection uses. Deliberately not layerDescriptions: that one tells
// the model the token is a condition the control plane wrote, and the whole
// point of this route is that it is a token the model invented.
var subjectDescriptions = map[layer]string{
	layerContainer:  "a statement about a container, not about this object",
	layerPod:        "a statement about a Pod, not about this object",
	layerController: "a statement about a workload controller, not about this object",
	layerVolume:     "a statement about a volume, not about this object",
	layerNode:       "a statement about a Node, not about this object",
	layerService:    "a statement about a Service, not about this object",
}

// layer is the part of Kubernetes that writes a condition or event reason.
type layer int

const (
	layerContainer layer = iota + 1
	layerPod
	layerController
	layerVolume
	layerNode
	layerService
	layerConfig
)

// layerDescriptions says who writes a token, in the words the rejection uses.
// The point is to send the agent somewhere specific rather than to tell it the
// answer is wrong.
var layerDescriptions = map[layer]string{
	layerContainer:  "a container-level condition, written into a Pod's containerStatuses by the kubelet",
	layerPod:        "a Pod-level condition, written onto a Pod by the scheduler or the kubelet",
	layerController: "a controller-level condition, written onto a Deployment, ReplicaSet, StatefulSet or Job",
	layerVolume:     "a volume-binding condition, written onto a PersistentVolumeClaim or PersistentVolume",
	layerNode:       "a Node-level condition",
}

// reasonLayers maps a known reason token to the layer that emits it.
//
// Membership is by exact normalized token, never by substring: `MatchesReason`
// in internal/evals was a bidirectional substring match, and the bare token
// `Failed` matched `FailedMount` while `Error` matched `ImagePullError`, which
// resolved honest tokens to the wrong family twice. A token absent from this
// map is accepted against any kind.
var reasonLayers = map[string]layer{
	// The kubelet writes these into containerStatuses[*].state, never onto an
	// object that has no containers.
	"crashloopbackoff":           layerContainer,
	"oomkilled":                  layerContainer,
	"imagepullbackoff":           layerContainer,
	"errimagepull":               layerContainer,
	"invalidimagename":           layerContainer,
	"imageinspecterror":          layerContainer,
	"createcontainerconfigerror": layerContainer,
	"createcontainererror":       layerContainer,
	"runcontainererror":          layerContainer,
	"containercannotrun":         layerContainer,
	"starterror":                 layerContainer,

	// The scheduler and the kubelet write these onto a Pod. Unschedulable is
	// the token this whole check exists for: it has now been attached to a PVC
	// and to a Deployment across six occurrences, and to a pod correctly in the
	// one run that scored.
	"unschedulable":      layerPod,
	"failedscheduling":   layerPod,
	"evicted":            layerPod,
	"nodeaffinity":       layerPod,
	"failedmount":        layerPod,
	"failedattachvolume": layerPod,

	// Workload controllers write these onto the controller object.
	"replicafailure":             layerController,
	"failedcreate":               layerController,
	"progressdeadlineexceeded":   layerController,
	"backofflimitexceeded":       layerController,
	"deadlineexceeded":           layerController,
	"minimumreplicasunavailable": layerController,

	// The PV controller writes these onto the claim or the volume.
	"volumebindingfailed":  layerVolume,
	"provisioningfailed":   layerVolume,
	"waitforfirstconsumer": layerVolume,
	"waitforpodscheduled":  layerVolume,
	"pvcpending":           layerVolume,
	"failedbinding":        layerVolume,

	// Node conditions.
	"memorypressure":     layerNode,
	"diskpressure":       layerNode,
	"pidpressure":        layerNode,
	"networkunavailable": layerNode,
	"kubeletnotready":    layerNode,
}

// resourceLayers maps a kind to the layer whose vocabulary it may use.
//
// v1 covers only the kinds for which the rejection cannot misfire:
//
//   - ConfigMap and Secret have no status subresource at all, so no control
//     plane component writes any failure token onto them.
//   - A PersistentVolumeClaim and a PersistentVolume have exactly one failure
//     vocabulary, the binding one, and the agent has already demonstrated it
//     knows it.
//   - A Service and an Ingress cannot crash, be OOM-killed, be scheduled, or be
//     rolled out.
//
// Pod, the workload controllers and Node are deliberately unmapped, so a
// borrowed token on any of them still passes. That is v2 and it is the arguable
// half; see the scope note on crossLayerReasons.
//
// Aliases are here because the field is prose from a model, not an API object:
// `kubectl` itself accepts `pvc` and `svc`, and a finding that spells the kind
// short is identifying its object perfectly well.
var resourceLayers = map[string]layer{
	"persistentvolumeclaim": layerVolume,
	"pvc":                   layerVolume,
	"persistentvolume":      layerVolume,
	"pv":                    layerVolume,
	"service":               layerService,
	"svc":                   layerService,
	"ingress":               layerService,
	"ing":                   layerService,
	"configmap":             layerConfig,
	"cm":                    layerConfig,
	"secret":                layerConfig,
}

// tokenNoise is everything that separates words in the many spellings of one
// reason: "CrashLoopBackOff", "crash-loop-back-off", "CRASH_LOOP_BACKOFF".
var tokenNoise = regexp.MustCompile(`[^a-z0-9]+`)

// normalizeToken folds a token to its comparable form. Case and separators are
// noise; nothing else is removed, so two genuinely different tokens can never
// collapse into one.
func normalizeToken(s string) string {
	return tokenNoise.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "")
}

// objectName is the shape of a single Kubernetes object name.
//
// Deliberately wider than RFC 1123: uppercase and underscores are accepted even
// though the API server would reject them, because a case difference does not
// stop a finding being fingerprinted (monitor.Fingerprint lowercases) and the
// failure being caught here is not "this name is invalid" but "this field does
// not hold one name".
var objectName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// maxNameLen is the API server's own bound on a DNS subdomain name. Past it,
// the field is holding something other than a name.
const maxNameLen = 253

// unnamedResources rejects a resource_name that is not one object's name.
//
// `unidentifiedFindings` only requires the field to be non-empty, and against a
// real cluster the agent filled it with three different things that are not
// identifiers:
//
//	fourteen comma-joined Deployment names
//	"(unspecified — 1 pod/1 container per top.unlimited scan)"
//	three " / "-joined ValidatingWebhookConfiguration names
//
// All three submissions passed, and none of the findings can be fingerprinted,
// deduplicated or closed: the first is fourteen findings wearing one coat, the
// second names nothing at all. The separator differs every time, so the check
// is on the shape of a name rather than on any list punctuation — a character
// that cannot appear in a Kubernetes object name is the one signal common to
// all three.
//
// Like the rest of this file the lapse is intermittent: the very next run filed
// the same class of namespace-wide advisory as `Namespace/online-boutique`,
// which is well formed.
func unnamedResources(h *schema.HealthReport) []string {
	var problems []string
	for i, f := range h.Findings {
		name := strings.TrimSpace(f.ResourceName)
		// Absence is unidentifiedFindings' complaint, and reporting it twice
		// would spend a round trip telling the model the same thing in two
		// voices.
		if name == "" {
			continue
		}
		if len(name) <= maxNameLen && objectName.MatchString(name) {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"finding %d (%q): resource_name %s is not the name of a single object. A finding "+
				"covers exactly one object, so file one finding per object rather than listing "+
				"several, and put only that object's metadata.name here — no namespace or kind "+
				"prefix, no description, no count. This field is how the finding is matched "+
				"against the next monitoring run",
			i+1, f.Title, quoteName(name)))
	}
	return problems
}

// quoteName echoes the offending value back, truncated. Echoing it matters —
// the model has to see which of its fields is meant — but a fourteen-name join
// quoted in full is most of the error message.
func quoteName(name string) string {
	const limit = 60
	if len(name) > limit {
		return fmt.Sprintf("%q… (%d characters)", name[:limit], len(name))
	}
	return fmt.Sprintf("%q", name)
}
