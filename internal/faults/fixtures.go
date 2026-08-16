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

package faults

import (
	"fmt"

	"github.com/go-steer/k8s-sre-agent/internal/schema"
)

// All returns the live-tier fixture set.
//
// # The single-fault six
//
// The set is deliberately small and deliberately varied. Six faults across
// six failure classes tell you more than sixty variations of CrashLoopBackOff,
// because the thing being measured is whether the agent picks the right check
// for the symptom — and a fixture set weighted toward one symptom rewards an
// agent that has one reflex.
//
// The seventh fixture is healthy on purpose. Recall alone is trivially gamed
// by an agent that reports every failure mode it knows about in every
// namespace; the healthy namespace is what makes precision cost something.
//
// # The discriminating three
//
// Those seven ran out of headroom. Once the read path grew an enumeration tool
// the agent scored twenty of twenty-one fixture-metric cells at the ceiling in
// two consecutive runs, and delegation collapsed to two or three fixtures out
// of seven — because a namespace with exactly one broken workload is fully
// diagnosed by enumerate-then-triage, which one agent can do alone. A suite
// that can only detect catastrophic regressions cannot tell a working
// eight-specialist fan-out from an orchestrator ignoring it.
//
// So three fixtures were added, each attacking a different way of being right
// for the wrong reason:
//
//   - multipleFaults: three unrelated faults in one namespace. An agent that
//     stops at the first thing it finds scores 0.333, and finding all three is
//     work that actually wants breadth.
//   - cascade: a crash loop and the Service it takes down. Both are true, and
//     an agent that reports only the outage has named the symptom.
//   - unboundVolume: pods Pending, but not for the reason Pending usually
//     means. The reflexive read is wrong and there is no resource pressure to
//     find.
//
// cascade is the one that carries a Root want, which is what root_cause
// grades; the other two discriminate on recall and hallucination alone.
//
// # The eleventh, which is about the read path rather than about breadth
//
// Those ten all have one thing in common that nobody chose: every fault in
// them is visible in object status. That is why the crash-loop fixture writes
// `FATAL: connection refused` to stderr and is still diagnosed off pod status
// — the log is corroboration, never the evidence — and it is most of why
// log-analyzer has been invoked on 0, 1, 0, 0, 0, 1 of the ten across six
// runs. A specialist that is never *needed* cannot be shown to work, and a
// roster can be nominally "all 8 used" while one of them is decoration.
//
//   - silentFailure: a workload that is Running, Ready and unrestarted, and
//     is doing none of its job. Nothing in status, the graph or the event
//     stream says so; the evidence is what the container is writing.
//
// It scored 1.00 on its first run, which was not the expectation and is worth
// reading carefully rather than as a pass — see the note on the function.
func All() []Fault {
	return []Fault{
		imagePull(),
		crashLoop(),
		oomKill(),
		unschedulable(),
		failedJob(),
		serviceSelectorMismatch(),
		healthy(),
		multipleFaults(),
		cascade(),
		unboundVolume(),
		silentFailure(),
	}
}

// ByName returns the named fixtures, in the order given.
func ByName(names []string) ([]Fault, error) {
	index := map[string]Fault{}
	for _, f := range All() {
		index[f.Name] = f
	}
	out := make([]Fault, 0, len(names))
	for _, n := range names {
		f, ok := index[n]
		if !ok {
			return nil, fmt.Errorf("faults: no fixture named %q", n)
		}
		out = append(out, f)
	}
	return out, nil
}

// checkNamespace is the only thing the agent is told. It names the task and
// the scope and nothing else — no symptom, no workload name, no hint that
// anything is wrong at all. An agent that is told "the checkout pods are
// crash-looping" is being graded on paraphrase; this tier grades diagnosis.
func checkNamespace(ns string) string {
	return fmt.Sprintf("Assess the health of the %q namespace in this Kubernetes cluster "+
		"and report what you find.", ns)
}

func imagePull() Fault {
	const ns = "fault-imagepull"
	return Fault{
		Name:     ns,
		Prompt:   checkNamespace(ns),
		Manifest: deployment(ns, "checkout-api", 2, MissingImage, nil, "", ""),
		Settle: []Condition{{
			Describe: "pods to report an image pull failure",
			Args: []string{"get", "pods", "-n", ns, "-o",
				"jsonpath={.items[*].status.containerStatuses[*].state.waiting.reason}"},
			Contains: "ImagePullBackOff",
		}},
		WantSeverity: schema.OverallCritical,
		Want: []Want{{
			Kind:            "Pod",
			Name:            "checkout-api",
			AlsoAcceptKinds: []string{"Deployment", "ReplicaSet"},
			// ErrImagePull is the same fault a few seconds earlier; kubelet
			// alternates between the two while backing off.
			Reasons:     []string{"ImagePullBackOff", "ErrImagePull", "ImagePullError"},
			MinSeverity: schema.SeverityWarning,
		}},
	}
}

// crashLoopSettle waits for a workload's containers to actually be observable
// as CrashLoopBackOff, which takes far longer than a crash loop takes to start.
//
// The state a poll can see is not the state you would expect. Through the early
// restarts the container sits in `state.terminated{reason: Error}` for the whole
// backoff window and never appears as `state.waiting{reason: CrashLoopBackOff}`
// at all — the kubelet restarts the container before a status write lands.
// `waiting` only becomes observable once the backoff has grown long enough to
// outlast a sync, which measured out at the **1m20s** step: 10s + 20s + 40s +
// 80s of backoff plus the container's own runtime, so roughly **170 seconds**
// after the pod first starts.
//
// That is the number the caller's settle budget has to cover, and it is why
// kindcluster.Create passes `--wait`: a cold node's 75-second scheduling delay
// used to land in front of this, and 75 + 170 overran the four-minute budget
// this test suite used. fault-crashloop had been passing by seconds; cascade,
// with two replicas, was the one that finally did not — see the note there.
//
// Waiting on the earlier, monotonic evidence instead (the kubelet's BackOff
// event, which shows up in ~25s) would settle sooner and would be wrong: it
// would hand the agent a namespace whose pods still read `Error`, and every
// crash-loop Want grades on the CrashLoopBackOff token. A settle condition has
// to mean "the agent can now observe the fault", not "the fault has begun".
func crashLoopSettle(ns string) Condition {
	return Condition{
		Describe: "the pods to be observable as CrashLoopBackOff",
		Args: []string{"get", "pods", "-n", ns, "-o",
			"jsonpath={.items[*].status.containerStatuses[*].state.waiting.reason}"},
		Contains: "CrashLoopBackOff",
	}
}

func crashLoop() Fault {
	const ns = "fault-crashloop"
	return Fault{
		Name:   ns,
		Prompt: checkNamespace(ns),
		Manifest: deployment(ns, "payments-worker", 1, BaseImage, []string{
			"sh", "-c", "echo 'connecting to payments db'; sleep 2; echo 'FATAL: connection refused' >&2; exit 1",
		}, "", ""),
		Settle:       []Condition{crashLoopSettle(ns)},
		WantSeverity: schema.OverallCritical,
		Want: []Want{{
			Kind:            "Pod",
			Name:            "payments-worker",
			AlsoAcceptKinds: []string{"Deployment", "ReplicaSet"},
			Reasons:         []string{"CrashLoopBackOff", "CrashLoop"},
			MinSeverity:     schema.SeverityWarning,
		}},
	}
}

func oomKill() Fault {
	const ns = "fault-oomkill"
	// Anonymous memory, not tmpfs. The obvious way to burn memory from busybox
	// is `dd` into /dev/shm, and it does trigger the cgroup limit — but /dev/shm
	// belongs to the *pod sandbox*, not the container, so it survives the
	// restart still full. The next container's runc init then has no headroom
	// and dies with StartError/RunContainerError before it ever runs. The fault
	// is real but its reason is wrong, which would grade an agent that
	// correctly read the cluster as having missed an OOM kill.
	//
	// Piping /dev/zero through `tail -n 1` forces the whole stream into tail's
	// buffer as anonymous memory, which the kernel reclaims when it kills the
	// process. That yields a clean repeating OOMKilled with exit code 137.
	return Fault{
		Name:   ns,
		Prompt: checkNamespace(ns),
		Manifest: deployment(ns, "cache-warmer", 1, BaseImage, []string{
			"sh", "-c", "echo 'warming cache'; head -c 400000000 /dev/zero | tail -n 1; sleep 3600",
		}, "32Mi", "64Mi"),
		Settle: []Condition{{
			Describe: "the container to be OOM killed",
			// Matched across the whole container status rather than one field:
			// the kill shows up in state.terminated while the container is
			// down and in lastState.terminated once it restarts, and which one
			// a poll catches is a race.
			Args:     []string{"get", "pods", "-n", ns, "-o", "jsonpath={.items[*].status.containerStatuses[*]}"},
			Contains: "OOMKilled",
		}},
		WantSeverity: schema.OverallCritical,
		Want: []Want{{
			Kind:            "Pod",
			Name:            "cache-warmer",
			AlsoAcceptKinds: []string{"Deployment", "ReplicaSet"},
			Reasons:         []string{"OOMKilled", "OutOfMemory", "MemoryLimitExceeded"},
			MinSeverity:     schema.SeverityWarning,
		}},
	}
}

func unschedulable() Fault {
	const ns = "fault-unschedulable"
	// 64 cores on a single-node kind cluster. Requesting something the cluster
	// cannot satisfy is the honest version of this fault: a taint or a node
	// selector would test the same code path but would need a second node to
	// be a realistic shape.
	return Fault{
		Name:     ns,
		Prompt:   checkNamespace(ns),
		Manifest: deploymentWithCPURequest(ns, "analytics-etl", 1, BaseImage, "64"),
		Settle: []Condition{
			{
				Describe: "the pod to be Pending",
				Args:     []string{"get", "pods", "-n", ns, "-o", "jsonpath={.items[*].status.phase}"},
				Contains: "Pending",
			},
			{
				Describe: "the scheduler to record why",
				Args: []string{"get", "pods", "-n", ns, "-o",
					"jsonpath={.items[*].status.conditions[*].reason}"},
				Contains: "Unschedulable",
			},
		},
		WantSeverity: schema.OverallCritical,
		Want: []Want{{
			Kind:            "Pod",
			Name:            "analytics-etl",
			AlsoAcceptKinds: []string{"Deployment", "ReplicaSet"},
			Reasons: []string{
				"Unschedulable", "FailedScheduling", "InsufficientCPU",
				"InsufficientResources", "Pending",
			},
			MinSeverity: schema.SeverityWarning,
		}},
	}
}

func failedJob() Fault {
	const ns = "fault-failedjob"
	return Fault{
		Name:   ns,
		Prompt: checkNamespace(ns),
		Manifest: fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: nightly-report
  namespace: %s
spec:
  backoffLimit: 1
  template:
    metadata:
      labels: {app: nightly-report}
    spec:
      restartPolicy: Never
      containers:
      - name: report
        image: %s
        command: ["sh", "-c", "echo 'building nightly report'; sleep 1; echo 'ERROR: source table missing' >&2; exit 2"]
`, ns, BaseImage),
		Settle: []Condition{{
			Describe: "the job to fail",
			Args: []string{"get", "job", "nightly-report", "-n", ns, "-o",
				"jsonpath={.status.conditions[*].type}"},
			Contains: "Failed",
		}},
		WantSeverity: schema.OverallWarning,
		Want: []Want{{
			Kind:            "Job",
			Name:            "nightly-report",
			AlsoAcceptKinds: []string{"Pod", "CronJob"},
			Reasons: []string{
				"BackoffLimitExceeded", "JobFailed", "Failed", "DeadlineExceeded", "Error",
			},
		}},
	}
}

func serviceSelectorMismatch() Fault {
	const ns = "fault-badselector"
	// The pods are healthy and the Service exists; only the selector is wrong.
	// This is the fixture that separates an agent which reads pod status from
	// one which reads the graph — nothing in `get pods` is abnormal here.
	return Fault{
		Name:   ns,
		Prompt: checkNamespace(ns),
		Manifest: deployment(ns, "frontend", 2, BaseImage, []string{"sh", "-c", "sleep 3600"}, "64Mi", "128Mi") +
			fmt.Sprintf(`---
apiVersion: v1
kind: Service
metadata:
  name: frontend
  namespace: %s
spec:
  selector:
    app: frontend-v2
  ports:
  - port: 80
    targetPort: 8080
`, ns),
		Settle: []Condition{
			{
				Describe: "the deployment to become available",
				Args: []string{"get", "deployment", "frontend", "-n", ns, "-o",
					"jsonpath={.status.availableReplicas}"},
				Contains: "2",
			},
			{
				Describe: "the service to have no ready endpoints",
				Args: []string{"get", "endpointslices", "-n", ns,
					"-l", "kubernetes.io/service-name=frontend",
					"-o", "jsonpath={.items[*].endpoints[*].addresses[*]}"},
				Empty: true,
			},
		},
		WantSeverity: schema.OverallWarning,
		Want: []Want{{
			Kind:            "Service",
			Name:            "frontend",
			AlsoAcceptKinds: []string{"Endpoints", "EndpointSlice", "Deployment"},
			Reasons: []string{
				"SelectorMismatch", "NoEndpoints", "NoMatchingPods",
				"EmptyEndpoints", "ServiceHasNoEndpoints", "LabelMismatch",
			},
		}},
	}
}

func healthy() Fault {
	const ns = "fault-none"
	// No fault. Want is empty and WantSeverity is ok, so this fixture scores
	// precision only: every warning-or-worse finding here is a false positive.
	// It is the floor that stops "report everything" from being a strategy.
	return Fault{
		Name:         ns,
		Prompt:       checkNamespace(ns),
		Manifest:     deployment(ns, "docs-site", 2, BaseImage, []string{"sh", "-c", "sleep 3600"}, "64Mi", "128Mi"),
		WantSeverity: schema.OverallOK,
		Settle: []Condition{{
			Describe: "the deployment to become available",
			Args: []string{"get", "deployment", "docs-site", "-n", ns, "-o",
				"jsonpath={.status.availableReplicas}"},
			Contains: "2",
		}},
	}
}

// multipleFaults puts three unrelated faults in one namespace.
//
// Every other faulty fixture has exactly one thing wrong with it, which means
// an agent is finished the moment it finds anything. Here it is not: the
// namespace holds an image that cannot be pulled, a workload that cannot be
// scheduled, and a Job that has given up, and recall is a third for each. The
// three are deliberately from different failure classes, so no single check
// answers all of them and no single specialist owns them.
//
// The namespace is named for what it hosts rather than for what is wrong with
// it — see cascade for why that matters more here than it did for the six.
func multipleFaults() Fault {
	const ns = "fault-storefront"
	return Fault{
		Name:   ns,
		Prompt: checkNamespace(ns),
		Manifest: deployment(ns, "orders-api", 2, MissingImage, nil, "", "") +
			"---\n" + deploymentWithCPURequest(ns, "recommendation-etl", 1, BaseImage, "64") +
			"---\n" + failingJob(ns, "inventory-sync"),
		Settle: []Condition{
			{
				Describe: "the orders-api pods to report an image pull failure",
				Args: []string{"get", "pods", "-n", ns, "-l", "app=orders-api", "-o",
					"jsonpath={.items[*].status.containerStatuses[*].state.waiting.reason}"},
				Contains: "ImagePullBackOff",
			},
			{
				Describe: "the recommendation-etl pod to be unschedulable",
				Args: []string{"get", "pods", "-n", ns, "-l", "app=recommendation-etl", "-o",
					"jsonpath={.items[*].status.conditions[*].reason}"},
				Contains: "Unschedulable",
			},
			{
				Describe: "the inventory-sync job to fail",
				Args: []string{"get", "job", "inventory-sync", "-n", ns, "-o",
					"jsonpath={.status.conditions[*].type}"},
				Contains: "Failed",
			},
		},
		WantSeverity: schema.OverallCritical,
		Want: []Want{
			{
				Kind:            "Pod",
				Name:            "orders-api",
				AlsoAcceptKinds: []string{"Deployment", "ReplicaSet"},
				Reasons:         []string{"ImagePullBackOff", "ErrImagePull", "ImagePullError"},
				MinSeverity:     schema.SeverityWarning,
			},
			{
				Kind:            "Pod",
				Name:            "recommendation-etl",
				AlsoAcceptKinds: []string{"Deployment", "ReplicaSet"},
				Reasons: []string{
					"Unschedulable", "FailedScheduling", "InsufficientCPU",
					"InsufficientResources", "Pending",
				},
				MinSeverity: schema.SeverityWarning,
			},
			{
				Kind:            "Job",
				Name:            "inventory-sync",
				AlsoAcceptKinds: []string{"Pod", "CronJob"},
				Reasons: []string{
					"BackoffLimitExceeded", "JobFailed", "Failed", "DeadlineExceeded", "Error",
				},
			},
		},
	}
}

// cascade is a crash loop and the outage it causes.
//
// The Service selector is correct and the Deployment exists; the pods are
// simply never ready, so the Service has endpoints and none of them serve.
// Both findings are true and an agent should report both, but they are not
// worth the same: "the session store is unreachable" is what an operator sees
// and "the container exits 1 on boot" is what they have to fix. Recall cannot
// express that — it scores symptom-only and cause-only identically at 0.5 —
// so the crash loop is marked Root and evals.RootCause grades it separately.
//
// The namespace is **not** named after the fault, unlike the original six.
// Those describe themselves (`fault-crashloop`), which is harmless when the
// health scan announces the same thing in its first line, but here the whole
// measurement is whether the agent traces the outage back to its cause. A
// namespace called `fault-crashloop-behind-a-service` would hand over the
// answer in the prompt, which is the thing this tier exists not to do.
func cascade() Fault {
	const ns = "fault-sessions"
	return Fault{
		Name:   ns,
		Prompt: checkNamespace(ns),
		Manifest: deployment(ns, "session-store", 2, BaseImage, []string{
			"sh", "-c", "echo 'binding :6379'; sleep 2; echo 'FATAL: cannot open append-only file' >&2; exit 1",
		}, "", "") + fmt.Sprintf(`---
apiVersion: v1
kind: Service
metadata:
  name: session-store
  namespace: %s
spec:
  selector:
    app: session-store
  ports:
  - port: 6379
    targetPort: 6379
`, ns),
		Settle: []Condition{
			// The slow one — see crashLoopSettle for why ~170s, and note that
			// Fault.Settled shares one deadline across every condition, so this
			// one spends most of the budget. It is first deliberately: the
			// endpoints condition below is already true by the time the crash
			// loop is observable, so ordering them this way costs nothing, while
			// the reverse would make a timeout point at the wrong condition.
			crashLoopSettle(ns),
			{
				// Not Empty, unlike fault-badselector: the selector matches, so
				// the EndpointSlice does have endpoints. They are just never
				// ready, which is a different observation and a different fix.
				Describe: "the service endpoints to be not ready",
				Args: []string{"get", "endpointslices", "-n", ns,
					"-l", "kubernetes.io/service-name=session-store",
					"-o", "jsonpath={.items[*].endpoints[*].conditions.ready}"},
				Contains: "false",
			},
		},
		WantSeverity: schema.OverallCritical,
		Want: []Want{
			{
				Kind:            "Pod",
				Name:            "session-store",
				AlsoAcceptKinds: []string{"Deployment", "ReplicaSet"},
				Reasons:         []string{"CrashLoopBackOff", "CrashLoop"},
				MinSeverity:     schema.SeverityWarning,
				Root:            true,
			},
			{
				Kind:            "Service",
				Name:            "session-store",
				AlsoAcceptKinds: []string{"Endpoints", "EndpointSlice"},
				Reasons: []string{
					"NoEndpoints", "NoReadyEndpoints", "EmptyEndpoints",
					"ServiceHasNoEndpoints", "NoHealthyBackends", "Unavailable",
				},
			},
		},
	}
}

// unboundVolume is a Pending pod whose obvious explanation is wrong.
//
// Pending plus FailedScheduling reads as resource pressure, and that is what
// fault-unschedulable actually is. Here the node has capacity to spare: the pod
// requests almost nothing and cannot start because its PersistentVolumeClaim
// names a StorageClass that does not exist, so the claim will never bind. An
// agent that pattern-matches Pending to "insufficient CPU" reports a fault the
// cluster does not have, in a namespace where the resource it should have named
// is one line of `k8s_list_resources` output away.
//
// Only the claim is a Want, and the pod is not.
//
// That is a deliberate asymmetry and the reason the fixture discriminates.
// Every agent will notice the Pending pod — it is the first thing any health
// scan says — so grading it would hand out half the recall for the observation
// that costs nothing. The claim is the answer, so the claim is the score.
//
// The pod's reasons are not gradeable either way: "FailedScheduling" is the
// literal token kubectl attaches to an unbound-PVC pod as well as to a genuinely
// unschedulable one, so it can be neither required nor penalized without
// punishing an agent for the vocabulary the cluster handed it. It is in
// evals.genericReasons for that reason, and this fixture is what put it there.
//
// No Want here is marked Root, so root_cause skips this fixture. With one Want
// it would be a second copy of recall — the claim is already the only thing
// graded, which is the same discrimination by a different route. The cause and
// symptom this fixture does contain are separated by *excluding* the symptom;
// cascade separates them by grading both, and that is what Root is for.
func unboundVolume() Fault {
	const ns = "fault-ledger"
	// A storage class that does not exist, rather than no storageClassName at
	// all: kind ships a default class, so an omitted name binds immediately and
	// the fixture would inject nothing.
	return Fault{
		Name:   ns,
		Prompt: checkNamespace(ns),
		Manifest: fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ledger-data
  namespace: %s
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: fast-ssd-encrypted
  resources:
    requests:
      storage: 1Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ledger-writer
  namespace: %s
spec:
  replicas: 1
  selector:
    matchLabels: {app: ledger-writer}
  template:
    metadata:
      labels: {app: ledger-writer}
    spec:
      containers:
      - name: app
        image: %s
        imagePullPolicy: IfNotPresent
        command: ["sh", "-c", "sleep 3600"]
        resources:
          requests: {cpu: 10m, memory: 16Mi}
        volumeMounts:
        - name: data
          mountPath: /var/lib/ledger
      volumes:
      - name: data
        persistentVolumeClaim:
          claimName: ledger-data
`, ns, ns, BaseImage),
		Settle: []Condition{
			{
				Describe: "the claim to stay unbound",
				Args: []string{"get", "pvc", "ledger-data", "-n", ns, "-o",
					"jsonpath={.status.phase}"},
				Contains: "Pending",
			},
			{
				Describe: "the pod to be Pending on it",
				Args:     []string{"get", "pods", "-n", ns, "-o", "jsonpath={.items[*].status.phase}"},
				Contains: "Pending",
			},
		},
		WantSeverity: schema.OverallCritical,
		Want: []Want{{
			Kind:            "PersistentVolumeClaim",
			Name:            "ledger-data",
			AlsoAcceptKinds: []string{"PVC", "PersistentVolume", "StorageClass"},
			Reasons: []string{
				"Unbound", "PendingBinding", "VolumeBindingFailed", "ProvisioningFailed",
				"StorageClassNotFound", "NoStorageClass", "MissingStorageClass",
				"UnboundImmediatePersistentVolumeClaims", "Pending",
			},
			MinSeverity: schema.SeverityWarning,
		}},
	}
}

// silentFailure is a workload that is healthy in every field and doing none of
// its job.
//
// The container starts, cannot reach the database it exists to write to, logs
// the failure, and retries forever. It never exits, so the kubelet never
// restarts it: the pods are `1/1 Running` with zero restarts, the Deployment is
// `2/2` available, the event stream holds nothing but Scheduled/Pulled/Created/
// Started, and there is no Service, so there is no endpoint story either. Every
// broad check this agent has returns the namespace clean, correctly. The only
// evidence that anything is wrong is what the container is writing.
//
// # Why the suite needed this and could not get it from the fixtures it had
//
// Every other fault here is visible in object status, which is not a property
// anyone chose — it is what "inject a fault" tends to produce. The consequence
// showed up in the delegation histogram: log-analyzer was invoked on 0, 1, 0,
// 0, 0, 1 fixtures across six tier-2 runs, and the one fixture that looks like
// its territory, fault-crashloop, writes `FATAL: connection refused` to stderr
// and is still diagnosed off `state.waiting.reason` — because pod status is
// enough, and reading the log would only confirm what the status already said.
// A specialist whose evidence is always available somewhere cheaper is a
// specialist the suite cannot distinguish from an unused one.
//
// So this fixture is not "another fault, in logs". It is the first one where
// no broad scan and no status field can produce a correct answer, which makes
// it the first measurement of whether the agent goes further.
//
// # What the first run actually showed, which is not what this claimed
//
// The claim here was "the read path has to reach k8s_triage_logs". Both halves
// of that were wrong and the 2026-08-15 run showed it. k8s_triage_logs is not
// the only tool carrying log evidence — k8s_triage_workload bundles a distilled
// `logs` section alongside spec, delta, edges and radius, and is advertised as
// the first call of every incident, so the ordinary trajectory reaches log
// content without ever naming a log tool. And the fixture as first written did
// not require log evidence at all, because the diagnosis was in the container
// command; see the Manifest note.
//
// What survives is the property the suite was actually short of, and it is
// verified rather than argued: the fault is invisible to every broad scan, to
// object status, to the event stream and to the workload graph. Whether the
// agent gets there by the log section of a bundle, by a dedicated log tool, or
// by noticing that a single-container pod polls a port nothing in it serves,
// all three are diagnosis rather than pattern-matching a status field. Only the
// first version's fourth route — reading the failure out of the manifest — was
// not, and it is closed.
//
// # A zero here would still mean something specific
//
// fault-badselector is the precedent: it scored zero for two baselines, and
// what the zero identified was a missing enumeration primitive rather than a
// bad model. The analogous gap here would be an orchestrator that treats "the
// broad scans came back clean" as the end of an assessment. That is a
// defensible reading of a clean namespace — fault-none rewards exactly it —
// and the two fixtures together are the discrimination: fault-none punishes an
// agent that invents a fault in a clean namespace, and this one punishes an
// agent that stops before it can tell the two namespaces apart.
//
// # Reason tokens, and why the accepted set is narrower than it looks
//
// Kubernetes writes no reason for this, because Kubernetes does not know. The
// agent has to coin one, and its recorded coining style is compound and
// specific — `DataDependencyMissing`, `NoServiceDefined`, `RolloutIncomplete`
// are all real tokens from tier-2 runs — so the accepted set is a handful of
// compounds and Want.MatchesReason's substring matching covers the spellings
// around them: "Unreachable" admits `DatabaseUnreachable` and
// `UpstreamUnreachable`, "DependencyMissing" admits `DataDependencyMissing`.
// That is the matcher being used the way it is meant to be, as one answer's
// several spellings; see evals.familyOf for the other direction, where the
// same matcher was wrong.
//
// What the set must *not* admit is a token the agent could write about this
// workload without ever reading a log. That is a sharper constraint than it
// first appears, and the first draft of this fixture failed it. The matcher is
// bidirectional, so a *short* token the agent writes matches a *longer* one
// listed here: with `DependencyUnavailable` in the list, a bare "Unavailable"
// is accepted — and `Available=False` is a real Deployment condition, so an
// agent that never looked at the log could assert it, be wrong, and score
// recall 1.000. `ApplicationErrors` let in a bare "Error" the same way, which
// is a token models write constantly and which evals.genericReasons exists
// because of.
//
// So every "…Unavailable" and every "…Errors" token is out, and the concepts
// they covered survive as "Unreachable" and "DependencyFailure". The rule that
// picked the survivors is `failureFamilies`' own, one layer over: a member must
// assert *this* cause. "ErrorsInLogs" names the evidence channel rather than
// the failure, and it is exactly the token an agent could reach for after
// noticing nothing at all.
//
// TestSilentFailureRejectsStatusOnlyReasons pins both halves against the
// vocabulary recorded runs actually produced, including the handful of bare
// substrings that are still admitted and are judged harmless. Do not widen the
// list without re-running it.
func silentFailure() Fault {
	const ns = "fault-invoicing"
	// Named for the domain, not the fault — cascade's rule, and it binds
	// harder here than anywhere else in the set. Every broad check reports
	// this namespace clean, so the namespace name is very nearly the only
	// signal in the prompt, and a name like `fault-badlogs` would be the
	// entire diagnosis.
	return Fault{
		Name:   ns,
		Prompt: checkNamespace(ns),
		// The container must not narrate its own failure, and the first version
		// of this fixture did. It echoed `ERROR: dial billing-db:5432: connect:
		// connection refused` on a timer, which put the entire diagnosis into
		// `spec.template.spec.containers[0].command` — and both k8s_resource_spec
		// and k8s_triage_workload's spec section hand that string straight back.
		// The first live run solved the fixture in exactly that way: it read the
		// workload bundle, learned the hostname from the command, ran
		// k8s_net_probe against it, and reported correctly having never called a
		// log tool at all. Score 1.00, measuring nothing the suite was short of.
		//
		// So the failure is produced rather than described. wget writes `wget:
		// can't connect to remote host (127.0.0.1): Connection refused` on its
		// own stderr; the command says only what a healthy manifest would say,
		// which is that this workload polls a database on a local port.
		//
		// That also makes the fixture a better story than the one it replaces.
		// An app waiting on a database proxy sidecar that is not in the pod is a
		// real and common production bug — somebody dropped the second container
		// from the Deployment — and it is invisible in exactly the way this tier
		// could not previously pose: one container, Running, Ready, restarts 0.
		//
		// -q still writes the error; it only suppresses progress. || true keeps
		// the loop alive under wget's nonzero exit without `set -e` being
		// involved either way.
		// The attempt counter is not a second leak and the distinction is the
		// one the rewrite turns on: it says the app is retrying, which is what a
		// healthy manifest for a reconciler says too, and it does not say the
		// retries fail. The first run without it read the errors as a startup
		// blip — "6 occurrences ... immediately followed by
		// invoice-reconciler starting", filed info as TransientStartupError —
		// and a numbered attempt in the fortieth iteration is the cheapest
		// available refutation of that reading. Two-second interval for the same
		// reason: density is what makes a repeating failure look repeating.
		Manifest: deployment(ns, "invoice-reconciler", 2, BaseImage, []string{
			"sh", "-c",
			"echo 'invoice-reconciler starting; reconcile interval 2s'; " +
				"i=0; while true; do i=$((i+1)); echo \"reconcile attempt $i\"; " +
				"wget -q -T 3 -O /dev/null http://127.0.0.1:5432/healthz || true; " +
				"sleep 2; done",
		}, "64Mi", "128Mi") + fmt.Sprintf(`---
apiVersion: v1
kind: Service
metadata:
  name: invoice-reconciler
  namespace: %s
spec:
  selector:
    app: invoice-reconciler
  ports:
  - port: 9102
    targetPort: 9102
`, ns),
		Settle: []Condition{
			{
				// Load-bearing, not a formality. The fixture's whole claim is
				// that status is clean at the moment the agent looks, so the
				// green state has to be waited for exactly as the broken states
				// are elsewhere. Settling only on the log would allow a run
				// where the agent sees a half-rolled-out Deployment and reports
				// that instead — a true finding, about the wrong thing.
				Describe: "the deployment to become available",
				Args: []string{"get", "deployment", "invoice-reconciler", "-n", ns, "-o",
					"jsonpath={.status.availableReplicas}"},
				Contains: "2",
			},
			{
				// The Service closes a hole rather than adding realism, though
				// a reconciler exposing a metrics port is realistic enough.
				// Without it this namespace holds a Deployment and no Service,
				// which tier 3 established is a finding an agent legitimately
				// makes — `prod-checkout` was exactly that, and correctly
				// critical. An agent that stopped there would be right, would
				// have found the wrong thing, and if it spelled the observation
				// `NoEndpoints` or `ServiceHasNoEndpoints` would additionally be
				// charged with invention: those are no-endpoints family tokens
				// and this fixture injects no family at all, so `injected` is
				// empty and every family claim in the namespace is bogus. A
				// Service whose selector matches and whose endpoints are ready
				// removes the observation and adds one more green signal.
				Describe: "the service to have ready endpoints",
				Args: []string{"get", "endpointslices", "-n", ns,
					"-l", "kubernetes.io/service-name=invoice-reconciler",
					"-o", "jsonpath={.items[*].endpoints[*].conditions.ready}"},
				Contains: "true",
			},
			{
				// The only condition in the suite that reads a log, because
				// this is the only fixture whose fault is in one. The first
				// error lands ~1s after the container starts.
				// --since rather than --tail, and that is the load-bearing part.
				// The condition has to mean "the failure is ongoing", not "the
				// failure has happened once": the first scored run saw a
				// ten-second error window on an eighty-second-old pod and
				// concluded, not unreasonably, that it was a startup error that
				// had resolved. Requiring the error inside the last 25 seconds
				// means the pod is already past the age where that reading is
				// available, which is the settle rule this file states
				// elsewhere — a settle condition means "the agent can now
				// observe the fault", and a fault the agent will misread as
				// transient is not yet observable as what it is.
				Describe: "the dependency failure to still be happening",
				Args:     []string{"logs", "-n", ns, "-l", "app=invoice-reconciler", "--since=25s"},
				// wget's own wording, matched case-sensitively on the half that
				// is wget's rather than the host's, so a change to the address
				// does not silently stop this condition from meaning anything.
				Contains: "Connection refused",
			},
		},
		// Critical rather than warning, and the discriminator is impact rather
		// than symptom, which is what the orchestrator rubric asks for. Nothing
		// here is crashing and nothing is restarting — the symptom-level reading
		// is "two healthy pods" — but the workload performs zero percent of its
		// function and has since it started. Contrast fault-badselector, which
		// is warning: there the workload is genuinely fine and one label fixes
		// the route to it.
		WantSeverity: schema.OverallCritical,
		Want: []Want{{
			Kind:            "Pod",
			Name:            "invoice-reconciler",
			AlsoAcceptKinds: []string{"Deployment", "ReplicaSet", "Container"},
			// Five compounds, each asserting the cause rather than the
			// evidence, and each chosen so that the shorter tokens the
			// bidirectional matcher lets in with it are still harmless. See
			// the doc comment for what was removed and why.
			Reasons: []string{
				"ConnectionRefused", "ConnectionFailure", "Unreachable",
				"DependencyFailure", "DependencyMissing",
			},
			MinSeverity: schema.SeverityWarning,
		}},
	}
}

// failingJob renders a Job that exhausts its backoff limit.
func failingJob(ns, name string) string {
	return fmt.Sprintf(`apiVersion: batch/v1
kind: Job
metadata:
  name: %s
  namespace: %s
spec:
  backoffLimit: 1
  template:
    metadata:
      labels: {app: %s}
    spec:
      restartPolicy: Never
      containers:
      - name: job
        image: %s
        imagePullPolicy: IfNotPresent
        command: ["sh", "-c", "echo 'starting'; sleep 1; echo 'ERROR: upstream feed unavailable' >&2; exit 2"]
`, name, ns, name, BaseImage)
}

// deployment renders a Deployment. requests/limits are omitted when empty so
// the healthy fixtures can carry them and the broken ones need not.
func deployment(ns, name string, replicas int, image string, command []string, memRequest, memLimit string) string {
	var cmd string
	if len(command) > 0 {
		cmd = "\n        command: " + jsonList(command)
	}
	var resources string
	if memRequest != "" || memLimit != "" {
		resources = "\n        resources:"
		if memRequest != "" {
			resources += fmt.Sprintf("\n          requests: {memory: %s, cpu: 10m}", memRequest)
		}
		if memLimit != "" {
			resources += fmt.Sprintf("\n          limits: {memory: %s, cpu: 200m}", memLimit)
		}
	}
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  replicas: %d
  selector:
    matchLabels: {app: %s}
  template:
    metadata:
      labels: {app: %s}
    spec:
      containers:
      - name: app
        image: %s
        imagePullPolicy: IfNotPresent%s%s
`, name, ns, replicas, name, name, image, cmd, resources)
}

func deploymentWithCPURequest(ns, name string, replicas int, image, cpu string) string {
	return fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  namespace: %s
spec:
  replicas: %d
  selector:
    matchLabels: {app: %s}
  template:
    metadata:
      labels: {app: %s}
    spec:
      containers:
      - name: app
        image: %s
        imagePullPolicy: IfNotPresent
        command: ["sh", "-c", "sleep 3600"]
        resources:
          requests: {cpu: "%s"}
`, name, ns, replicas, name, name, image, cpu)
}

// jsonList renders a string slice as a YAML flow sequence. YAML flow syntax is
// JSON-compatible, so quoting each element is enough for the shell fragments
// the fixtures use.
func jsonList(xs []string) string {
	out := "["
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%q", x)
	}
	return out + "]"
}
