# AGENTS.md — k8s-sre-agent

Instructions for AI coding agents working in this repo, and the design record
they are drawn from. This is the committed, canonical file; `CLAUDE.md` is
gitignored and holds personal session context only, as in `k8s-lookout`.

A Go port of LangChain's Python `sre-agent` — an autonomous Kubernetes SRE
agent that monitors cluster health, diagnoses issues, and applies fixes behind
human approval.

The upstream project lives at `../langchain-samples/sre-agent` and is the
reference implementation. Read it before changing behaviour here; most design
decisions in this repo are either a faithful port of one of theirs or a
deliberate, documented divergence.

## Status

Early. What exists:

- `internal/schema` — the `Finding` / `HealthReport` structured-output
  contract, wire-identical to upstream's `schemas.py`.
- `internal/monitor` — stable finding fingerprints, verified byte-identical
  to the Python implementation across 994 differential vectors.
- `internal/evals` — the tier-1 eval harness with repaired evaluators.
- `internal/llm` — the two Vertex model tiers (`claude-sonnet-5` main,
  `claude-haiku-4-5@20251001` subagent).
- `internal/lookout` — the MCP client for the live read path, plus the offline
  toolset tier 1 runs against.
- `internal/kuberead` — `k8s_list_resources`, the namespace enumeration tool
  lookout does not have. One `kubectl get` across 18 kinds, rendered as one
  line per object keyed by the `<Kind>/<namespace>/<name>` target every other
  tool wants.
- `internal/kubectl` — the pinned kubectl subprocess `kuberead` and `kubewrite`
  share, plus the kubeconfig context check that makes the pin real. What
  `internal/lookout` shares is the *check* (`VerifyContext`) and nothing else:
  the lookout binary is client-go throughout and never shells to kubectl, so
  kubectl is in the image for our two packages alone. That matters for a
  distroless static deployment, where every external binary has to be shipped
  deliberately.
- `internal/sre` — the orchestrator plus the eight read-only specialists and
  `change-executor`, all built from `specs/*.tmpl` via mast. Also `stall.go`,
  the payload mast's stall guard files when a specialist stops without
  reporting — a gap in the orchestrator's report instead of a dead run — and
  `identity.go`,
  the submission-time checks that stop a finding whose identity fields cannot
  be fingerprinted.
- `internal/bounded` — the health check the scheduler runs every cycle: two
  `lookout` scans at zero model tokens, then one forced-tool Haiku call
  emitting the same `schema.HealthReport` the agent submits. Scored against the
  same fixtures via `sre-eval-live -bounded`; see the bounded-pass baseline.
- `internal/kubewrite` — the fourteen write tools, each one gated on a human
  approval that shows the exact `kubectl` command before it runs.
- `internal/approval` — the human end of that gate: read the interrupts out of
  a turn, put them to an `Approver`, resume the run.
- `internal/kindcluster` — the throwaway cluster tier 2 runs on, with the
  isolation guards that keep fault injection off real clusters.
- `internal/faults` — the eleven tier-2 fixtures: a manifest, a settle
  condition, and the findings a correct agent would report.
- `cmd/sre-eval` — runs the 31 tier-1 scenarios end to end.
- `cmd/sre-eval-live` — runs the tier-2 fixtures against a real broken cluster.
- `cmd/sre-agent` — the first entry point that is not an eval harness: one
  read-only assessment of named namespaces on a cluster somebody else built.
  No ground truth, so nothing is scored; what it measures is what a fixture
  cannot (see the tier-3 notes).

All three run against Vertex, read-only: they build the agent with no
`Config.Writes`, so the write specialist is not wired up at all and the
published numbers are numbers for the read path. Not built yet: the scheduler,
Slack, and any entry point that actually grants writes — `kubewrite.Tools` and
`approval.Session` exist and nothing calls them together outside tests.

**The finding diff came off that list without us building it.** k8s-lookout
shipped it (issue #212) as `k8s_findings_diff` / `k8s_findings_ack`, keyed on a
`SubjectKey(cluster, namespace, kindOfObject, NormalizeName(name),
canonicalReason)` alongside the frozen class fingerprint — instance identity for
the diff, class identity for fleet rollup. So the remaining work here is not a
differ; it is feeding one a stable object choice, which is the half tier 3
showed we do not have (see "The result that most constrains the finding diff":
the same fault fingerprinted as a Deployment on one run and its own pod on the
next). `NormalizeName` already absorbs the pod-suffix half of that collision;
what it cannot absorb is the *kind* segment or a `canonicalReason` borrowed from
the wrong layer, which is task #11 again from a third direction.

### Tier-1 baseline (2026-08-14, Sonnet 5, 31/31 scored)

The first run on the 24-tool lookout surface and the first with
`internal/sre/identity.go` installed. It is **a new baseline rather than a
comparison**, exactly as the previous section's closing note said it would have
to be: the offline toolset went from 22 tools to 24 with no `alias.go` entry for
either addition, so `tool_coverage` and its floors both moved underneath the
number.

```
resource_grounding   1.000     # 24 scored, 7 skipped (no resource named in ground truth)
severity_accuracy    0.387
severity_calibration 0.796     # 12 exact, 19 too hot, 0 too cold
tool_coverage        0.930     # floors 0.414 / 0.702
reported             31/31 runs submitted a health report
delegation           24/31 examples, all 8 specialists  (min 1: job-inspector)
delegation errors    0
stalled specialists  3/31 runs  (config-auditor, security-auditor and
                     reliability-auditor, one example each)
cost                 $4.06 for the suite, $0.1309 per example
```

Read them together or not at all, for the reasons the 2026-08-13 section sets
out at length — that argument is not repeated here, and each guard line below
means what it means there.

**The identity checks never fired, so nothing in this run is attributable to
them.** The transcript contains zero occurrences of `violates the contract`,
zero of `ProtestMarker`, and zero of `one finding per object`. That is what v1's
scope predicts rather than a surprise: `resourceLayers` maps only PVC, PV,
Service, Ingress, ConfigMap and Secret, and tier-1 findings are almost entirely
Pod and Deployment — the two kinds it deliberately leaves out. So #11 is
installed and **unmeasured by this tier**, which is what tier 2 is for.

**`severity_accuracy` 0.387 is below the previous four-run band, and the
direction is unbroken.** Five runs on this wiring now read 0.452 / 0.548 /
0.484 / 0.548 / **0.387**, with calibration 0.763 / 0.806 / 0.806 / 0.839 /
**0.796**. Every one of this run's 19 misses is exactly one level and every one
is too hot; zero too cold, which is the fifth consecutive tier-1 run to say so.
Note how differently the two statistics moved: accuracy fell 0.161 from the last
run while calibration fell 0.043. That gap is the whole reason both are
reported — an agent whose errors are all one notch hot loses exact-match credit
far faster than it loses calibration, so reading accuracy alone would call this
a large regression in judgement when what it measures is a small shift in where
one notch lands.

**ex-19 is one level this time rather than two**, reported `info` against a
ground truth of `ok`. It is the same behaviour the previous baseline recorded —
every lookout call returned the offline marker and the agent declined to certify
a cluster it could not see — and the label it chose for that refusal came down a
notch on its own. Nothing was changed to cause it, and the example is still
ungradeable in the sense examples 4 and 5 are.

**Delegation is down and the histogram is thinner:** 24/31 against 28/31, still
all eight specialists, but `job-inspector` on exactly one example
(`pod-inspector` 11, `config-auditor` 9, `reliability-auditor` 5,
`scaling-analyzer` 5, `log-analyzer` 3, `performance-analyzer` 3,
`security-auditor` 3, `job-inspector` 1). "All 8 used" is one example away from
being false, which is precisely why the histogram is printed and not just the
count.

**Stalls are up, 3/31 against 2/31, and for the first time the text says
why.** ex-00 `config-auditor`, ex-22 `security-auditor`, ex-26
`reliability-auditor` — three different specialists, three different examples,
and all three last-words are the same sentence in different words: the
specialist noticed the toolset was in offline mode and began narrating the
investigation it *would* have run. That is an artefact of the offline tier
rather than a read-path gap — it is the failure `lookout.OfflineMessage` exists
to prevent, arriving anyway — and it is worth holding apart from tier 2's
stalls, which have been zero on every recent run against a real cluster. The
guard did its job in all three cases: each run still reported.

**The cost attribution now has 31 examples behind it and it says what the
three-example sample said.** The orchestrator is **88.9% of input tokens** and
$3.39 of the $4.06; the eight specialists together are $0.67 across 92
requests, against the orchestrator's 175. So "delegate for breadth" is still a
cost argument as well as an accuracy one, and it is a stronger one at 24/31
fan-out than it was at three examples: a run's bill is the orchestrator's
context, not its subagents.

### Tier-1 baseline (2026-08-13, Sonnet 5, 31/31 scored)

Superseded by the run above; kept because the argument for each guard line lives
here and the four-run severity band it establishes is what the new run is read
against.

Measured with `k8s_list_resources` in the roster and the stall guard installed,
on top of the three fixes the previous baseline established (delegation
executing, the report arriving through `submit_health_report`, specialist
transfer disabled).

```
resource_grounding   1.000     # 24 scored, 7 skipped (no resource named in ground truth)
severity_accuracy    0.548
severity_calibration 0.839     # 17 exact, 14 too hot, 0 too cold
tool_coverage        0.913     # floors 0.414 / 0.702
reported             31/31 runs submitted a health report
delegation           28/31 examples, all 8 specialists  (min 2: log-analyzer,
                     job-inspector, performance-analyzer)
delegation errors    0
stalled specialists  2/31 runs  (config-auditor twice)
```

Read these together or not at all. `tool_coverage` is meaningful only against
its floors; `severity_accuracy` only against the fact that the misses are
one-directional; and both only against `reported` *and* `stalled specialists`,
since a run that ends without a report scores zero on severity and folds into
the mean looking exactly like a wrong answer, and a run with a stalled
specialist folds in looking exactly like a complete one.

**`reported` is 31/31 for the first time**, and that is the one number here that
is unambiguously a result rather than noise. It has been 29 and 30 on every
previous baseline, and the fix is the stall guard (then `internal/sre/stall.go`,
now `mastagent.FinishOnStall`): it fired
twice, on ex-03 and ex-23, and both of those runs went on to score
`severity_accuracy` 1.00. Before the guard each would have been a total loss.
(Which specialist's words were lost is not recorded for this run — the harness
kept only the name until immediately after it, and now keeps the text; see
`Run.Stalls`.)

**The severity numbers are inside the noise band and should not be read as
movement.** Four runs on this wiring have now scored `severity_accuracy`
0.452 / 0.548 / 0.484 / 0.548 and `severity_calibration` 0.763 / 0.806 / 0.806 /
0.839. Every one had **zero** too-cold misses. The spread is ±0.05 on 31
examples; the direction is invariant, and the direction is the part worth acting
on.

**`tool_coverage` 0.913 is not comparable to the 0.881 that preceded it.** The
agent gained a tool between the two runs, so both the score and its floor moved:
against the single-call floor of 0.414 it looks like an improvement, and against
the blind fan-out floor of 0.702 — the three broad discovery calls an agent can
make against any namespace before reading a single result — it is a smaller gap
than the old number's. `cmd/sre-eval` prints both floors under every run for
exactly this reason.

And it has moved again since, for the same reason: the lookout upgrade of
2026-08-14 took the offline surface from 22 tools to 24 (`k8s_findings_diff`,
`k8s_findings_ack`). Neither has an `alias.go` entry, so neither can earn
coverage credit — the only way they can move the number is by consuming a call
that would otherwise have gone to a check that scores. That is a small effect
and an unmeasured one, so **the next tier-1 run is a new baseline rather than a
comparison**, and 0.913 should not be quoted next to it.

**One miss is now two levels rather than one**, which breaks an invariant every
previous baseline held. It is ex-19, and it is the harness rather than the
agent: "run a routine cluster health audit across all namespaces" against the
*offline* toolset, where `k8s_cluster_health` and all six specialists the
orchestrator fanned out to returned the offline marker. The agent reported
`warning` — "no telemetry, so no statement can be made about cluster health this
cycle" — against a ground truth of `ok`. Refusing to certify a cluster it could
not see is the behaviour the whole design argues for; grading it as
over-escalation is a third ungradeable label, alongside examples 4 and 5 below.

The last four lines are not scores — they are the guards that keep the other
four honest, and each one exists because the thing it counts once went wrong
invisibly.

A specialist roster that is never invoked scores identically to one that is, so
without the delegation line the eight specs could quietly become dead config and
the orchestrator's numbers would be credited to a fan-out that never happened.
That is not hypothetical: it is exactly what two published baselines did. The
histogram matters as much as the count — `log-analyzer` has gone 0, 2, 4, 2
across four runs, so a roster can be nominally "all 8 used" and still have a
specialist hanging by one example. `delegation errors` is there because a count
of calls says nothing about outcomes; see "A delegation count counts attempts,
not outcomes".

`reported` exists because of a failure mode delegation introduced. A run ended
with no report at all when a specialist ended its turn by asking a question
instead of reporting: ADK ends the caller's turn when a delegation returns
nothing, so one stalled specialist cost the entire run. Prompting took this as
far as it defensibly goes — tier 2 showed why it could not go further, since a
prompt cannot forbid the only remaining action.

The stall guard fixed that structurally, and in doing so moved the
failure somewhere quieter rather than removing it. The orchestrator now reports
with one area unchecked, which scores like a complete answer minus whatever that
area was worth. So `stalled specialists` is the line that inherits the job
`reported` used to do alone, and from here on a zero in *both* is the only
combination that means what `reported: 31/31` used to mean by itself. Each entry
carries the specialist's own last words, because that sentence usually names the
read-path gap that caused the stall — which is how the enumeration tool was
found.

### `ProtestMarker` fired, for the first time (2026-08-15, switchboard smoke)

Recorded because two baselines in a row said the identity checks had never fired
against a live model, and the run that changed that was not an eval — it was the
first end-to-end test of the switchboard notifier, against a *healthy* kind
cluster.

```
UNDER PROTEST: finding 1 ("Control-plane metrics unavailable") is missing
resource_name — name the object the finding is about ...
```

It was the **bounded pass**, not the agent, and the check was
`unidentifiedFindings` rather than the new layer rule. `lookout health` answers
`control-plane: unavailable — requires cloud provider metrics; no cloud provider
configured` on a cluster with no cloud provider, and that is a *scorecard
category*, not an object. The model faithfully filed it as a finding, and a
category has no kind and no name to give.

Same root cause as `go-steer/k8s-lookout#247` arriving in a second place. The
scheduler strips `kind=health.category` lines before diffing them, because the
differ keys on an object; nothing stripped them from the *analysis prompt*,
where they are wanted — the explicit healthy/degraded/unavailable per category
is what makes a clean cluster legible. So the fix is in the prompt rather than
the filter: the scorecard is now labelled as context, with "a category is not an
object", "do not file a finding about a category", and what `unavailable` means.
Re-run: `ok` instead of `info`, no protest, and the model mentions the un-run
check in its summary rather than reporting it as a fault — which is the
behaviour the explicit-healthy contract is for.

**It came back the next day, so the prompt fix is a reduction rather than a
cure.** On the 2026-08-16 demo run the third cycle protested
`finding 1 ("Control plane health check unavailable") is missing resource_name`
— same category, same shape, a different wording of the same title, on a
bounded pass whose two previous cycles that morning did not protest. So the
occurrence is intermittent and the prompt only lowered its rate: nothing in
`internal/bounded` structurally prevents the model filing a scorecard category
as a finding, and the protest bound is what turns it into a labelled report
rather than a lost cycle. The next increment, if one is wanted, is the same
polarity as the filter — drop a finding whose `resource_name` is empty *and*
whose title names a scorecard category, at the bounded pass's own submission —
rather than a fourth paragraph.

Two things worth taking from it beyond the fix. **The counter earned itself the
same day it was built**: `Run.Protests` was added that morning precisely because
widening `crossLayerReasons` made a rejection loop likelier and the bound was
invisible — and the first real occurrence was something else entirely, on a code
path nobody was watching. And **it surfaced on a healthy cluster**, the cheapest
possible test, which is an argument for smoking new wiring against a namespace
with nothing wrong in it rather than one full of faults.

### Tier 3 for the scheduler: 30 minutes on `simian-test` (2026-08-15)

`cmd/sre-monitor` against the real GKE cluster, 3-minute interval, floor 24h over
`online-boutique,prod-checkout`, cap 2, heartbeat every 4 quiet cycles. Ten
cycles, three digests, then a clean SIGTERM shutdown.

```
17:12:38  floor      5 transitions (all new)      2 escalations
17:18:37  heartbeat  5 transitions (all ongoing)  0 escalations
17:30:34  heartbeat  5 transitions (all ongoing)  0 escalations
17:39:24  stopped
cost ≈ $0.55 for the 30 minutes, almost all of it the two cold-start escalations
```

**The result that matters is the middle column: five subjects, identical across
ten cycles.** Same kinds, same names, same reasons; `new` once and `ongoing`
every time after. That is fingerprint stability *measured on a real cluster*,
and it is the property the escalation trigger is built on. It is worth being
precise about what it does and does not say: the collector's findings are
stable, and this says nothing about the agent's, which are the ones task #16 is
about.

**Nothing re-escalated.** Nine consecutive cycles saw the same five subjects and
started zero agent runs, which is the entire point of putting a differ in front
of the agent. Both escalations happened on the cold cycle — `online-boutique`
from the trigger and `prod-checkout` from the floor, deduped correctly.

**The three cluster-scoped findings were recorded rather than escalated.** Real
`MutatingWebhookConfiguration` and `ValidatingWebhookConfiguration` objects with
`SlowWebhookRisk`, no namespace, so nothing to send a namespace-scoped agent to.
The fix that came out of the kind-cluster smoke test held against the real thing.

**A ticker note.** Cycle 1 took 3m14s against a 3m interval — the two escalations
— so it overran a tick and the next cycle started immediately on the buffered
one. Standard `time.Ticker` catch-up and harmless here, since the overrunning
cycle is by definition the one that just did the expensive work. Worth knowing
before setting an interval close to the escalation budget.

**And the cluster had changed since yesterday.**
`Pod/online-boutique/loadgenerator-…` came back `OOMKilled`; the tier-3 notes
from 2026-08-14 recorded that workload at 493Mi of a 512Mi limit, which the
agent rendered as 96.4% and filed as headroom. It has since crossed. The monitor
found it on its first cycle, which is the use case.

#### `gemma4-vllm` is not `Unschedulable`, and it is not not-`Unschedulable`

Checked directly during this run, because a claim that the collector's token was
"correct" and the agent's was "wrong" turned out to be sloppier than it sounded:

```
Deployment/gemma4-vllm   Available=False     reason=MinimumReplicasUnavailable
                         ReplicaFailure=True reason=FailedCreate
                         Progressing=False   reason=ProgressDeadlineExceeded
pods matching app=gemma4-vllm: none
```

No pod exists, so nothing carries `PodScheduled=False` and nothing in this
object's tree emits `Unschedulable` — Warden rejects the pod at admission and
the scheduler never sees it. But the *cause* is a nodeSelector no node can
satisfy, so "unschedulable" is a fair description of the situation and nobody
reading it is misled. It is wrong as a fingerprint field, not as prose, which is
exactly the split #11 exists for.

The distinction matters because the argument for keying the diff on the
collector does **not** rest on the agent's token being wrong. It rests on
*consistency*: the same fault must produce the same key every cycle, which fails
whether or not either token is defensible. The bounded pass writing
`RolloutIncomplete`, `ExcessiveRestarts` and `CrashLoopBackOff` for related
conditions inside one suite is the whole case. Do not restate it as a
correctness claim.

### Bounded-pass baseline (2026-08-15, Haiku 4.5, 11/11 scored)

The scheduled path, measured against the same eleven fixtures and the same four
evaluators as the agent, on the same day. This is what `schema.HealthReport`
being one contract with two producers buys: the comparison is code, not
argument. Run it with `go run ./cmd/sre-eval-live -bounded`.

```
                     bounded    agent     ratio
fault_recall         0.517      1.000
hallucinated_fault   1.000      1.000
fault_severity       0.848      0.909
root_cause           1.000      1.000
cost per fixture     $0.0046    $0.2374   51.6x cheaper
suite cost           $0.0507    $2.61
latency per fixture  1.1–6.6s   16s–2m44s
model calls          1          7 (mean)
```

**The falsifiable prediction held.** `fault-badselector` was named in
`internal/bounded`'s package comment, before the run, as the fixture this pass
should be expected to fail — a Service selecting nothing in front of a perfectly
healthy Deployment is the absence class in fixture form. It reported `ok`, no
findings: recall 0.00, severity 0.33. `fault-invoicing` failed the same way and
for the same reason one layer over — neither scan carries log content, so there
is nothing for the model to see. Those two are genuine blindness and they are
the argument for the escalation design rather than a defect in it.

**But most of the recall gap is not blindness, and that is the more useful
result.** Of the five below-ceiling recall cells, only two are "did not see it".
The other three are *seen and mislabelled*, and all three the same way:

```
fault-crashloop      Pod/payments-worker-…      ExcessiveRestarts     (want CrashLoopBackOff)
fault-unschedulable  Deployment/analytics-etl   RolloutIncomplete     (want Unschedulable…)
fault-storefront     Deployment/recommendation-etl  RolloutIncomplete (want Unschedulable…)
```

`RolloutIncomplete` is the bounded pass's `Unschedulable`: a true
controller-level token reached for in place of the pod's own failure mode, on a
finding that names the right object. It is the same defect `crossLayerReasons`
exists for, from the other producer, and the same "right and unusable" shape —
the report reads correctly and the fingerprint field does not identify the
failure. Note the pass has the vocabulary and used it elsewhere: it wrote
`CrashLoopBackOff` on `fault-sessions` and `OOMKilled` on `fault-oomkill` in the
same run.

**As a change detector — which is the job it actually has — it is far better
than 0.517 suggests.** The escalation trigger asks "did something change here",
not "what is it". On that reading:

```
10 faulty fixtures:  8 flagged non-ok, 2 missed (badselector, invoicing)
 1 healthy fixture:  correctly ok, no findings
hallucinated_fault:  1.000 — it invented nothing, on any fixture
```

That is the design thesis measured rather than asserted: noticing that something
changed is what a status snapshot is reliable at, diagnosing what it was is what
it is worst at, and the seam belongs between the two.

**It beats the agent on the healthy fixture.** `fault-none` severity 1.000
against the agent's 0.333, because the bounded pass does not editorialize — no
`MissingProbes`, no `NoPodDisruptionBudget`, no advisories lifting a healthy
namespace to `warning`. Read that carefully rather than as a win: the agent's
advisories are correct and their severity is arguable, which is the standing
note on that fixture. What it does say is that a cycle running this will not
page anybody about a missing PodDisruptionBudget.

**Cost, at the scale the design argues about.** 288 five-minute cycles a day at
$0.0046 is **$1.32/day** for the whole cluster, against roughly $683/day for the
agent on ten namespaces — and the agent does not fit a five-minute interval at
its 2m44s end, while this finishes in single-digit seconds. The 51.6x is per
fixture; per *cycle* the gap is wider still, because a multi-namespace bounded
scan is one `-A` call rather than one call per namespace.

**Two caveats on the numbers.** The bounded pass ran on Haiku and the agent on
Sonnet, so the cost ratio is a tier difference as well as a step-count
difference — that is the intended comparison (it is the configuration each would
actually run in) but it is not an apples-to-apples measure of the loop's cost
alone. And `root_cause` 1.000 is one scored fixture out of eleven in both
columns; it is not evidence of anything yet.

### Tier-2 baseline (2026-08-15, Sonnet 5, 11/11 scored)

First run on the eleven-fixture suite, first with exact-token `familyOf`, first
with `crossLayerReasons`' leading-noun signal, and first with `Run.Protests`.
The best figures the suite has produced — and the two most interesting things in
it are a check that did not fire and a fixture that scored for the wrong reason.

```
fault_recall         1.000   # 10 scored, 1 skipped (healthy fixture)
hallucinated_fault   1.000
fault_severity       0.909   # 0 too cold
root_cause           1.000   # 1 scored, 10 skipped
delegation           5/11 fixtures, 3 of 8 specialists
                     (job-inspector 2, reliability-auditor 2, config-auditor 1)
delegation errors    0
stalled specialists  none
under protest        none
cost                 $2.61 for the suite, $0.2374 per fixture
```

**`fault_recall` 1.000 is the first perfect recall, and the identity checks are
not why.** Zero occurrences of the layer rejection, zero of
`is not the name of a single object`, zero of `ProtestMarker` — third
consecutive run in which the checks never fired, and the first in which they
were *more* likely to, since `leadingSubject` shipped between this run and the
last. Everything below is the agent, not the gate.

**The two fixtures the checks were written for both fixed themselves.**
`fault-sessions` filed `Service/session-store NoReadyEndpoints` — an accepted
token — where the 21:30 run filed `PodsNotReady` and scored 0.50; recall and
`root_cause` are both 1.00. `fault-ledger` filed the 17:44 positive control's
exact three-finding shape (`Deployment RolloutIncomplete`, `Pod Unschedulable`,
`PVC PVCPending`) and scored 1.00. That shape now correlates perfectly with
scoring: every run of that fixture which filed three findings scored 1.00 and
every run which collapsed the pod into the PVC scored 0.00. The defect is
variance, and this run is more evidence for that than for the check.

**`fault-invoicing` scored 1.00 on its first run and the score is not worth
much.** The fixture leaked its diagnosis into its own pod spec; see the
paragraph in "What the ten-fixture suite still cannot measure" for the mechanism
and the fix. Two smaller things fell out of it worth keeping:

- **The reason list is why it scored at all, and only after the tightening.**
  The agent coined `UpstreamDependencyMissing`, which `DependencyMissing`
  accepts by substring. The *original* twelve-token list — the one with
  `DependencyUnavailable` and `ApplicationErrors` in it — matches that token
  nowhere, so it would have scored 0.00. The tightening was made for a
  correctness reason (excluding tokens assertable from status) and the token
  that saved it, `DependencyMissing`, was picked off the recorded coining census
  where the agent had written `DataDependencyMissing`. Census-informed rather
  than lucky, but not predicted either.
- **`ReadinessProbeMissing` was filed `critical`** on a healthy-probing
  workload, which is what carried `overall_severity` to the fixture's expected
  `critical`. The severity cell scored 1.00 partly for the wrong reason.

**`fault-badselector` severity is 0.67 for the seventh consecutive run**, still
the only cell that has never moved. `fault-none` is 0.33 again, three
warning-level advisories on a healthy Deployment — the alternation the run-1/run-2
note predicted, and still not worth "fixing".

**Delegation is 5/11 with three specialists and `log-analyzer` is not one of
them.** Its history is now 0, 1, 0, 0, 0, 1, 0 across seven runs, and this is
the run that was supposed to change that. It did not, exactly as the fixture's
own caveat predicted: `k8s_triage_logs` is in the orchestrator's allowlist, and
`k8s_triage_workload` carries a distilled `logs` section anyway, so an
orchestrator reaching log evidence never needs the specialist.

**Cost concentration is the highest yet: the orchestrator is 93.4% of input
tokens** and $2.49 of the $2.61, against 88.9% on the last tier-1 suite. Three
specialists across 26 requests came to $0.12.

### Tier-2 baseline (2026-08-14 21:30, Sonnet 5, 10/10 scored)

The first run with `internal/sre/identity.go` installed, and the best figures
the suite has produced. Green throughout: 10/10 injected, no `InjectError`, no
fixture lost to quota, no stalls.

```
fault_recall         0.944   # 9 scored, 1 skipped (healthy fixture)
hallucinated_fault   1.000
fault_severity       0.967   # 0 too cold
root_cause           1.000   # 1 scored, 9 skipped
delegation           1/10 fixtures, 1 of 8 specialists  (job-inspector)
delegation errors    0
stalled specialists  none
cost                 $1.90 for the suite, $0.19 per fixture
```

**The identity checks did not fire, and here that is the result rather than a
caveat.** Zero occurrences of all three markers, same as tier 1 — but this tier
contains the fixture they were written for, and `fault-ledger` scored **1.00**
by filing exactly the shape of the 17:44 positive control:

```
Deployment/ledger-writer            RolloutIncomplete
Pod/ledger-writer-77cb5bc896-vr69n  Unschedulable
PersistentVolumeClaim/ledger-data   PVCPending
```

Three findings, each object carrying its own failure mode, the pod keeping
`Unschedulable` and the claim getting `PVCPending`. There was nothing for
`crossLayerReasons` to reject. So this run is **not** evidence that the check
works — it is evidence that the defect is variance, now 1.00 on two of six runs
of this fixture, and that the check has still never been exercised against a
live model. **#11 v2 is therefore not unblocked by this run**: widening
`resourceLayers` to the workload controllers cannot be argued from a run in
which v1 never fired.

**`fault-sessions` is "right and unusable" in a new spelling, and it is the one
finding v1 should have caught and did not.** Recall 0.50 with
`missed=Service/session-store` — and yet the agent filed that Service:

```
Service/session-store   severity=critical   reason=PodsNotReady
```

Right object, right severity, true observation, and none of the six
Service-layer tokens the fixture accepts (`NoEndpoints`, `NoReadyEndpoints`,
`EmptyEndpoints`, `ServiceHasNoEndpoints`, `NoHealthyBackends`, `Unavailable`).
This is the PVC defect on a Service: the reason names the *pods'* condition on a
finding about the Service in front of them. `resourceLayers` maps `service`, so
the kind half of the check was ready and waiting — it passed because
`podsnotready` is absent from `reasonLayers` and the table fails open by design.

That is the fail-open trade-off costing exactly what it was designed to cost,
and it points the next increment somewhere other than where #11 v2 was aimed:
**more reason tokens, not more kinds.** But it does not simply extend the
existing table, and the reason is worth having before anyone tries. The agent
*coined* `PodsNotReady` — Kubernetes writes it nowhere — so no table of real
emitted tokens will ever contain it, and a known-token→legal-layers map cannot
catch a token it has never heard of. Catching this needs the opposite polarity,
a per-kind allowlist ("a Service may say these things"), which is closed-world
and would reject honest vocabulary the way `genericReasons` twice did.

**#30 is decided: no closed-world allowlist. The polarity stays open, and the
second signal is a different axis.** `fault-invoicing` is what settled it — its
only correct answer is a coined token on a Pod (`ConnectionRefused`,
`Unreachable`, `DependencyFailure`, none of which Kubernetes writes anywhere),
so a per-kind allowlist broad enough to be honest would not be an allowlist. The
dimension that *is* enumerable is the set of kinds, not the set of reasons, and
that is what `leadingSubject` uses: a coined reason that borrows across a layer
nearly always says whose state it borrowed, in its first word. `PodsNotReady`
opens by naming pods, so it is refused on a Service; a token that opens with no
kind noun is accepted against everything. Both routes into `borrowedLayer` fail
open and neither constrains vocabulary.

**The restriction to the *leading* word is the whole of the conservatism**, and
the fixtures are what show it is the right cut. `fault-badselector` accepts
`NoMatchingPods` on a Service and `cascade` accepts `NoReadyEndpoints`; both
name another layer, both are honest, because they describe the Service's own
relationship to those pods rather than the pods' condition. `PodsNotReady` is
the same words with the subject moved to the front. Three further restrictions
keep honest tokens out of the net: a reason that is *only* a kind noun is not a
borrow claim; a `<Kind>NotFound`/`Missing` suffix is a statement about a
reference this object holds, which is its own problem (`StorageClassNotFound` on
a PVC, `ServiceNotFound` on an Ingress, and the near-miss
`PodDisruptionBudgetMissing`); and a noun that names the object's *own* layer is
self-description (`ServiceHasNoEndpoints`, `PVCPending`).

`"rollout"` was in the noun table and was removed before it shipped. `Service:
RolloutIncomplete` is genuinely wrong and it would have been caught — but a
rollout is a concept, not a kind, and the single property that makes this table
safe is that it enumerates something finite and known. That is where the creep
would have started.

Four tests carry it, and the last two matter more than the rule.
`TestTheLeadingNounRuleAcceptsHonestVocabulary` is the corpus every
over-rejection in this repo would have failed. `TestNoFixtureAnswerIsRefusedAtSubmission`
runs every reason every tier-2 fixture accepts, against every kind it accepts,
through the gate — because a fixture grades a report that has to get *through*
submission first, so a rule that refuses a fixture's own right answer makes it
unscoreable while looking like a model failure. And
`TestTheRecordedVocabularySweep` takes the seventeen distinct reason tokens four
saved tier-2 runs produced and sweeps each against all six mapped kinds, naming
which must be refused and which must not — the sweep is stronger than replaying
the actual pairings, and it survived the transcripts ageing out of `/tmp`, which
the replay did not.

**What is still not caught, unchanged by any of this:** `Deployment/gemma4-vllm
reason=Unschedulable`, the more reproducible miss at six occurrences. Workload
controllers are still unmapped in `resourceLayers`, because "Deployment:
Unschedulable" is arguable as a summary of its pods' state and the PVC case is
not. This increment widened the *signal*, not the kinds.

Note also what recall does to this. Run 1's `fault-sessions` miss was the
Service going *unmentioned*; this one found it and named it wrong, and
`fault_recall` renders the two identically at 0.50. That is the same collapse
`root_cause` was added to fix one rung up.

**`fault-badselector`'s severity is still the only cell that has never moved:**
0.67, `critical` against `warning`, six consecutive runs.

**Delegation collapsed to its lowest ever while the scores went to their
highest** — 1/10 fixtures, one specialist, and the fastest run yet at 16s to 51s
per fixture. The orchestrator carried nine of ten fixtures alone and scored
better doing it. That is not an argument that the fan-out is worthless; it is
the sharpest available statement of what the section below already says about
this suite — every fixture is one namespace's problem, visible to an agent that
enumerates and then triages the object that stands out. #12 is the item that
changes it, and until something here needs breadth the delegation line will keep
measuring the fixtures rather than the agent.

### Tier-2 baseline (2026-08-14, Sonnet 5, 10/10 scored, two runs)

Superseded by the run above. Kept for the metric-bug account, the `root_cause`
first result, and the per-fixture argument the newer run is read against.

First baseline on the ten-fixture suite — the original seven plus
`fault-storefront`, `fault-sessions` and `fault-ledger` — and the first with the
`root_cause` evaluator.

```
                     run 2     run 1
fault_recall         0.889     0.833   # 9 scored, 1 skipped (healthy fixture)
hallucinated_fault   1.000     1.000   # see the metric-bug note below
fault_severity       0.900     0.967   # 0 too cold in either run
root_cause           1.000     1.000   # 1 scored, 9 skipped
delegation           5/10 then 4/10 fixtures, 5 then 3 of 8 specialists
delegation errors    0
stalled specialists  none
```

Both runs green: 10/10 injected, no `InjectError`, no fixture lost to quota, no
stalls. Read the two columns as a spread, not as a trend — once the metric bug
below is accounted for they differ on exactly two cells, and in opposite
directions: run 2 recovered `fault-sessions`' missing symptom and lost
`fault-none`'s severity.

**The two runs are not on identical eval code, and the difference is a metric
bug the new fixtures found.** Run 1 scored `hallucinated_fault` 0.900, charging
`fault-ledger` with inventing a resource shortage because its PVC finding
carried `reason: Unschedulable`. That is the reason string the scheduler itself
wrote on the pod, with the message "pod has unbound immediate
PersistentVolumeClaims" — a true statement, scored as invention. `Unschedulable`
moved into `evals.genericReasons` and the family it left was renamed
`resource-pressure`, which is what its remaining tokens actually assert. Run 2
is under the fixed metric; run 1's 0.900 recomputes to 1.000 by hand, since that
was its only bogus claim, and the table shows the corrected figure. The
generic-token rule has now been wrong twice in one session, both times found by
a live run rather than by a test, and both times in the same direction: honest
use of kubectl's own vocabulary scored as a lie.

**There is headroom again, which was the point.** Three cells are below the
ceiling in each run, spread over four different fixtures. Two repeat —
`fault-badselector`'s severity and `fault-ledger`'s recall, which are the
reproducible misses and therefore the useful ones — and two appear in one run
each. The seven-fixture suite had a single below-ceiling cell out of twenty-one,
the same one both times.

**`root_cause` measured something `fault_recall` could not, on its first run.**
On `fault-sessions` in run 1 the agent scored `fault_recall` 0.50 and
`root_cause` 1.00: it named the crash-looping Deployment behind the Service and
never mentioned the Service's dead endpoints. Recall alone reports that as "half
right", the same number it would give an agent that reported the broken Service
and never found the cause — and those are not the same answer. Run 2 reported
both and scored 1.00 on each. That is exactly the discrimination the evaluator
was added for, and it appeared without waiting for a regression.

**`fault-ledger` is the reproducible miss, and it is the "right and unusable"
failure again.** `fault_recall` 0.00 in both runs, on a report whose prose is
correct in both: *"PVC ledger-data stuck Pending — referenced StorageClass does
not exist"*, with the detail naming `fast-ssd-encrypted`, quoting the
not-found error, and calling it the root cause of the Deployment being down. The
object is right, the narrative is right, and the machine-stable triple says
`(PersistentVolumeClaim, ledger-data, Unschedulable)`. A PVC is not
unschedulable — pods are — so the one field `internal/monitor` fingerprints on
names the wrong failure mode, and the finding cannot be diffed against the next
cycle's volume-binding finding.

This is the same class as the `fault-failedjob` lapse that produced
`unidentifiedFindings`, one step further in: there the fields were empty, here
they are filled with a token borrowed from the downstream symptom. It is *not*
being fixed by widening the fixture's accepted reasons, which would dissolve the
measurement, and not yet by prompting, which would be tuning against a fixture
the same week it was written. It is the suite's headroom, and it is now the
best-evidenced open item in the read path: two runs, same fixture, same field.

(Later runs qualify "reproducible" and sharpen the diagnosis: it scored 1.00 on
one of three subsequent runs, and the run it passed differs from the ones it
failed in exactly one respect — it filed the pod as its own finding instead of
collapsing it into the PVC. See the three-run section below.)

**`fault-none` moved for a reason worth not "fixing".** Severity 1.00 in run 1,
0.33 in run 2 — the run-2 report carried three `warning`-level advisories
(missing liveness probe, missing readiness probe, no PodDisruptionBudget) on a
healthy Deployment, and `submit_health_report`'s consistency rule lifts
`overall_severity` to the highest finding severity, so a namespace the fixture
calls `ok` was reported `warning`. `hallucinated_fault` stayed 1.000 in both
runs, which is the design working: advisories are not inventions, and the
absence of a precision metric is deliberate. But severity is the channel through
which thoroughness still leaks into a penalty, and the honest reading is that
the agent's advisories are *correct* and their severity is arguable. The lever,
if one is ever pulled, is the rubric's line on what an advisory in an otherwise
healthy namespace is worth — not the metric, and not the consistency rule.

**Delegation is up from the collapse but not recovered:** 4/10 and 5/10
fixtures, 3 and 5 of 8 specialists. The three new fixtures are most of the
difference — `fault-storefront`, `fault-sessions` and `fault-failedjob` are
where the delegations happened — which is the fan-out returning as soon as a
namespace holds more than one broken thing, and is the argument the previous
baseline made from the other direction. **`log-analyzer` was invoked for the
first time in six tier-2 runs**, on `fault-storefront`. Its history is now
0, 1, 0, 0, 0, 1.

The diagnosis of the original `fault-badselector` failure is kept below in full,
because it is the argument for the tool and the record of what "fixed" was
measured against.

It failed identically in both of the previous runs — same fixture, same ~23s,
same two auditors, no structured report — so it was structural, not variance.
The mechanism:

- The fixture is a Service whose selector matches nothing, in front of a
  Deployment that is perfectly healthy. Nothing is unhealthy, so
  `k8s_cluster_health` and `k8s_triage_delta` correctly report the namespace
  clean and name no objects.
- Every lookout tool that returns object detail — `k8s_state_edges`,
  `k8s_resource_spec`, `k8s_triage_workload` — is scoped to *one* workload and
  wants `<Kind>/<namespace>/<name>`. **There is no enumeration primitive**: no
  `kubectl get all -n ns` equivalent, nothing that answers "what is in here".
- So the agent guessed. Both runs it tried `api`, `web`, `app`, `backend`; the
  Deployment is called `frontend`. Fifteen to nineteen tool calls of
  `resource_spec`/`state_edges` against names that do not exist.
- Then the specialist asked the user to run `kubectl get all -n
  fault-badselector` and paste the output — which kills the entire run (see "A
  Task sub-agent that ends its turn with a question"), so all three metrics
  score 0 rather than the fixture scoring badly.

Two things followed, and both are now built.

The read path needed a list/enumerate tool, and that was the highest-value gap
in it — not the audit family. An agent that cannot enumerate can only diagnose
faults that announce themselves in a health scan, which excludes the entire
class tier 2 was built to probe. That is `internal/kuberead`, and the four-call
trajectory above is the experiment coming back.

The "do not ask questions" prompt rule is **not sufficient**, and this is the
proof. Every spec in `specs/` carries it and the specialist asked anyway,
because it had genuinely run out of moves — a prompt cannot forbid the only
remaining action. That argued for the structural fix, which is now
the stall guard: a specialist that returns nothing degrades to a gap in
the orchestrator's report rather than taking the run down with it. Note the
shape of the damage it was costing, too — the orchestrator had already delegated
twice and would have reported *something*; the question cost a partial answer,
not a wrong one, and that partial answer is what the fix hands back.

The two fixes are independent and both were needed. The tool removes the reason
to ask; the callback removes the cost of asking. Either alone leaves the fixture
one model decision away from scoring zero, and the second is the general one —
`fault-badselector` is the stall we found, not the only stall there is.

And the eval did its job through all of it. This fixture has now been, in
successive baselines, the only severity miss, then a hard zero when the wiring
around it was fixed, then a clean recovery when the missing tool was built —
which is the whole life cycle a fixture with headroom is supposed to have. Its
severity has been one level hot in every run since, including both of these,
which is the last thing left in it.

**What the ten-fixture suite still cannot measure.** Every fault here is one
namespace's problem, and every one of them is visible to an agent that
enumerates and then triages the object that stands out — which is why the
orchestrator can still carry half the suite alone. The fixtures that would move
the delegation number further are ones where the *breadth* is real: a namespace
whose faults belong to different specialists at once (a security misconfiguration
next to a performance regression), or a fault whose evidence is only in a place
one specialist looks. `log-analyzer` is the standing example — it has now gone
0, 1, 0, 0, 0, 1 across six tier-2 runs, and the crash-loop fixture that writes
`FATAL: connection refused` to stderr is still diagnosed off pod status, because
pod status is enough. A fixture whose only evidence is in the log would be the
first one that is not.

**That fixture is now built, and it is the eleventh.** `fault-invoicing`
(`silentFailure`) is a Deployment that is `2/2` available, whose pods are `1/1
Running` with zero restarts and whose event stream holds nothing but
Scheduled/Pulled/Created/Started — and whose container has been polling a
database on `127.0.0.1:5432` in a retry loop since it started, getting
`Connection refused` from `wget` every five seconds, because the proxy sidecar
it expects is not in the pod. Every broad check the agent has reports the
namespace clean, correctly.

Three things about it are deliberate and are the difference between this and
"another fault, in logs":

- **Status is settled green before the agent looks.** The availability
  condition is load-bearing, not a formality: settling only on the log content
  would allow a run where the agent catches a half-rolled-out Deployment and
  reports *that* — a true finding about the wrong thing, scored as a miss.
- **Its Want reasons resolve to no `failureFamilies` entry**, because
  Kubernetes writes no reason for this and the agent has to coin one. The
  consequence is a second measurement for free: with `injected` empty, any
  concrete family claim in that namespace is charged as invention, so an agent
  that reaches for `CrashLoopBackOff` to explain a workload that has never
  restarted pays for it. `TestEveryFixtureStillNamesItsInjectedFamily` records
  the empty entry with that reasoning, so it cannot be mistaken for an
  oversight.
- **No accepted reason is assertable from status**, which took two attempts and
  is the part most likely to be got wrong again. `Want.MatchesReason` is a
  *bidirectional* substring match, so a short token the agent writes is accepted
  whenever a longer token in the list contains it. The first draft listed
  `DependencyUnavailable` and `ApplicationErrors`, which quietly admitted a bare
  `Unavailable` and a bare `Error` — the first is a real Deployment condition an
  agent could assert from status alone and be wrong about a 2/2 Deployment, the
  second is the token models write constantly and the reason `genericReasons`
  exists. Either would have paid full recall for never reading a log. The list
  is five compounds now — `ConnectionRefused`, `ConnectionFailure`,
  `Unreachable`, `DependencyFailure`, `DependencyMissing` — picked so the
  substrings they admit are harmless, and
  `TestSilentFailureRejectsStatusOnlyReasons` pins all three groups: the
  vocabulary that must be rejected, the spellings that must be accepted, and the
  bare substrings still admitted, listed explicitly so "harmless" is a judgement
  on the record rather than an oversight.
- **The namespace has a Service it does not need**, and that is the second
  correction. Without one it held a Deployment and no Service, which tier 3
  established is a finding an agent legitimately makes — `prod-checkout` was
  exactly that, and correctly critical. An agent that stopped there would be
  right, would have found the wrong thing, and if it spelled the observation
  `NoEndpoints` would additionally be charged with invention, because that is a
  no-endpoints family token in a fixture whose `injected` set is empty. A
  Service with a matching selector and ready endpoints removes the observation
  and adds one more green signal; its readiness is a settle condition for the
  same reason the Deployment's availability is.

**It is expected to score zero at first, and the zero will mean something
specific.** `fault-badselector` is the precedent — two baselines at zero, and
what the zero identified was a missing enumeration primitive rather than a bad
model. The analogous gap here is an orchestrator that treats "the broad scans
came back clean" as the end of an assessment, which is a defensible reading that
`fault-none` actively rewards. The two fixtures together are the discrimination:
`fault-none` punishes inventing a fault in a clean namespace, `fault-invoicing`
punishes stopping before you can tell the two namespaces apart. Nothing about
either is worth prompt-tuning until a run has been scored.

**And be precise about which of the two things it measures.** The paragraph
above wants a fixture for two reasons that are easy to conflate — that log
evidence be *necessary*, and that the delegation number move — and this fixture
delivers only the first with certainty. `k8s_triage_logs` is in the
orchestrator's own allowlist as well as `log-analyzer`'s, `pod-inspector`'s and
`job-inspector`'s, so an orchestrator that reads the log itself scores the
fixture perfectly and leaves the histogram exactly where it was. That is the
same thing the delegation line has been saying about the other ten and is not a
defect in the fixture; the alternative — taking the log tool off the
orchestrator so the fixture is forced to delegate — would be changing the agent
to make an eval measure what we wanted, which is the one move this harness
exists to catch. Run it and read the two numbers separately:

```bash
SRE_LOOKOUT_BIN=/tmp/lookout go run ./cmd/sre-eval-live -only fault-invoicing -v
```

**The first run answered both halves and corrected the first one.** Delegation
went exactly as predicted: `reliability-auditor`, not `log-analyzer`, and the
histogram did not move. The other half was wrong in two ways at once, and the
transcript is what showed it.

`k8s_triage_logs` is *not* the only tool carrying log evidence.
`k8s_triage_workload` bundles a distilled `logs` section next to spec, delta,
edges and radius, and advertises itself as "the first call of every incident" —
so the ordinary trajectory reaches log content without ever naming a log tool.
That is lookout working as designed and it is fine.

What was not fine is that **the fixture leaked its own diagnosis into the pod
spec.** The container announced its failure by echoing `ERROR: dial
billing-db:5432: connect: connection refused` on a timer, which put the entire
answer in `containers[0].command`, and both `k8s_resource_spec` and the bundle's
spec section hand that back verbatim. The run scored `fault_recall` 1.00 on this
fixture in 26 tool calls, none of them a log tool, having learned the hostname
from the command and then confirmed it with `k8s_net_probe`. Competent
diagnosis; not the thing the fixture was added to measure.

The fix is that the container now *produces* the failure instead of describing
it — `wget -q -T 3 -O /dev/null http://127.0.0.1:5432/healthz` in a loop, whose
own stderr says `wget: can't connect to remote host (127.0.0.1): Connection
refused`. The command now says only what a healthy manifest would say. It is
also a better fault: an app waiting on a database proxy sidecar nobody put in
the pod is a real production bug, and it is invisible in precisely the way this
tier could not previously pose.

The general lesson is the one this file keeps relearning from a different
direction: **a fixture whose workload is a shell one-liner has its behaviour in
its own spec**, and any tool that returns the spec returns the answer. Every
other fixture here is safe from it by accident — their faults are in status, so
the command is irrelevant — and this is the first one where the command *was*
the fault. Check it on the next fixture whose evidence is not in a status
field.

**Re-run with the leak closed: 0.00, which is the fixture finally working.**
`fault_recall` 0.00, `fault_severity` 0.67 (`warning` against `critical`, and
the second too-cold miss in either tier). The agent *found the evidence* —
`k8s_triage_workload`'s log section gave it "6 occurrences of `wget: can't
connect to remote host (127.0.0.1): Connection refused` across both pods" — and
filed it `info` with reason `TransientStartupError`, on the reading that the
errors were a startup blip that had resolved. Everything else in its report is
correct and unremarkable: `MissingProbes`, `NoPodDisruptionBudget`, both true
advisories on a genuinely healthy-looking Deployment.

So the fixture now measures the thing it was built for and the agent misses it,
which is the `fault-badselector` life cycle starting over. But the first score
is not a clean measurement of the agent either, because **the fixture handed it
a defensible misreading**: the pod was eighty seconds old, the error window it
saw was ten seconds wide, and "transient startup error that self-resolved" is a
reasonable thing to conclude from that. Two changes make the failure observably
ongoing rather than merely present — a two-second retry interval with a
`reconcile attempt N` line, and a settle condition that requires the error
inside the last 25 seconds rather than anywhere in the tail. The counter is not
a second leak: it says the app retries, which a healthy manifest also says, and
not that the retries fail.

That settle change is the general rule this file already states, applied one
level up: a condition means *the agent can now observe the fault*, and a fault
the agent will reasonably read as transient is not yet observable as what it is.
All eleven fixtures still inject under it. Whether the agent then reads a
forty-attempt failure as ongoing is the open question, and the next scheduled
tier-2 run is what answers it — nothing here should be prompt-tuned first.

The other open item is severity, and it is not a fixture problem: two of the
three below-ceiling cells in run 2 are severity, both too hot. As of these two
runs no run in either tier had ever produced a too-cold miss, which is the same
one-directionality tier 1 measures, now confirmed against a real cluster where
the agent chose the label from evidence rather than from a scenario paragraph.

**A later run broke that invariant, and the sentence above is kept as it was
written rather than retconned.** On 2026-08-14 at 17:44, `fault-oomkill` scored
`fault_severity` 0.67 with `warning` against a ground truth of `critical` — one
level *too low*, the first such miss in either tier. It is one occurrence and
not yet a trend, so the reading that severity error is a calibration problem
with a direction survives; what does not survive is stating it unconditionally.
The mechanism is ours and is worth knowing before the next run reads this: the
orchestrator rubric says an OOMKilled pod that "restarts back into service" is
`warning` — correct, and the reason tier-1 over-escalation came down — and the
very next sentence says a pod that cannot start at all is `critical`. The agent
took the first branch on a workload whose `Available=False` it had already
written into its own summary. The discriminator is in the rubric and was not
applied, so the fix is a test of current state, not a retreat to symptom-keyed
severity. See task #21, which is explicitly gated on reproducing it.

#### Three more runs the same day, re-scored together (2026-08-14, 17:10 / 17:44 / 18:24)

Three further runs, two of them prompt experiments rather than baselines: 17:44
tested the fix for lookout argument names, 18:24 tested the scan-scoping split.
All three are re-scored **offline under the current evaluators**, so the columns
are comparable to each other in a way none of them is to the published
run-1/run-2 table above. Re-scoring a saved transcript needs no cluster and no
Vertex call, since `Run.Health` is serialized and `evals.ScoreLive` is pure —
which makes it the right way to check a metric change, and the reason `#19` cost
nothing to verify.

```
                     18:24     17:44     17:10
fault_recall         0.833     0.944     0.889
hallucinated_fault   1.000     1.000     1.000
fault_severity       0.900     0.933     0.900
root_cause           1.000     1.000     1.000
delegation           5/10, 4   3/10, 3   3/10, 2
stalls / errors      0 / 0     0 / 0     0 / 0
```

These are keyed by wall-clock time, not numbered against the table above,
because they cannot be reconciled with it: 17:10 reproduces run 2's four
aggregates and its whole per-fixture pattern exactly, and reports 3/10 fixtures
and 2 specialists against run 2's published 5/10 and 5. Either it is a distinct
run that landed on identical scores with a different fan-out, or one of the two
delegation lines is wrong, and there is no way to tell from what was kept. The
identical-scores reading is not far-fetched — it is the same observation the
delegation counter exists to make, that a fan-out and its absence can score the
same — but it is a reading, not a fact, so nothing here is renumbered.

**Every run is green and no aggregate has moved outside ±0.06.** Nothing in the
prompt work of the last two runs shows up in a score, and neither experiment was
aimed at one. What the columns are good for is separating the standing misses
from the noise, and on that they are unambiguous: `fault-badselector` severity
is 0.67 in all three and in both published runs, which makes it the only cell
that has never moved. `fault-none` severity alternates on advisory count,
exactly as the run-1/run-2 note predicted. `fault-sessions` recall is 0.50 in
two of three.

**`fault-ledger` scored 1.00 once, and that run is the best evidence task #11
has.** It is 0.00 / **1.00** / 0.00 here and 0.00 in both published runs, so
"the reproducible miss" now needs a qualifier — but the run it passed is worth
more than the four it failed. Same fixture, same fault, same model:

```
17:44, recall 1.00                      18:24, recall 0.00
Deployment/ledger-writer                PersistentVolumeClaim/ledger-data
  reason=RolloutIncomplete                reason=Unschedulable
Pod/ledger-writer-77cb5bc896-jqk8m      Deployment/ledger-writer
  reason=Unschedulable                    reason=RolloutIncomplete
PersistentVolumeClaim/ledger-data
  reason=PVCPending
```

Both reports are correct in prose and both name the missing StorageClass. The
difference is that 17:44 filed **three** findings and gave each object its own
failure mode — the pod is `Unschedulable`, the PVC is `PVCPending` — while
18:24 filed two, collapsing the pod into the PVC and carrying the *pod's*
reason onto the PVC's row. A PVC is still not unschedulable.

That is a positive control, and it changes what #11 is. The agent is not
missing a vocabulary for a PVC's failure mode; it has one and used it. What it
does is borrow a reason across a layer boundary at exactly the moment it merges
two findings into one, and merging is otherwise good behaviour. So the fix is
not "teach it `PVCPending`" — it is that a finding's `reason` has to be checked
against the object the finding names, which is what #11 already says and now
has a worked example of, with a matched pair rather than a single failure.

**The scoping experiment did not reach the opening move, and the aggregate hid
it.** Bare-versus-scoped counts across the runs with transcripts:

```
              k8s_triage_delta     k8s_cluster_health   unknown arg names
17:10         10 bare /  0 scoped   1 bare / 0 scoped   28
17:44         12 bare /  4 scoped  11 bare / 2 scoped    0
18:24          8 bare /  7 scoped  12 bare / 1 scoped    0
```

The argument-name half is fixed and stayed fixed. The scoping half looks like
it moved on `k8s_triage_delta` until the calls are read in order: in **both**
17:44 and 18:24, the first two calls of **every** fixture are a bare
`k8s_cluster_health` followed by a bare `k8s_triage_delta`, ten out of ten, no
exceptions. Every scoped call in either run is a later, additional call — never
a replacement for the opening pair. Two fixtures got as far as scoping the
second of the pair; none scoped the first.

Three prompt formulations have now failed at this, and the reason is the same
one the stall guard was built for: nothing punishes it in context. A bare scan
succeeds, carries its summary line, and returns real findings — they are just
mostly about namespaces nobody asked about. It is a reflex, not a
misunderstanding, and a fourth paragraph is not the answer. The structural fix
is to fill an omitted `namespace` at the toolset boundary, which is the layer
that knows whether the run is scoped to one namespace; see task #20. Note what
it is and is not worth: no score has ever moved on this, so what it buys is
latency and a first result that is about the right namespace.

### Tier 3: a real cluster, unscored (2026-08-14, Sonnet 5, GKE `simian-test`)

Four assessments across three namespaces of a 94-day-old GKE cluster nobody
prepared for us, via `cmd/sre-agent`. There is no ground truth, so **nothing
here is a score** — every claim below was checked by hand against `kubectl`
afterwards, and that is the whole method. What this tier tests is what a
fixture cannot: a namespace with fourteen real workloads instead of two, an
incident that has been sitting there for 27 days, telemetry that actually
exists, and a cluster whose normal state is *already* full of things worth
mentioning.

```
online-boutique  48.1s   8 calls, no delegation   critical, 3 findings
online-boutique  2m44s  91 calls, 2 specialists   critical, 4 findings
prod-checkout    2m14s  36 calls, 2 specialists   critical, 5 findings
kube-system      3m39s  61 calls, 2 specialists   warning,  6 findings
                        0 stalls, 0 delegation errors, 4/4 reported
```

**Zero inventions in eighteen findings** — these eighteen; a later session found
the first false claim, and it was a misdiagnosis rather than an invention (see
"A fault we injected ourselves"). Every factual claim that could be
checked was true, including the ones that would have been the most natural
place to confabulate: the `gemma4-vllm` rollout is blocked by GKE Warden's
`ccc-node-affinity-selector-limitation` (a `cloud.google.com/gke-accelerator`
nodeSelector that Custom Compute Classes forbid) — the agent named the
constraint, not just the symptom; `loadgenerator` really is at 493Mi of a
512Mi limit, which it rendered as 96.4%; `prod-checkout` really has no Service
at all in front of its only Deployment; `kube-system` really has zero
PodDisruptionBudgets; and `cloud-kubernetes-gemini-agenttools` really is a
second field manager writing to all fourteen `online-boutique` Deployments.

The finding that says most about calibration is one it declined to escalate.
`Service/kube-system/antrea` has no endpoints, which is the exact shape of
`fault-badselector` — and instead of reporting a broken Service the agent
checked, found `antrea-controller` scaled to 0, and filed it `info` with
reason `IntentionalZeroReplicas`. A namespace like `kube-system` is full of
that: things that look broken and are not. `hallucinated_fault`'s design
argument — advisories are not inventions, misdiagnoses are — survives contact
with a cluster that has a hundred opportunities to get it wrong.

**`prod-checkout` is the case neither eval tier could have produced.** One
Deployment, one pod, `1/1 Running`, 12 days old, nothing in its status wrong at
all — and the correct answer is `critical`, because there is no Service, so
nothing can reach it. Tier 1 would have had to state that in the prompt; tier 2
would have had to inject it. Here it came out of enumerating a namespace and
noticing what was *absent*, which is a different skill from reading a status
field and the one the fixtures are worst at posing.

**The same two contract defects, on real objects.** Both are task #11 and both
are now much better evidenced than `fault-ledger` alone made them:

- `reason` still borrows a token from the wrong layer. `gemma4-vllm` got
  `Unschedulable` in both runs. Nothing was ever unschedulable — no pod was
  ever *created*; the ReplicaSet carries `ReplicaFailure`/`FailedCreate`. The
  prose is right and the fingerprint field is wrong, exactly as in
  `fault-ledger`. Note the word: `Unschedulable` is also what forced
  `genericReasons` into `internal/evals` a day earlier, and this is the third
  distinct occasion the agent has reached for it to mean "this workload has no
  running pods".
- `resource_name` is not always a name. One finding carried fourteen
  comma-joined Deployment names; another carried
  `(unspecified — 1 pod/1 container per top.unlimited scan)`. Both pass
  `unidentifiedFindings`, which only requires the field to be non-empty, and
  neither can be fingerprinted, deduplicated or closed by `internal/monitor`.
  A finding about fourteen objects is fourteen findings or it is a paragraph.

**Run-to-run variance is in the secondary findings, not the primary one.** Both
`online-boutique` runs led with `gemma4-vllm` and the same root cause. After
that they disagree completely: the 8-call run found the `loadgenerator` memory
headroom and a missing-limits container; the 91-call run found GitOps drift,
single-replica SPOFs and missing probes, and never looked at memory. The
difference is which tools ran — `k8s_resource_top` in one, `k8s_gitops_drift`
in the other — and delegation is most of it, since the two auditors are what
went looking for drift and PDBs. So the finding *set* is a function of the
trajectory in a way the fixtures cannot show, because a two-object namespace
has nothing secondary in it. Whatever the finding diff ends up being, it has to
survive this: a real namespace produces a different tail of true findings every
cycle, and a naive diff would report all of it as new.

**Delegation looks healthy here and it is the same two specialists every
time** — `reliability-auditor` and `config-auditor`, on all three delegating
runs, and nothing else. That is consistent with tier 2's collapse rather than a
counterexample to it: a real namespace is broad enough to want the audit
family, and still nothing here asks for `log-analyzer`, `job-inspector` or
`security-auditor`. Six of eight specialists went unused against the most
complicated input the agent has ever been given.

**Cost and latency, since this is the first entry point where they are
operational rather than a harness detail.** 48s to 3m39s per namespace, 8 to 91
tool calls. The 8-call run and the 91-call run reached the same critical
finding, so the eleven-fold spread bought the tail described above and nothing
about the headline. A scheduler running this on a cycle inherits that spread
directly.

#### Everything below the agent has to be told about credentials

The first `simian-test` run is worth keeping because of how it failed. Every
`lookout` tool returned an auth error, five of six delegated specialists
stalled, the run took 3m19s and 66 tool calls, and it still submitted a report
— naming the right Deployment, hedging the mechanism explicitly, and telling
the operator to fix the tooling. The stall guard and `k8s_list_resources`
between them turned a total read-path outage into a partial answer, which is
the strongest evidence either has produced; but the underlying failure was
ours.

`internal/lookout` spawns its subprocess with an **empty environment** on
purpose (see `Toolset`), and that is correct for a kind cluster, whose
kubeconfig carries an embedded client certificate and needs nothing else. A
managed cluster does not work that way: a GKE kubeconfig authenticates through
`exec: gke-gcloud-auth-plugin`, which must be found on `PATH` and reads its
credentials from `HOME`. With neither, every call failed with `executable
gke-gcloud-auth-plugin not found`.

`internal/kuberead` was unaffected — `internal/kubectl` builds `cmd.Env` with
`PATH` and `HOME` already — which is why enumeration worked while every check
failed, and why the agent could see *that* `gemma4-vllm` was 0/1 but nothing
about why. Restoring the credentials took the same namespace from 66 calls and
five stalls to 8 calls and the exact root cause.

The fix is `credentialNames` in `cmd/sre-agent`: an explicit allowlist, not an
inherited environment, and `KUBECONFIG` is dropped from it unconditionally
because it is the one variable that could repoint the child. What this costs is
that the pin now rests on two things instead of three — `KUBECONFIG` names the
file, and the file describes exactly one context — since `HOME` makes
`~/.kube/config` *reachable* even though client-go's loading rules never read
it. That is why `verifySoleContext` is mandatory in this command and merely
prudent in `kindcluster`, and why it prints the `kubectl config view --minify
--flatten` recipe rather than just refusing.

#### A fault we injected ourselves, on the real cluster (2026-08-14, later)

Two more `online-boutique` assessments, this time against a fault put there on
purpose: `emailservice`'s Deployment image set to
`gcr.io/google-samples/does-not-exist:v0-demo-break`, injected and restored by
hand between runs. Tier-2 methodology on tier-3 infrastructure — known ground
truth, but fourteen real workloads around it, a standing unrelated `critical`
in the same namespace, and the ordinary generic prompt. The fault was fully
settled before each run started (`ImagePullBackOff` on the pod within two
seconds of creation, ~10s and ~43s ahead of the two runs), so neither run was
racing the cluster.

```
                run 3 (16:12)                     run 2 (15:37)
elapsed/calls   1m14s / 10                        1m36s / 11
delegations     0                                 0
finding 1       Deployment/gemma4-vllm            same
                reason=Unschedulable
finding 2       Pod/emailservice-6597bbfdbb-j5lpq Deployment/emailservice
                reason=ImagePullBackOff           reason=ImagePullBackOff
finding 3       Namespace/online-boutique         6 webhooks, one field
                reason=SingleReplicaNoPDB         reason=SlowWebhookRisk
```

**The detection is the strongest single result the agent has produced.** Run 2
found the break among fourteen Deployments it had not been pointed at, named the
exact bogus tag, read the surviving pod's logs and quoted them, got the
structure right (`Service currently selects 2 pods but only 1 is ready` — true
for the fault's whole lifetime), and ranked it `warning` under `gemma4-vllm`'s
`critical` with the single-point-of-failure argument stated correctly.

**And run 3 is the first misdiagnosis on a real cluster.** It reported the same
pod as *"a stray pod from an old ReplicaSet"*, *"orphaned"*, scaled up *"out of
band … bypassing the Deployment"*, with *"the current Deployment … otherwise
healthy"*. The Deployment was at `revision: 39` carrying the broken image; the
failing ReplicaSet was `revision: 39`, the highest, and the Deployment's own
condition read `ReplicaSet "emailservice-6597bbfdbb" is progressing`. The frame
is inverted end to end: that ReplicaSet is the current one, the healthy pod's is
the old one, and nothing was out of band.

Three things about it are worth keeping.

- **It is not an invention, and that is the problem.** Pod, image, `NotFound`,
  `ImagePullBackOff`, the endpoint count — every checkable fact is true, so
  `hallucinated_fault` scores it 1.000 and no tier-2 evaluator scores the causal
  frame at all. The damage is in the remediation it implies: deleting an
  "orphaned" pod gets a new one in seconds, because the Deployment wants it.
  This is the argument for extending something `root_cause`-shaped past the one
  fixture that currently uses it.
- **Run 2 got it right on identical evidence,** so it is variance rather than a
  blind spot, and the missing step is one comparison the agent held both halves
  of — the Deployment's `spec.template` image against the failing pod's. Having
  written "the current Deployment is otherwise healthy" it cannot have made it.
- **What misled it is `k8s_recent_changes`.** It reported the 0→1 scale, and a
  bare scale event looks the same whether the Deployment ordered it or a human
  did. The agent supplied the missing half from the wrong prior.

**The result that most constrains the finding diff:** the same fault, held
constant, fingerprinted two different ways.

```
run 2:  (Deployment, emailservice,                  ImagePullBackOff)
run 3:  (Pod,        emailservice-6597bbfdbb-j5lpq, ImagePullBackOff)
```

`internal/monitor` would close run 2's finding as RESOLVED and open run 3's as
NEW, for a fault that never changed. The tier-3 notes above already said a real
namespace produces a different *tail* every cycle; this is worse, because it is
the headline fault on a pinned cluster. So fingerprint stability is not enough —
the agent has to pick the same **object** for the same fault, and it does not.
Deployment-versus-its-own-pod is the specific collision and the tractable one: a
pod-level finding whose owner already carries a finding is arguably the same
incident. That is the first thing the diff has to handle, ahead of tail noise.

**`reason: Unschedulable` on `gemma4-vllm` is now fully reproducible** — 3 of 3
runs today, 6 occurrences overall — which makes it the regression target for
task #11 rather than just its best evidence. **The `resource_name` defect is
intermittent**: run 2 put three `" / "`-joined webhook names in the field, run 3
filed the namespace-level advisory as `Namespace/online-boutique`, which is
well-formed. **Delegation was 0 in both**, 0 of 3 on this namespace today.

## Architecture: import, don't fork

We build on three upstream substrates and write only the SRE domain layer.

| Layer | Source | Role |
| --- | --- | --- |
| Runtime | `google.golang.org/adk/v2` | Runner, Chat/Task/SingleTurn agent modes, HITL-as-session-event, `session/database` durability, workflow graphs |
| Substrate | `github.com/go-steer/mast` | Permissions, providers, specialists, budget, digest, pricing |
| K8s data plane | `github.com/go-steer/k8s-lookout` | Read-path diagnostics, consumed **over MCP** |
| Domain | this repo | Write tools + HITL gate, specialist specs, scheduler, finding diff, Slack, evals |

Three consequences worth internalizing:

**We do not fork a substrate.** The thing being ported is an application, not
a runtime — upstream's `agent.py` is 177 lines. ADK v2 provides natively what
upstream builds by hand (checkpointing, interrupts, call limits). Forking
`mast` would inherit its Phase-1 schedule; forking `core-agent` would pin us
to ADK v1.

**lookout is a runtime dependency, not a compile-time one.** `k8s-lookout`
depends on `core-agent` → ADK **v1**, while `mast` is ADK **v2**. We consume
lookout by spawning `lookout mcp` (stdio JSON-RPC), which keeps ADK v1 out of
our binary entirely. Never add `github.com/go-steer/k8s-lookout` to `go.mod`.

**Specialists are the config surface.** The nine upstream subagents become
`mast/pkg/specialists.Spec` values. This repo is the proving ground for
multi-agent-via-config; resist hardcoding agent wiring in Go.

All nine exist as `internal/sre/specs/*.tmpl`: `pod-inspector`,
`scaling-analyzer`, `performance-analyzer`, `log-analyzer`, `security-auditor`,
`reliability-auditor`, `job-inspector`, `config-auditor`, and
`change-executor`. Each names its own lookout tool allowlist;
`internal/sre/agent_test.go` pins the roster so deleting a spec fails a test
rather than quietly shrinking the agent.

The eight read-only specialists run on `TierSubagent`. `change-executor` is the
exception and runs on `TierMain`: it is the only one whose mistakes land on a
real cluster, and writes are rare enough that the tier difference costs nothing
measurable.

`change-executor` also differs in two structural ways, both of which the tests
pin:

- **It exists only when the caller grants write tools.** `Build` skips the spec
  when `Config.Writes` is empty, so a monitoring deployment has no delegation
  target for a mutation *at all* — the orchestrator is not offered a tool it
  would nonetheless be told to route changes to. That is what the old "the
  ninth is deliberately absent" note was protecting, now enforced per-build
  rather than by the spec not existing. Both eval tiers build this way.
- **Its write tools are bound in Go, not by the spec.** See "The write binding
  cannot live in the spec" below.

Two constraints came out of building them. The orchestrator's `## Delegation`
section says **delegate for breadth, not for depth**, and never to more than
one specialist for the same question — without it the orchestrator fans out
redundantly and pays subagent latency for a second opinion it does not use. And
the four audit specialists must **report their own coverage gaps**: lookout has
no probe/`:latest`/pod-security checks, and a security report that silently
omits a category reads as a clean bill of health.

`mast` is resolved through a `replace` directive to `/home/user/projects/mast`
because it is private during early access.

## Commands

```bash
go build ./...
go test ./...
go vet ./...

# Regenerate the Python-vs-Go fingerprint vectors after touching monitor/
python3 dev/diffcheck.py && cp dev/golden.json internal/monitor/testdata/

# Tier-1 evals (needs Vertex credentials)
source ~/scripts/claude-env.sh
go run ./cmd/sre-eval -out /tmp/eval.json -concurrency 3 -v

# Tier-2 evals (needs Vertex credentials, docker, kind, and a lookout binary).
# Creates its own kind cluster, injects the faults, and deletes it on the way
# out — including on ^C. -keep leaves it up for inspection.
go build -o /tmp/lookout ../k8s-lookout/cmd/lookout
SRE_LOOKOUT_BIN=/tmp/lookout go run ./cmd/sre-eval-live -out /tmp/eval-live.json -v

# Tier 3: one read-only assessment of a cluster that already exists.
# Both flags are required — nothing here resolves the ambient current-context —
# and the kubeconfig must describe exactly one context, so minify it first.
kubectl config view --minify --flatten --context=simian-test > ~/kubeconfig-simian-test
SRE_LOOKOUT_BIN=/tmp/lookout go run ./cmd/sre-agent \
  -kubeconfig ~/kubeconfig-simian-test -context simian-test \
  -namespace online-boutique,prod-checkout -out /tmp/gke.json -v

# The same path as a guided manual check: preflight, hermetic tests, a minified
# kubeconfig, each of the three refusals firing, then one real assessment.
dev/smoketest.sh simian-test online-boutique

# Recapture lookout's MCP tool surface after upgrading the binary. This one
# takes -bin rather than SRE_LOOKOUT_BIN — it spawns lookout itself instead of
# going through internal/lookout, and the env var was never wired up here.
go run ./dev/captureschema -bin /path/to/lookout

# Score the bounded pass — the scheduled path — against the same fixtures and
# the same four evaluators as the agent. Same cluster setup, same flags.
SRE_LOOKOUT_BIN=/tmp/lookout go run ./cmd/sre-eval-live -bounded -v

# Re-grade saved tier-2 transcripts under the evaluators in the working tree.
# No cluster, no lookout, no Vertex call — ScoreLive is pure and Run.Health is
# serialized — so a change to an evaluator or to failureFamilies/genericReasons
# is measurable against every run ever recorded, before it is believed.
go run ./dev/rescore /tmp/eval-live-*.json
```

All three commands print what the run cost, per model and per agent, and all
three take `-max-cost` (USD) and `-max-turns` (model calls) to bound one
example, fixture or namespace. Both default to **unlimited**, and a published
baseline must be run that way — a ceiling that fires produces a run that scores
like a bad diagnosis. A run that hits one is reported as `STOPPED ON BUDGET`
rather than `FAILED`.

Env gates: `SRE_LIVE_MODEL=1` enables the tests that call Vertex;
`SRE_LIVE_CLUSTER=1` enables the test that builds a kind cluster and injects
every fixture into it; `SRE_LOOKOUT_BIN` points at the lookout binary for the
tests that spawn it. All are off by default so `go test ./...` stays hermetic.

Note which way that gating runs for safety: `internal/kindcluster`'s guard tests
are *not* gated. They cover refusing a foreign cluster, refusing an existing
kubeconfig, and rejecting an unprefixed name — all reachable without Docker, on
the principle that a safety check which only runs when Docker is up is a safety
check that does not run in CI.

The same rule holds for the write path. `internal/kubewrite`'s guards and
`internal/approval`'s deny/approve/bound behaviour are ungated: kubewrite tests
against a stub `Runner` and approval drives the real Chat → Task → gated-tool
nesting with a scripted model, so the one thing that must not regress fails in
`go test ./...` rather than only under `SRE_LIVE_MODEL`.

And again for tier 3, where the cluster is somebody else's. `cmd/sre-agent`'s
refusals — a kubeconfig describing more than one context, a toolset declaring a
mutating tool, an unclassified write on lookout's advertised surface, and a
forwarded `KUBECONFIG` — are all ungated and all reachable with no cluster, no
Docker and no credentials. The write-classification one is ungated because the
captured `tools.json` is checked-in data: the live handshake needs the binary
and is skipped without it, but the thing that must not silently change can be
tested from the repo alone.

## Findings worth not rediscovering

**There are exactly two ways to reach a Task-mode agent, and `agenttool` is
neither.** ADK spells them out in `workflow/validation.go`: a Task agent may be
a sub-agent of an LlmAgent coordinator, or it may be dispatched dynamically via
`workflow.RunNode`. That is the whole list.

We used `agenttool.New` for all eight specialists, and it fails in the least
visible way available. `agenttool` spins up its *own* inner runner with the
sub-agent as root (`tool/agenttool/agent_tool.go`); the runner rejects any root
that is not a Chat LlmAgent (`runner/runner.go`); and the refusal comes back as
an ordinary `FunctionResponse` carrying `"root agent pod-inspector must be a
chat LlmAgent, but has mode task"`. The orchestrator reads that, shrugs, and
does the work itself with its own lookout tools. Nothing errors. **No specialist
ever executed**, through two published eval baselines.

The orchestrator is therefore Chat mode with the specialists as `SubAgents`.
Three consequences:

- Only `runChat` dispatches delegation. `runTask` installs the delegation tools
  and never fires them, so a Task orchestrator with Task sub-agents is silently
  inert in a different way. The orchestrator's mode is load-bearing.
- A Chat root is legal, so the one-node `workflowagent` wrapper is gone and
  `internal/sre/root.go` with it.
- A Chat agent has no `finish_task`, and `OutputSchema` does not replace it —
  see the next finding, which cost a whole eval run to learn.

**ADK's Chat-mode `OutputSchema` is Gemini-only, and it fails silently on
everything else.** `needOutputSchemaProcessor` gates the `set_model_response`
injection on `googlellm.NeedsOutputSchemaProcessor`, which is
`IsGeminiModel(llm.Name()) && !CanUseOutputSchemaWithTools(llm)`. With an
Anthropic model the first conjunct is false: no tool is injected, ADK falls
back to setting `req.Config.ResponseSchema` natively
(`llminternal/basic_processor.go`), and the Anthropic/Vertex provider ignores
it. The agent answers in prose.

The first tier-1 run after the orchestrator moved to Chat mode returned 30
well-argued markdown reports and scored `severity_accuracy` **0.000**,
`severity_calibration` **0.022**. That is the shape of this bug: nothing errors,
the output is *good*, and every structured evaluator reads zero. It is the same
failure class as the `agenttool` one — a contract that looks configured and
isn't — found the same way, by scoring the thing rather than inspecting it.

So the report carrier is ours: `submit_health_report`
(`internal/sre/report.go`), a tool whose parameters are `schema.ReportSchema()`.
Same mechanism ADK uses internally, minus the dependence on the model vendor.
Two properties worth keeping:

- It **validates at submission** and hands violations back as a retryable tool
  error, while the model still has the evidence in context. `OutputSchema` gave
  no such hook; a malformed report surfaced as a decode failure in the harness
  long after the run. The consistency check (`overall_severity` must equal the
  highest finding severity) is contract enforcement, not calibration tuning —
  it cannot make a report less alarming than its own findings, and each
  finding's severity, which is where tier 1's over-escalation actually lives,
  is still entirely the agent's.
- `TestOrchestratorDeclaresTheReportTool` asserts it is on the wire by reading
  the `LLMRequest` through a recording model. ADK's `Agent` interface exposes no
  tool list, which is exactly how the previous carrier stayed inert across two
  published baselines without a single test noticing.

`internal/evals`'s `reportCarriers` now *ranks* carriers rather than listing
them. A run emits several — every specialist finishes with `finish_task` before
the orchestrator answers — so last-wins would credit a specialist's sub-report
as the orchestrator's answer on exactly the runs where the orchestrator failed
to produce one.

**A finding can be right and still unusable.** On one tier-2 run
`fault-failedjob` reported "Job fault-failedjob/nightly-report has failed
permanently (BackoffLimitExceeded)" in its `title` and `detail`, left `kind`,
`resource_name`, `reason` and `namespace` empty, and scored `fault_recall`
**0.000** on a fault it had diagnosed correctly. Those fields are the
fingerprint `internal/monitor` diffs runs on, so a finding without them cannot
be tracked, deduplicated or closed — it is a paragraph, not a finding.
`unidentifiedFindings` now rejects a submission missing `kind`,
`resource_name` or `reason`.

Two judgement calls in that check. `namespace` stays unchecked, because
cluster-scoped objects genuinely have none and a required field an honest answer
cannot fill teaches the model to invent one — the same reasoning that keeps the
three fields *optional* in `ReportSchema` rather than JSON-required. And the
check lives at submission rather than in the evaluator specifically because the
lapse is **intermittent**: the next run filled the fields and scored 1.000. A
contract violation that happens sometimes is worse than one that happens always,
because it makes a score that moved for this reason indistinguishable from one
that moved because the diagnosis changed. Catching it while the model still
holds the evidence costs a round trip and removes the variance.

**Non-empty is not the same as identifying, and tier 3 found both ways it
fails.** `unidentifiedFindings` only asks that the three fields be present, and
against a real cluster the agent filled them with things that are not
identifiers: one `resource_name` held fourteen comma-joined Deployment names,
another held `(unspecified — 1 pod/1 container per top.unlimited scan)`. Both
submissions passed, and neither finding can be fingerprinted — the first is
fourteen findings wearing one coat, the second names nothing at all. The
`reason` half fails the same way and more often: `Unschedulable` on a workload
whose ReplicaSet says `ReplicaFailure`/`FailedCreate`, which is `fault-ledger`'s
defect on a real object.

Both halves are now checked at submission, in `internal/sre/identity.go`, and
they are different kinds of check because the two fields fail differently.

**A name is checkable syntactically, and the separator is not the signal.**
`unnamedResources` requires `resource_name` to match one object name — the
recorded failures joined names with a comma, with `" / "`, and with prose in
parentheses, so a check keyed on any particular punctuation would have caught
one of three. The pattern is deliberately wider than RFC 1123 (uppercase and
underscores pass, which the API server would reject) because the question being
asked is not "is this name legal" but "is this field holding one name";
`monitor.Fingerprint` lowercases, so a case difference costs nothing.

**A reason is only checkable against the object the finding names**, which is
`crossLayerReasons`: Kubernetes' own vocabulary partitioned by the layer that
*emits* each token, kinds partitioned the same way, and a cross-layer pairing
refused. Four things about it are worth not rediscovering.

- **A prompt had already failed at this.** The 2026-08-14 orchestrator gained an
  explicit instruction to take delta's reason as the failure mode of the object
  it is attached to, and `fault-ledger` came back 0.00 with
  `reason=Unschedulable` for the third consecutive run. Same shape as the stall
  guard: necessary, not sufficient.
- **The table is known-token → legal-layers, so an unknown token passes.** An
  incomplete map under-enforces rather than over-rejects, which is the same
  judgement call that keeps `namespace` unchecked — and the one `genericReasons`
  got wrong twice in one session, both times by scoring honest use of kubectl's
  vocabulary as a lie. Membership is on the exact normalized token for the same
  reason task #22 exists. **The cost of that choice is now measured**, on the
  first live run after it shipped: `Service/session-store reason=PodsNotReady`
  is the PVC defect on a mapped kind, and it passed because the agent coined a
  token no table of real emitted reasons can contain. Catching it needs the
  opposite polarity and is not a matter of adding rows. That is now built and
  it is *not* an allowlist: `leadingSubject` refuses a coined compound whose
  first word is another kind's noun, which enumerates kinds rather than
  vocabulary and so stays open-world. See the #30 note above and the
  2026-08-14 21:30 tier-2 baseline.
- **Only the kinds where it cannot misfire are mapped.** PVC, PV, Service,
  Ingress, ConfigMap and Secret; the workload controllers, Pod and Node are
  deliberately absent, so `Deployment/gemma4-vllm reason=Unschedulable` — the
  *more* reproducible miss, 6 occurrences — still passes. "Deployment:
  Unschedulable" is at least arguable as a summary of its pods' state and the
  PVC case is not, so that half has to be measured before it is widened.
- **The wording is the load-bearing part, and the positive control dictated
  it.** The obvious way for the model to satisfy a layer check is to re-file the
  finding against the pod, which trades this defect for the object-choice
  instability of task #16 — the same fault fingerprinted as a Deployment on one
  run and as its own pod on the next. But the 17:44 run shows the agent's own
  better answer keeps the PVC finding and gives it `PVCPending`: it does not
  have to move the finding to fix the reason. So the rejection says *name this
  object's own failure mode* and never mentions the pod, and a test asserts
  that it doesn't.

**And the handback is bounded, which is new for this tool.** `maxRejections` is
3, after which the report is accepted and the acknowledgement leads with
`sre.ProtestMarker` naming the violations that were not resolved. Every check in
`validate` is satisfiable, so in the normal case this never binds; it is there
because both eval commands ship with `-max-turns` unlimited, and a model that
cannot find a token the layer check accepts would otherwise resubmit forever.
Refusing outright is the worse failure — `reported` 31/31 is the headline tier-1
result and a rejection loop costs the whole run, where accepting under protest
costs exactly what the defect cost before the check existed. The marker is the
"no silent caps" rule applied one layer down: a run that gave up on the contract
must not read like one that satisfied it.

**And it is counted now, which it was not when the bound was written.** The old
note here said counting was "the follow-up if it ever fires", and the trigger
for doing it early was `crossLayerReasons` growing its second signal: a rule
that fires more often makes a rejection loop more likely, and the bound was
still invisible. `Run.Protests` records the unresolved violations of every
protested submission, both eval commands print `reports accepted under protest`
*including its zero*, and `cmd/sre-agent` appends `UNDER PROTEST:` to the
namespace footer beside `STALLED:`. It has still never fired; printing the zero
is the point, because "never fired" should be readable off a run rather than
assumed.

Three things about the shape, each the same lesson as `Run.Stalls` at the other
end of the run:

- **The protest travels on the success channel.** `Run` returns it under
  `result`, not `error`, because the report *was* accepted — which is exactly
  the hazard. To anything not looking for the marker it is an ordinary
  acknowledgement.
- **The entry carries the violations, not a count.** They name the check that
  could not be satisfied in three tries, which is the difference between
  actionable and merely alarming — the same argument that makes a stall entry
  carry the specialist's last words.
- **The seam is `sre.Protest`, mirroring `mastagent.Stalled`,** and the two
  packages test their own halves of it. `internal/evals` matches through it and
  tests that `protestedSubmissions` counts what it returns; `internal/sre` tests
  that the give-up produces something it recognises. Without that second half
  the counter could watch for a string the tool no longer writes and report
  "none" forever, which is the failure it exists to prevent one layer up.

**ADK loses an ordinary tool call batched into the same turn as a
delegation.** `runChat` dispatches every delegation FC in an event and then
`break`s out of the agent's iterator so the outer loop can re-enter with the
specialist's answer in history. The break happens before the base flow executes
the event's *other* function calls, so a batched `k8s_cluster_health` is never
run and never answered. The assistant message keeps a `tool_use` block with no
`tool_result` after it, and Anthropic rejects the **next** request:

```
messages.4: `tool_use` ids were found without `tool_result` blocks
immediately after: toolu_...
```

This killed 16 of 31 tier-1 examples the first time delegation actually worked,
and it reads as a provider error rather than as the framework bug it is —
nothing in the message points at delegation. A multi-specialist fan-out is
fine; only mixing a delegation with a plain tool call in one turn breaks.

`serializeDelegations` (an `AfterModelCallback` on the orchestrator) drops the
batched non-delegation calls before the response becomes an event, so the
dangling `tool_use` never enters history. The model re-issues them on the next
round — the cost is a round trip, and nothing is lost. Prompting the
orchestrator to delegate alone was the alternative and is the wrong tool: one
lapse costs the whole run, and batching is usually the right thing for a model
to do. `TestParallelDelegationKeepsCallsAndResponsesPaired` reproduces the
defect hermetically with a scripted model and will start failing — correctly —
if ADK ever fixes `runChat`.

**A delegation count counts attempts, not outcomes.** This is what let the above
hide. `evals.specialistCalls` counted `FunctionCall` parts named after a
specialist, which is exactly as true of a delegation that failed as of one that
worked — the error only appears later, in the response. `Run.DelegationErrors`
now records responses carrying an error, and `TestDelegationActuallyExecutes`
runs the real `Build()` path against a live model and fails unless a specialist
both ran without error *and* came back with a payload. An error-free empty
response is its own failure mode and scores identically to a real one.

**A Task sub-agent that ends its turn with a question kills the whole run.** If
it never calls `finish_task`, ADK deliberately does not synthesise a
delegation-closing response — it ends the *caller's* turn too, so the
orchestrator never regains control and the run produces no report at all. ADK is
explicit about it and its reasoning is sound for an interactive coordinator,
where the user's next message should route back into the paused task
(`agent/llmagent/llm_agent_wrapper.go:518`). It is wrong for this agent: on a
monitoring cycle there is nobody to answer, so the delegation stays unresolved
forever.

Two things were done about it, in this order, and the order is the point.

First the prompt, which is necessary and **not sufficient**. A specialist has no
interactive channel and must be told so; every spec in `specs/` says it, and
`lookout.OfflineMessage` says the absence of telemetry is permanent for the run.
Without that last clause a specialist would enumerate the telemetry it wanted
and wait forever — reasoning correctly about missing data, and grading as a
total failure. Tier 2 then showed a specialist asking anyway, because it had
genuinely run out of moves: a prompt cannot forbid the only remaining action.

Then the structural fix — an `AfterModelCallback` on every specialist that
appends a `finish_task` call to a terminal silent turn, carrying an empty report
whose summary opens with `mastagent.StallMarker` and tells the orchestrator the
area is *unchecked*, not clean. It was `finishOnStall` in
`internal/sre/stall.go` and is now `mastagent.FinishOnStall` with
`sre.stallReport` as its payload; see the mast#130 note below for which half
went upstream and why the payload did not.
The delegation closes, the orchestrator regains control, and the run costs one
section of the answer instead of all of it. Four things about it are worth not
rediscovering:

- **A wrapper agent cannot be written.** The obvious shape — an `agent.Agent`
  that notices the drained iterator and synthesises the output — is impossible
  outside ADK, because both the coordinator's delegation-tool builder and
  `workflow.AgentNode` recognise a Task agent by type-asserting to
  `internal/llminternal.Agent`. A wrapper would be silently demoted to a
  transfer target and delegation would stop working altogether. The interception
  has to happen at the *response*, one layer down.
- **An FC appended by an `AfterModelCallback` really does execute.**
  `handleFunctionCalls` reads the post-callback response and
  `finalizeModelResponseEvent` stamps it with an ID, so the injected call
  produces exactly the event pair `runTask` waits for: the FC sets
  `pendingFCArgs`, the tool's success response promotes them to the node Output.
- **The report must say "ok".** `schema.HealthReport.Validate` rejects any other
  severity with no findings, and that rule is right — a severity is a severity
  *of* something. But "ok" read alone is an affirmative claim of health nobody
  made. A severity field cannot carry "not checked", so only the text can, which
  is why the marker leads the summary rather than trailing it.
- **The specialist's last words are kept verbatim.** They are usually the most
  informative thing in the delegation: the question it wanted to ask names the
  data it could not get. That is how the missing enumeration tool was found.

And a degraded run has to be countable, which is the same lesson as
`DelegationErrors` one rung further along. Before the fix a stall was
unmissable — the run died. Now it is a complete-looking run with a hole in it,
so `Run.Stalls` records it (via `mastagent.Stalled`, which matches the marker at
the head of a specialist's response summary) and both eval commands print the count including
its zero. `evals.extractReport` also refuses a stall report as the run's answer:
it is a valid `finish_task` payload reading "ok, no findings", so crediting it
would score a run that diagnosed nothing as one that swept the cluster and found
it healthy — the most flattering possible reading of the exact failure the guard
exists to surface.

**A Task specialist has a second way out, and taking it kills the run.** ADK
offers every sub-agent `transfer_to_agent` alongside `finish_task`.
`transferTargets` skips Task-mode agents, so a specialist's peers are not
targets and it has no sub-agents of its own — the *only* destination it is ever
offered is the Chat-mode orchestrator, and that transfer is fatal. ADK forwards
it in-process, so the orchestrator's `runChat` runs under the specialist's node
context; `workflow/agent_node.go:104` rebuilt that context without a
`SubScheduler`, and `runChat`'s first act is to re-dispatch the still-unresolved
delegation FC through `workflow.RunNode`:

```
workflow: dynamic child sre-orchestrator/pod-inspector@toolu_…:
workflow: dynamic child failed: workflow: RunNode called outside a dynamic node
```

Both specialists are now built with `DisallowTransferToParent` and
`DisallowTransferToPeers`, which empties `transferTargets`, makes
`shouldUseAutoFlow` false, and removes the tool and its instruction block from
every specialist request. That is right on its own terms and not only as a crash
fix: delegation here is one-way, and a specialist that transfers abandons the
question it was asked with no report for the orchestrator to merge.

Two things about how this was found are worth keeping. It killed **2 of 7
tier-2 fixtures on each of two runs, on different fixtures each time**, which is
what a model-dependent choice looks like from the outside — it read as an ADK
flake for a while, and "flaky" is the label under which a deterministic bug
hides. And no amount of reading the failing fixtures' trajectories would have
found it, because the trajectory ends *before* the transfer; what settled it was
scripting the specialist to transfer and watching the exact error string come
back (`TestTransferFromASpecialistIsFatal`). `TestSpecialistsAreNotOfferedTransfer`
asserts on the `LLMRequest`, because the declaration surface is what the model
acts on and ADK builds it from flags several layers below `llmagent.Config`.

The flags belong upstream in `mast` — the hazard is a property of any Task
specialist under a Chat coordinator, not of this repo — but `TaskAgentConfig`
could not express them, which was one of the two reasons `buildOne` calls
`llmagent.New` directly rather than `mastagent.NewTaskAgent`.

That half is fixed upstream: [mast#126](https://github.com/go-steer/mast/pull/126)
(merged 2026-08-14) adds both flags to `TaskAgentConfig` and sets them in
`specialists.Build`, so mast's own `DispatchCoordinator` topology — a Chat
coordinator with Task specialists as `SubAgents`, which is exactly the hazard
shape — is no longer exposed. Note what writing it turned up in ADK that we had
not: `instructionsForTransferToAgent` returns `""` for Task and SingleTurn modes
while `appendTools` still runs, so the specialist is handed the tool with **no
instruction block explaining it** — a fatal affordance and no guidance on when
it applies.

**The stall guard is upstream too now, and `buildOne` is a `NewTaskAgent` call
again.** [mast#128](https://github.com/go-steer/mast/issues/128) →
[mast#130](https://github.com/go-steer/mast/pull/130) (merged 2026-08-14) adds
`Before/AfterModelCallbacks` to `TaskAgentConfig` and `CoordinatorConfig`, plus
`agent.FinishOnStall`, `agent.Stalled` and `specialists.BuildOptions.OnStall`.
It is the same hazard as the transfer flags from the other end — both are ways a
Task specialist ends a turn that kill the caller, and both are properties of
ADK's Task mode rather than of this repo — so `internal/sre/stall.go` is now 60
lines of payload where it was 200 of guard.

**The split is the interesting part, and it is the difference between
conforming and empty.** mast owns the callback: what counts as a terminal silent
turn, that the rewrite is additive so a thinking block survives with its
signature, that an appended FC really executes. What it deliberately does *not*
own is the `finish_task` arguments, because the injected call is validated
against the spec's `OutputSchema` exactly as a model-issued one is, and only the
roster that wrote the schema knows what an empty value means in its own
contract. mast *can* synthesise a conforming one — `conformingArgs` in its
`schemafill.go` does it for the offline fakes — and using it here would be wrong
for exactly that reason: it invents content. Ours is the worked example in the
PR. `findings` must come back `[]`, because a fabricated entry enters the
incident stream as a real cluster fault named "a subagent stopped talking"; and
with the findings list empty the severity is *forced* to `ok` by a rule in
`schema.HealthReport.Validate` that mast cannot see. `sampleValue` would fill
both.

So `stallReport` is a `mastagent.StallPayload` and nothing else, and the two
tests that survived the collapse are the ones testing this repo's half:
`TestTheStallReportSatisfiesTheContract` (the payload against the schema *and*
against the report tool's own validator) and the matched pair
`TestASilentSpecialistDoesNotKillTheRun` / `TestWithoutTheGuardASilentSpecialistEndsTheRun`
(the wiring, with the control that stops the treatment passing vacuously). The
callback-mechanics tests were deleted rather than kept: a second copy of an
upstream test fails in two repos for one cause and drifts the day mast changes.

Two consequences to know before reading an old transcript. The marker text
changed — `INCOMPLETE — no report from this specialist.` became mast's
`INCOMPLETE — no result from this agent.` — so a grep across transcripts from
before this lands wants both. And `sre.StallMarker` is gone: `internal/evals`
counts stalls through `mastagent.Stalled`, which is the seam the PR added for
exactly this and does the same head-of-string match for the same reason.

**What still keeps `specialists.Build` bypassed is the schema, and that is not
going to change.** `Build` can pass an `OutputSchema` but only the one a spec
declares in frontmatter, and this repo's contract is `schema.ReportSchema()` — a
Go value shared with the orchestrator's report tool. Restating it as YAML in
eight spec files would fork the contract into nine copies, which is a worse
trade than one `NewTaskAgent` call. `BuildOptions.OnStall` exists for rosters
that do not have that problem.

**Vertex signals back-pressure two different ways.** A 429 is the obvious one;
`overloaded_error` arrives in a streamed body under an HTTP **200**, so it
passes every status check while still meaning "retry later". `internal/evals`
retries both. Nine of 31 examples were lost to the first and three to the
second before it did — and because quota exhaustion is bursty, the losses are
correlated, not a random sample.

**Retry patience must be wall clock, and backoff must be jittered.** Both
lessons came from one tier-2 run and both are pinned by tests in
`internal/evals/retry_test.go`.

An attempt cap sounds like a patience budget and is not: with exponential
backoff, `-retries 4` bought 3m45s. The first full tier-2 run met a quota
outage lasting over an hour, so every fixture exhausted its retries at nearly
the same moment and **6 of 7 were lost**. The budget is now wall clock
(`-retry-for`, default 20m). On the rerun `fault-oomkill` scored after waiting
17m6s — an example the old policy would have thrown away.

The same log showed three fixtures backing off through 15s/30s/60s/120s in
perfect lockstep, so every retry re-collided with the other two. Concurrent
workers sharing a quota and backing off deterministically synchronize and stay
synchronized. Tier 1 had an anti-lockstep offset already — a fixed `i%7`
seconds, which spreads at most 6s across a 120s backoff, enough to look
addressed and not enough to decorrelate. `Backoff` is now capped exponential
with ±25% jitter, and the cap bounds the *jittered* result rather than the base:
clamping the result instead would land half of all draws on exactly the cap,
which is a mass point that re-synchronizes the workers the jitter exists to
spread.

**`functiontool` intercepts a rejection before the handler runs.**
`functiontool.Run` checks `ctx.ToolConfirmation()` before it decodes arguments
or calls the handler (`tool/functiontool/function.go:202`), and on
`!Confirmed` returns `tool.ErrConfirmationRejected` unconditionally — the
`requireConfirmation` flag governs the *else* branch only. So a package that
raises its own confirmation to control the hint, as `internal/kubewrite` does,
still cannot shape its own rejection. Its handler is simply never reached.

What ADK produces instead is `{"error": "error tool \"kubectl_delete_pod\"
call is rejected"}`. That does not end the run — the base flow turns a tool
error into an ordinary `FunctionResponse` — but it is the wrong shape twice
over: it reads as a malfunction rather than as a decision, and a malfunction
invites another attempt; and it arrives on the `error` channel, so a
`change-executor` asked to report what it did has nothing structured to report.
`kubewrite.gatedTool` wraps each write tool to substitute a `Result` with
`status: rejected` and an explicit "do not retry, and do not attempt an
equivalent change by another route" — the second clause because the obvious
reading of a refused `kubectl_delete_pod` is that `kubectl_rollout_restart` is
still available, and a human who declined the change declined the change, not
the spelling.

The wrapper has to mirror ADK's own `confirmationTool` (`tool/tool.go`) in
`ProcessRequest`: let the inner tool declare itself, then substitute the
wrapper into `req.Tools[name]` so the wrapper's `Run` is the one that executes.
Skip the substitution and the declaration is right while the gate is bypassed —
invisible until someone rejects a change and watches it happen anyway.

**ADK's `mcptoolset` throws away the one annotation a read-only agent needs.**
`convertTool` copies an MCP tool's name, description and schemas onto a
`tool.Tool` and drops `mcp.Tool.Annotations` on the floor
(`tool/mcptoolset/tool.go`), so `readOnlyHint` — the server's own statement
about whether calling the tool changes anything — is unreachable from a
`tool.Toolset`. Neither seam that looks like it would help does: `ToolFilter` is
a predicate over the *converted* tool, and `Config.Client` takes an `*mcp.Client`
rather than the `MCPClient` interface, so there is nowhere to interpose on the
`tools/list` response.

That is why `internal/lookout.Surface` is a second handshake rather than a
wrapper. It costs one extra subprocess spawn per `cmd/sre-agent` invocation —
once, before the first namespace, not per assessment — and it is the only way to
ask the binary a question ADK has already discarded the answer to. The same gap
is why `dev/captureschema` now records annotations: the captured surface is
where an ungated test can see them.

**The write binding cannot live in the spec.** Everywhere else in this repo
"specialists are the config surface", and writes are the deliberate exception.
The spec allowlist matches MCP toolsets *by server name*, so if writes were
granted that way any spec could grant itself writes by naming the server — and
`specs/` is the operator-editable surface, overridable wholesale with
`Config.SpecDir`. Upstream's guarantee is that the main agent holds only read
tools and every mutation goes through one interrupting subagent; **a guarantee
a config file can revoke is not one.** So `Config.Writes` is handed to
`WriteAgentName` in Go. A spec may still *subtract*, via `tools.builtin`, and
naming a tool that does not exist is an error rather than a silent empty
allowlist — a typo in an allowlist is otherwise indistinguishable from an
intentional revocation.

**The only per-agent discriminator on an `LLMRequest` is the identity
sentence.** `model.LLMRequest` is `{Model, Contents, Config, Tools}` — nothing
names the agent — so a single recording model stub shared across a roster
cannot tell the orchestrator's request from a specialist's. But ADK's identity
processor writes `You are an agent. Your internal name is %q.` into every
system instruction (`internal/llminternal/identity_request_processor.go:39`),
and parsing that string back out is a reliable key.

That is what makes the structural wire tests possible:
`TestOnlyTheWriteSpecialistIsOfferedWriteTools` fans out to all nine agents in
one turn and asserts, per agent, who declares a write tool — the declaration
surface being the thing the model actually acts on, and the thing ADK builds
several layers below `llmagent.Config`. `internal/approval`'s hermetic tests
use the same key to script a different reply per agent through the real
Chat → Task → gated-tool nesting.

Two caveats. The processor returns early for `ModeSingleTurn` agents, which
have no identity block. And a test keyed this way passes vacuously if an agent
never reaches the model at all, so assert that every expected name was *seen*
before asserting anything about what it declared.

**`runner.Run` does not echo the incoming message as an event.** Scanning a
turn's events for the confirmation `FunctionResponse`s you just sent finds
nothing, because the resume message is an input, not an output. This matters
for the property `approval.Answer` exists to preserve — that all of one round's
decisions travel in a single user message, since Anthropic rejects a history
where an assistant message's `tool_use` blocks are not all answered by the
message immediately after. `approval.Turn` therefore keeps `Resumes
[]*genai.Content` alongside `Events`; it is the only record that the answers
went back together, and the only thing a test can assert against.

**Token usage was unattributable, and the fix is in two different repos.**
`internal/evals.Usage` sums `session.Event.UsageMetadata` across a run, and on
its first live run every one of 98 requests landed under `"unknown"`: ADK
defines `LLMResponse.ModelVersion` and mast's Anthropic provider never set it.
One undifferentiated total is unusable for cost, because the two tiers are
priced differently, and unusable for checking a tier assignment, which is the
only reason to have tiers. It is set upstream now, from the model the API echoed
back rather than the one we asked for — they normally agree, and where they do
not the server's answer is the billed one.

The model split still cannot answer the question this repo keeps asking, so
`Usage` records a `Span` per (agent, model) pair — `session.Event.Author` and
`ModelVersion` together — and `ByModel`/`ByAgent` are folds of that one table
rather than two parallel maps. The pairing is what makes the per-agent view
*priceable*: an agent's tokens are only billable once you know which tier they
ran on, so two independent maps could report the split and never the cost.
**ByModel prices a run; ByAgent says where the cost went**, and the standing
arguments here are all
about that: whether a fan-out pays for itself, what the 8-call and 91-call
tier-3 assessments differed by, whether `change-executor`'s placement on the
main tier costs anything. The model is a proxy for the tier; the author *is* the
tier, and it survives two agents sharing one model — which `change-executor`
deliberately does, and is therefore exactly the pair a model split collapses.

The first measured attribution, three tier-1 examples with two delegations:

```
claude-haiku-4-5-20251001  in=47,213  (fresh 26,413)   out=6,625  reqs=8
claude-sonnet-5            in=410,052 (fresh 210,000)  out=7,202  reqs=17
  performance-analyzer     in=20,620                   out=3,487  reqs=4
  pod-inspector            in=26,593                   out=3,138  reqs=4
  sre-orchestrator         in=410,052                  out=7,202  reqs=17
```

The orchestrator is **89.7% of input tokens**, and the two rows cross-check —
sonnet's total is the orchestrator's exactly, haiku's is the two specialists
summed — which is the only assertion available that both maps were fed from the
same responses. Two things follow for the delegation argument. A specialist is
cheap because it is cheap *twice*: the subagent tier and a context that does not
carry the orchestrator's history. And the orchestrator's input is where any
budget will actually bind, so "delegate for breadth" is a cost argument as well
as an accuracy one — the fan-out is not what a run is paying for.

Read the block the way `reported` is read: `UsageSummary` prints a coverage line
(`3/3 runs measured`) even when it is complete, because the aggregate is a sum
over whatever reported usage and a sum says nothing about what it left out. A
suite where half the runs died before reaching the model produces a perfectly
plausible total. For the same reason an event with no `UsageMetadata` writes
nothing rather than a zero: "not measured" and "free" must not render alike.

**mast's flat cost rate overcharges this agent by ~5.9x, and the fix was
unblocked by the ModelVersion one.** `pkg/budget.Meter` priced a session as
`TotalTokenCount x RatePer1K`, with `internal/compose` deriving that rate as the
plain average of a model's input and output prices — an approximation whose
premise was that the meter could see nothing else. Two things had since made
that false: the event carries the input/output split *and* the cache-read
subset, and (after the fix above) it names the model, so a call can be priced
against the same `pkg/pricing` catalog everything else uses. On the recorded
18:24 tier-2 figures — in 2,326,094 of which 1,469,558 were cache reads, out
39,676 — the flat rate says **$14.19** and exact cache-aware pricing says
**~$2.40**. A cost ceiling wrong by that much fires on the wrong sessions. So
`budget.Limits` grew an optional `Catalog`, and this repo always supplies one.

Cached input is the whole of the error, and it is structural rather than a
coefficient: cache reads are billed at a tenth of fresh input, and on this agent
they are 63% of the prompt. Any pricing that folds them in at the input rate
overstates a cache-warm agent specifically, which is to say the operational
case.

**"Unpriced" is not "free", and it is enforced in three places** because it is
the same failure as "not measured" one layer down. `Group.Unpriced` renders
`$—` rather than `$0.0000`; `Meter.Unpriced()` counts the calls a catalog miss
sent to the fallback, so a mixed figure can be labelled as one; and
`evals.Limits` deliberately sets `RatePer1K` to the main tier's average even
though the catalog prices both models we run — a ceiling whose fallback is zero
is a ceiling that silently stops metering the day someone points it at a model
nobody added to the table. For *enforcement* the safe direction is to
overcharge. For *reporting* it is to refuse to state a number.

**A ceiling is off by default in every command, and a stop is labelled.**
`Runner.Limits`' zero value is unlimited and that is what `cmd/sre-eval`,
`cmd/sre-eval-live` and `cmd/sre-agent` ship; `-max-cost` / `-max-turns` are
opt-in. The reason is the harness's own "no silent caps" rule: a run cut off
part-way through reports fewer findings and lower recall, which reads *exactly*
like an agent that failed to find them, so a baseline quietly bounded is worse
than an expensive one. Where a ceiling belongs is the operational path — a
scheduler on a cycle, where an agent that loops is a bill rather than a bad
number. When one does fire, every command prints `STOPPED ON BUDGET` rather than
`FAILED` and exits with a sentence that says so, because a suite the operator
bounded and a suite that broke want opposite next actions.

Two properties of the metering are worth knowing before reading a number from
it. **Enforcement is after the call**: the meter sees a request's usage only
once its event lands, so a single runaway call always completes and a two-turn
cap costs three calls — `TestATightBudgetAbortsAndKeepsThePartialRun` pins that
arithmetic rather than papering over it. And **the record and the ceiling are
separate accountants**: `Usage` never refuses anything, the `Meter` never keeps a
breakdown, and folding them together would mean either a record that can abort a
run or a ceiling that has to be asked about the answer. The abort keeps the
partial snapshot, so an operator who set a bound gets what was found up to it.

One known undercount survives all of this, upstream in mast's Anthropic
provider: `cache_creation` tokens are billed at 1x rather than the 1.25x
Anthropic charges, so a cache-warming turn is slightly cheap. It is in the
opposite direction from the flat rate's error and about two orders of magnitude
smaller. It is **not** a mast fork defect — `core-agent`'s provider carries the
identical `totalInput := Input + CacheRead + CacheCreation` and the identical
"KNOWN GAP" comment, and both repos' `pkg/pricing/refresh.go` parses
`cache_creation_input_token_cost` without plumbing it onto `Rates`. Fixing it
means adding a rate field in both.

**And the cached counter has to be clamped, which is the one thing core-agent
had that we did not.** `core-agent/pkg/usage/tracker.go` clamps
`CachedInputTokens` down to `InputTokens` as a guard against providers whose
cached counter over-reports; neither `evals.Tokens.Fresh()` nor mast's
`budget.priceOf` did, and both derive fresh input by subtraction. Unclamped
that goes negative, and `CostUSDWithCache` bills negative fresh input at the
*input* rate — a credit. So the error runs in the direction nobody audits: the
run gets cheaper the more the provider miscounts, and a cost ceiling gets
further away exactly when the data is worst. Both are clamped now
(`Tokens.split`, `Meter.priceOf`), and both tests assert the same bound — a
miscounted prompt costs what a fully-cached prompt costs, never less.

## Divergences from upstream

### The scheduled path is bounded; the agent is what an escalation runs

Decided 2026-08-14, before the scheduler was written, because everything
downstream inherits it. **The bounded pass itself is now built and measured** —
`internal/bounded`, and the bounded-pass baseline above is the delta against the
agent on the same eleven fixtures. The scheduler, the escalation trigger and the
daily floor are still designs; this section is the shape they are to be built
to, and the reasoning is recorded because the decision is only defensible with
the cost numbers next to it.

**Upstream does not run its agent on a cycle, and mast agrees.**
`scheduler.py:run_structured_health_check` is "the canonical health-check
implementation shared by the scheduler and the interactive Slack path": zero-
token collection through the Kubernetes client, then a single forced-tool Haiku
call, chosen because it "performs a fixed number of steps and therefore can
never hit the agent's recursion limit — unlike routing a 'health check' request
through the full Deep Agents orchestrator." `MonitoringScheduler.__init__` takes
an `agent` the canonical check never uses. mast's W4.3 is the same design
("bounded analysis path … their strongest lesson"), with W4.2 making the
collector `lookout health --format=json` at zero model tokens.

**They are right about the cycle and wrong about the agent, and our own numbers
say which is which.** The recorded tier-2 run at 18:24 cost ~$2.40 for ten
fixtures — about $0.24 per two-object namespace — and tier 3 ran 48s to 3m39s
per real namespace at 8 to 91 tool calls. A five-minute cycle is 288 runs a day,
so the full agent on ten namespaces is roughly $690/day and its *latency* alone
does not fit a five-minute interval at the 3m39s end. The bounded pass is one
subagent-tier call over a zero-token collection: order $0.03, one round trip,
and a fixed step count that cannot loop. That gap is not something prompt work
closes.

**But the bounded pass cannot find the class of fault tier 3 exists to
demonstrate.** `prod-checkout` is one Deployment, one pod, `1/1 Running`, twelve
days old, nothing wrong in any status field — and the correct answer is
`critical`, because no Service exists and nothing can reach it. That came from
enumerating a namespace and noticing an *absence*, which a fixed-step snapshot
of object status structurally cannot do. `gemma4-vllm`, by contrast, announces
itself and a bounded pass would have found it.

So: **bounded pass every cycle, full agent on escalation.** Three parts, and the
third is the one that is easy to leave out.

- **Every cycle: the bounded pass**, emitting a `schema.HealthReport` through
  the same contract the agent uses. This is not a convenience — `internal/monitor`
  fingerprints the report, and two producers with two vocabularies would make
  every switch between them look like the whole fault set changing.
- **Escalate to the full agent on what the diff says**: a new fingerprint, or a
  severity increase on an existing one. Noticing that *something* changed is the
  one thing a status snapshot is reliable at, and diagnosing what it was is the
  thing it is worst at, so that is the right seam.
- **And a slow full-agent floor regardless — daily, not on the cycle.** This is
  the part the escalation trigger cannot cover: a namespace that has been broken
  the same way for twelve days produces no transition, so nothing ever fires.
  `prod-checkout` would never have been escalated to. Absence findings do not
  change on a five-minute clock; they change when somebody deploys.

On-demand — Slack, CLI — always runs the full agent. A human asking is not a
cycle.

Two things gate this, and both are already open items rather than new work.

**The escalation trigger is only as good as fingerprint stability — and the
resolution is that it does not use ours.** The paragraph that stood here said to
wire the bounded pass first and the trigger after task #16, because the same
injected fault fingerprinted as `(Deployment, emailservice, ImagePullBackOff)`
on one run and `(Pod, emailservice-…, ImagePullBackOff)` on the next.

That gate is about the *agent's* object choice, and the trigger never sees the
agent's findings. `internal/scheduler` feeds `lookout findings diff` the
**collector's** report, not the model's: lookout derives its reason mechanically
from object status and normalizes the pod-name suffix itself, so the same fault
produces the same subject key every cycle. The bounded-pass run is what settled
it — the model wrote `RolloutIncomplete`, `ExcessiveRestarts` and
`CrashLoopBackOff` for closely related conditions inside a single suite, which
is not a diff key. So #16 still matters for the agent's own reports and is off
the scheduler's critical path.

**Three defects turned up in the first live cycle and none of them was
visible in the design.** Worth recording, because the loop had a full hermetic
test suite before it was ever pointed at a cluster and all three got through it:

- A cluster-scoped transition — a Node, a cert, a webhook — names no namespace,
  and the escalator ran the agent on the namespace `""` for 29 seconds. They
  reach the digest now as `Unscoped` rather than being dropped or escalated: a
  dead node is a real finding that a namespace-scoped agent cannot be sent to
  investigate.
- The floor re-assessed a namespace the trigger had already escalated in the
  same cycle. Two full agent runs, same namespace, same evidence — the same
  waste that makes two transitions in one namespace a single escalation, one
  layer up.
- `lookout health` emits one `kind=health.category` scorecard line per category
  alongside its object findings, and feeding those to the differ stored two
  permanent subjects that identify nothing and reported `ongoing` in every
  digest. They are filtered from the diff input now, and the mandatory summary
  line is kept, because a stream without one is void by lookout's contract.

Cold-start cycle before: 4 transitions, 3 escalations. After: 2 transitions,
1 escalation. Each fix carries a test that names the live cycle as its source.

**Three more turned up the first time a human read the digest in Slack rather
than the log**, which is a different reviewer and caught different things. Same
note applies: the hermetic suite passed throughout.

- The header took its severity from the bounded pass alone, so a cycle whose
  bounded scan said `ok` and whose escalation said `warning` went out headed
  **OK**. `notify.headline` is the max across the whole digest now. When the two
  disagree, the full agent is the one that looked harder.
- The escalation block printed only the identity triple — severity,
  `Kind/name`, reason — which is what `internal/monitor` fingerprints on and
  none of the answer. A $0.25 assessment arrived reading `critical
  Deployment/recommendationservice ImagePullBackOff`, with no cause and no
  remedy, because `Summary`, `Title`, `Detail` and `RecommendedActions` were all
  dropped in rendering. They are rendered now; switchboard chunks a long message
  into ordered in-thread posts, so length was never the objection it looked
  like. **This was not inherited — it is the port's own regression, and upstream
  has the opposite bias.** `slack_notifier.send_structured_report` renders
  `report.summary` as the body block and each finding as
  `*{f.title}*{ns} — {f.detail}`, with `recommended_actions` in their own
  numbered attachment; what it never prints is `kind`, `resource_name` or
  `reason`. So upstream posts the prose and drops the identity triple, and our
  first renderer did exactly the reverse — which is the same mistake twice, since
  a human needs both. The rendering now carries the triple *and* the prose.
- `lastFloor` was an in-process field, and every `sre-monitor -once` is a fresh
  process — so every invocation read a zero, called the floor due, and swept
  every configured namespace at full-agent price whatever the differ had said.
  It persists to `<store>.floor` now. The second cost is the worse one: a demo
  whose floor assesses the namespace on every run can never show the trigger
  *declining* to escalate an unchanged fault, which is the entire argument for
  putting a differ in front of an agent.

The mark goes beside the store rather than in it. The store is lookout's SQLite
database with lookout's schema, migrated by lookout, and a second writer with
its own table in someone else's file is a migration hazard for one timestamp.
Sharing the *path* keeps the property that matters: `rm findings.db` resets
both, which is right, because a fresh store makes every subject new. Every way
of failing to read the mark — absent, garbage, a timestamp in the future —
sweeps, since an unreadable mark costs one sweep and a wrongly trusted one costs
the absence class.

**And the fix for the second of those over-corrected, which the next reading in
Slack found immediately.** Printing the prose meant printing all four prose
fields, and a `HealthReport` restates itself by design: a Summary that
summarises the findings, a Title that names each one, a Detail that argues it at
length, and a RecommendedAction per finding that inverts it. Four layers of the
same content — a four-finding assessment of a namespace whose only faults were
*advisories* ran to forty lines, of which about six were new information. The
reviewer's words were "a bit dense in verbosity" and "lot's of duplication",
which is the opposite complaint to the one before it and the same root cause:
nobody had decided which layer was the answer.

One prose layer per escalation now, and the layer is the **Summary** — the only
field the model writes about the namespace as a whole, and therefore the only
one carrying the causal frame (what broke, what it did to traffic, what still
works). Findings collapse to one line each, the triple plus a Title that adds to
it, because their job in the digest is to index that paragraph against objects
the differ can track. RecommendedActions stay: they are the one thing the
summary does not contain. Detail is dropped, and the fallback is the argument
for the rule — an assessment with no Summary borrows its leading finding's
Detail, because a digest needs *a* paragraph and not three saying the same
thing.

The same rule then applies one level up, and that half was only visible on real
messages. Both producers write a Summary against the same contract, so a cycle
that escalated printed two, and they were never complementary: either the scan
restated the fault the agent had diagnosed better, or — on the floor cycle — a
bounded `ok` sat directly under a `WARNING` header as the first line of body,
because `headline` is the max across the digest and the prose under it was not.
`scanProse` suppresses the bounded summary when an escalation carries prose.
Nothing diffable is lost, since the bounded pass's findings reach the digest as
transitions and never as prose.

Two smaller things came off the same reading, both about a line written for the
wrong reader:

- **`resolved` transitions now say how long the subject was open**, and a cycle
  whose only changes are resolutions is headed `recovered` rather than
  `changes`. The trigger word is the scheduler's own and it describes why the
  cycle ran, not what it found; the fault-cleared cycle went out as
  `INFO — changes`, where even the INFO came from an unrelated control-plane
  advisory. Upstream draws the same distinction in its title (`— Recovered` when
  `not diff.active and diff.resolved`). The severity is deliberately left alone:
  a namespace recovering does not make an unrelated advisory go away.
- **A protest is clipped to its violation clause.** The strings in
  `internal/sre` are rejections addressed to the *model*, written in the
  imperative and mostly remediation only the model can perform —
  `… is missing resource_name — name the object the finding is about and give a
  terse stable condition word; these fields identify the finding across
  monitoring runs …`. Posted verbatim under the heading "Report accepted under
  protest" it read, in the reviewer's words, "a bit off": three lines of
  instructions to a human who cannot act on them, at the bottom of an otherwise
  clean message. Every check writes `<violation> — <instruction>` or
  `<violation>. <instruction>`, so cutting at the first separator keeps the fact
  and drops the imperative, under the heading **Findings not tracked** — which
  is what the protest actually costs the operator. If neither separator is
  present the whole string survives, because an unprinted protest is the one
  outcome that section exists to prevent.

One thing to raise with lookout rather than work around: `findings diff` emits
`subject_key=[REDACTED]` in both wire formats, which looks like its
secret-safety pass firing on a field that is an identifier rather than a value.
`lookout findings ack` takes that key, so an operator ack surface is blocked on
it — the loop keys on (namespace, kind, name, reason) meanwhile.

**And the bounded pass was measurable before it was trusted, which is now done
rather than promised.** The eleven tier-2 fixtures and all four live evaluators
work against any producer of a `HealthReport`, so `sre-eval-live -bounded`
publishes the cost of the bounded path as a delta rather than an argument:
recall 0.517 against 1.000, hallucination 1.000 in both, at 51.6x less per
fixture and single-digit seconds. `fault-badselector` was named in advance as
the fixture it should fail and it failed it, `ok` with no findings.

What the run added to the design is the split inside that recall gap. Only two
of the misses are blindness — `fault-badselector` and `fault-invoicing`, both
absence-class, both the escalation case. The other three are faults it *saw* and
labelled with a controller-level token instead of the pod's own failure mode,
which means the escalation trigger has more signal available to it than the
recall number implies: eight of ten faulty namespaces came back non-`ok`, and
the healthy one came back `ok` with nothing invented.

One thing that does *not* carry over from the eval harness: `-max-cost` and
`-max-turns` default to unlimited there on purpose, because a baseline quietly
bounded reads exactly like an agent that failed to find things. The escalated
run is the operational path AGENTS.md's budget note already points at, and it
gets a ceiling.

### The read path uses lookout's checks, not kubectl wrappers

Upstream's ~40 read tools shell out to `kubectl` and return raw telemetry.
lookout returns compressed, secret-safe logfmt findings with an explicit
summary line, so "cluster healthy" is never ambiguous silence. It also
provides capabilities upstream lacks entirely: blast radius, run-to-run
deltas, time-travel graph queries, drain blockers, net/perf probes.

This means **our tool names differ from upstream's**, which matters for
scoring — see `internal/evals/alias.go`.

lookout does not cover everything. Still to build here, highest value first:

- The audit family (probes, `:latest` tags, pod security, selector mismatch,
  missing limits).
- All Helm tooling.

Two things have come off that list, and both are ours rather than lookout's for
the same kind of reason.

The write path is `internal/kubewrite`. lookout has no write surface and is not
going to grow one, and a mutation that arrives over MCP could not carry the
reviewable `kubectl` line the approval gate is built around.

**Enumeration is `internal/kuberead`,** and it was the highest-value read gap
until it was built. Every detail-returning lookout tool is scoped to one
workload and wants `<Kind>/<namespace>/<name>`; the broad scans
(`k8s_cluster_health`, `k8s_triage_delta`) report only what is *unhealthy*. So
an agent dropped into a namespace whose fault does not show up in pod status
had no way to learn what was in there, and guessed — see the tier-2 baseline.
`k8s_list_resources` is one `kubectl get` across 18 namespaced kinds, rendered
as one line per object. Four properties, each of which a test pins:

- **Every line opens with a target the other tools accept.** The output is not
  a table; it is `<Kind>/<namespace>/<name>` followed by that object's headline
  status, because the whole point is to be the input to the next call.
- **It is an inventory, not a check.** It prints only what `kubectl get <kind>`
  prints in its own default table, and specifically *not* a Service's selector
  — naming the mismatch here would answer `fault-badselector` inside the
  enumeration tool instead of through `k8s_state_edges`, and the fixture would
  stop measuring diagnosis. `TestItDoesNotDiagnose` pins that.
- **No Secret values, ever.** A Secret's line is its type and a key count.
- **A refusal is a `Listing`, not a Go error.** A bad namespace, a kubectl
  failure, a partial result and a truncated one all come back as a result the
  model can read and act on, with the reason in `Error` or `Note`. A tool error
  reads to a model as a malfunction and invites a retry; "that namespace name
  is not a DNS label" is a fact it can use.

It is granted the way everything except writes is granted — through the spec
allowlist, as `server: kuberead` — to the orchestrator and all eight read
specialists, and deliberately not to `change-executor`, which is handed one
specific change with the object already named. The write path's
"a guarantee a config file can revoke is not one" argument does not apply here:
the worst a narrowed allowlist can do is return the agent to guessing, which is
the state it was already in.

Two prompt hazards came out of granting it. Every spec that carries "a result
with no summary line is a failed check" — the orchestrator and `pod-inspector`
— had to be amended, or the agent would discard every listing it ever got. And
the grant is worth nothing without an instruction to *use* it, so each spec now
says to list the namespace first when it was given a namespace rather than a
named object, and that guessing a name is never the move.

### The eval harness is ours

Upstream's `evals/evaluators.py` has three defects, each proven by an
executable test in `internal/evals/upstream_test.go`:

1. `tool_coverage` reads `outputs["expected_trajectory"]`; the dataset writes
   `expected_tools`. It returns a **constant 1.0**.
2. `severity_accuracy` matches `\[(CRITICAL|...)\]`, but 0 of 31 ground-truth
   responses use brackets — they write `"CRITICAL: ..."`. It returns a
   **constant 0**.
3. 7 of 23 distinct `expected_tools` name tools absent from upstream's own
   registry, affecting 13 of 31 examples. Defect 1 hid this: nobody inspects
   an evaluator that always returns 1.0.

Only `response_quality` (an LLM judge) measured anything at all.

We repair 1 and 2 in code, and 3 via `alias.go` rather than by editing the
dataset — the upstream JSONL stays a faithful copy and the repair stays
auditable.

**Any evaluator added here must be calibrated.** `baseline_test.go` asserts
the ground truth self-scores 1.000 (ceiling) and a deliberately useless agent
scores ≤0.25 (floor). An evaluator that fails either bound measures nothing,
which is exactly how upstream's defects survived.

#### `alias.go` maps intents, not names

lookout's `k8s_triage_workload` is explicitly a subsuming check — "one
correlated snapshot ... instead of 4–5 separate reads" — so a 1:1 alias table
marks an agent wrong for using the tool as designed. The mapping is therefore
one-to-many, which raises `tool_coverage`'s practical floor from 0 to **0.414**
(what one reflexive `k8s_triage_workload` call earns across the 31 examples).
`cmd/sre-eval` prints that baseline under every run, and
`TestSubsumptionDoesNotMakeOneToolSufficient` fails if it passes 0.5 — past
that, the evaluator is rewarding a reflex rather than judgment.

Subsumption entries must come from lookout's advertised tool description, not
from intuition about what a check probably returns. Two trims that came out of
reading them: `k8s_triage_workload` does not answer `kubectl_get_events` (its
delta section is derived from object status, not the event stream) nor
`kubectl_get_pods` (it takes a target, so it cannot be the call that discovers
which workload is broken). And `k8s_triage_status` answers *no* read intent at
all — it writes a triage record. An earlier table had it standing in for
`kubectl_describe_pod`, which paid out describe credit for filing a note.

**A single entry passing the one-tool bound is not enough — the credit
compounds.** `k8s_list_resources` is the entry most exposed to this, because it
really is a `kubectl get` and upstream's read surface is largely per-kind list
wrappers, so one call plausibly answers a dozen intents at once. The first
version listed eight, cleared `TestSubsumptionDoesNotMakeOneToolSufficient`
comfortably on its own, and moved a three-call blind fan-out from 0.605 to
**0.842** — against a real agent's measured 0.881. The evaluator had stopped
separating reflex from investigation and no existing test noticed.

Two things came out of that. The rule bounding the entry is that an intent is
earned only where the formatter prints what `kubectl get <kind>` prints, which
keeps out `kubectl_get_nodes` (cluster-scoped), `kubectl_get_resource_quotas`
(the used/hard numbers are the intent, and a listing gives a bare name) and the
two that made the difference: `kubectl_get_services` and `kubectl_get_ingress`
are each also the canonical form of a *describe* intent, because `datasetRepair`
collapses `kubectl_describe_service`/`_ingress` into them and the describe
spelling is the majority of both names' occurrences. Crediting a one-line
inventory with a describe is the `k8s_triage_status` mistake again. Six intents
remain and the fan-out sits at **0.702**.

And the bound itself is now measured across calls, not just per call:
`TestABlindFanOutStillDoesNotScoreLikeAnInvestigation` scores the three broad
discovery calls together and fails past 0.75. Note what caught the erosion in
the first place — running the number, not a failing test. Any new `satisfies`
entry needs the fan-out re-measured, because the per-entry bound will pass.

#### Severity is one-directional in tier 1, and partly ungradeable

Two separate things depress `severity_accuracy`, and they need separate
responses. The direction is a tier-1 observation and has held on every tier-1
run; tier 2 broke it once, on `fault-oomkill`, and that miss is recorded with
its mechanism in the tier-2 baseline above. Read the argument below as an
account of what tier 1 measures, not as a standing property of the agent.

- **The agent over-escalates.** Across 31 examples the misses run 14 too hot
  and **0 too cold**. That one-directionality is why `severity_calibration`
  exists: exact match scores a systematically-one-notch-hot agent the same as a
  confused one, and the fixes are nothing alike. The orchestrator rubric was
  rewritten to grade on current impact rather than on symptom name (upstream's
  lists "OOM kills" and "crash loops" as intrinsically critical), which cut the
  gap but did not close it.
- **Some labels are not inferable.** Examples 4 and 5 are the same fault shape
  — a workload Pending at zero replicas, blocked on a resource — labelled
  WARNING and CRITICAL. The discriminator is that one is a database and the
  other an ETL job, which neither scenario states.
  `TestGroundTruthSeverityDependsOnUnstatedContext` pins the pair. So
  `severity_accuracy` is bounded below 1.0 in practice, and a score short of
  the ceiling is not on its own evidence of miscalibration.

Resist tuning the prompt against these numbers past the point where the change
is defensible on its own terms. The rubric rewrite was worth making because
impact-keyed severity is simply better than symptom-keyed severity; chasing the
remaining delta would be fitting 31 labels, and the score would stop meaning
anything.

### Tier 2 grades findings, not prose

Tier 1 hands the agent the fault in the prompt, so it can only measure
narration and tool selection. Tier 2 breaks a real cluster and says nothing:
every fixture's prompt is `Assess the health of the %q namespace…`, and
`TestPromptsDoNotLeakTheDiagnosis` fails if a fixture ever names its own
symptom. Scoring is on the structured `HealthReport` — the right object, the
right failure mode — because that is the contract, not the paragraph.

Each fixture in `internal/faults` carries a manifest, a **settle condition**,
and the findings a correct agent would report. The settle condition is what
makes the tier trustworthy: `Inject` does not return until the fault has
actually manifested, so a fixture can never quietly measure an agent against a
cluster that had not broken yet. `cmd/sre-eval-live` tracks that failure mode
separately (`InjectError` vs `Error`) and skips rather than scores it — an
uninjected fixture measured nothing about the agent, and averaging it in as a
zero would be worse than dropping it.

#### There is no general precision metric, on purpose

The obvious counterpart to recall is "what fraction of findings were expected",
and it would be the wrong metric here. The fixture manifests are minimal: no
liveness probes, no PodDisruptionBudgets, no resource limits on the
deliberately-broken workloads. An agent that notes those is *correct*, and a
precision score would mark it down for thoroughness — which would create
pressure to prompt the agent into saying less, the opposite of what we want.

So `hallucinated_fault` penalizes exactly one thing: claiming a concrete
failure mode from `failureFamilies` in a namespace where that mode was not
injected. Calling a Pending pod a CrashLoopBackOff is a misdiagnosis; noting
that it also has no resource limits is not. This is what makes the healthy
fixture cost something, and it doubles as a misdiagnosis check on the faulty
ones — `TestMisdiagnosisCountsAsHallucination` and
`TestHealthyFixtureSeparatesAdvicefromInvention` pin both halves.

`Pending`, `Failed`, `Error`, `Unhealthy`, `FailedScheduling` and
`Unschedulable` are deliberately **out** of `failureFamilies`, and are listed in
`genericReasons` rather than merely omitted. They are accepted for recall but
are too generic to attribute to one failure mode, and calling them inventions
would punish an agent for using the vocabulary kubectl handed it.

The set has to be explicit rather than left implicit in absence from
`failureFamilies`, and `familyOf` matches on the exact normalized token. Both
are the same lesson, learned four times. `familyOf` used to classify with
`faults.Want.MatchesReason`, a **bidirectional substring** match, and that is
the right matcher for a fixture's `Want` — where the author lists the spellings
of one answer, and `CrashLoop` and `CrashLoopBackOff` *are* one answer — and the
wrong one for a token the agent chose. A bare family member annexed every longer
token containing it: `Failed` took `FailedMount` for the disk family, `Error`
took `ImagePullError`, `NotReady` took `PodsNotReady`, `DeadlineExceeded` took
`ProgressDeadlineExceeded`. Three of the four cost a live run.

The matcher is exact now, so that whole class is gone rather than patched, and
the "no bare substrings" requirement on `failureFamilies` retired with it. What
did **not** retire is the judgement that put `NotReady` and `DeadlineExceeded`
outside their families to begin with — both fail the assert-a-cause rule on
their own terms, so exact matching is not an invitation to re-add them.

Two things worth knowing before touching this again.

**The change is one-directional in one of its two uses and not the other.**
`HallucinatedFault.Score` calls `familyOf` twice in opposite directions: over
the fixture's own `Want.Reasons` to learn what was injected, and over the
agent's findings. Narrowing the matcher can only *fail to charge* a
misdiagnosis on the second — never invent one, which is the same argument the
`DeadlineExceeded` removal made. On the first it is the reverse: a `Want` reason
that stops resolving drops a family out of `injected`, and then a correct report
of the fixture's own fault is scored as invention, silently.
`TestEveryFixtureStillNamesItsInjectedFamily` pins each fixture's injected
families in a table for exactly that reason — a new fixture fails it until
somebody writes down what it injects.

**It moved nothing, and that was checkable for free.** Re-scored against all
four saved tier-2 transcripts, every aggregate and every below-ceiling cell is
identical to the published figures, and none of the seventeen distinct reason
tokens those runs produced classifies differently under the two matchers. So
this is a hazard removed, not a score changed — which is what `dev/rescore` is
for and the reason it is now a command rather than something rewritten each
time.

The two scheduling tokens each cost a live run to find, and both on
`fault-ledger` — a pod Pending behind a PVC that will never bind. The scheduler
writes `FailedScheduling` and `Unschedulable` for unbound volumes, taints and
node selectors exactly as it writes them for a full node, so neither names a
cause. What is left of that family — `InsufficientCPU` and friends, now keyed
`resource-pressure` — asserts a shortage, which is a claim that can be false.
The general rule that came out of it: **a family may only contain tokens that
assert a cause**, and the way to check a candidate is to ask what else the
control plane writes it for.

The tier-2 evaluators are calibrated the same way tier 1's are, and against one
extra attack: `TestCarpetBombingIsPunished` reports every fixture's fault
against every namespace. That buys recall 1.000, so the test asserts it costs
`hallucinated_fault` (measured 0.180) — otherwise the two numbers together are
still gameable by a single strategy.

`root_cause` is the fourth evaluator and grades only the fixtures whose Wants
mark one finding `Root`. On a cause-and-symptom fixture, recall gives 0.5 to an
agent that reported only the symptom and 0.5 to one that reported only the
cause; those are not the same answer, and the one that names the cause is the
one an operator can act on. It skips a fixture that marks no root, which is nine
of ten — marking the single Want on a single-fault fixture would make it an
exact copy of recall and bury the one fixture where the distinction is real.

## Safety rules

**Fault injection must never touch a real cluster.** This machine has 64
kubectl contexts, including live GKE clusters. The live eval tier must create
its own kind cluster, pin to that context explicitly, and refuse to run
against any context it did not create. Never resolve the ambient
`current-context`.

`internal/kindcluster` makes that mechanical rather than advisory, in four
layers, because a rule enforced only by remembering it is not enforced:

1. **Name prefix.** Every cluster is `sre-eval-*`, and `Delete`/`destroy`
   refuse any name without the prefix. Tested against the real
   `kode-gopher-smoke` and `agent-sandbox-poc` names on this machine.
2. **Own kubeconfig.** The cluster is created into a file we make. `kind
   create cluster --kubeconfig` *merges* into an existing file rather than
   failing, so `prepareKubeconfig` refuses a path that already exists — the
   merge is the hazard, not a convenience.
3. **Explicit `--context` on every call.** `Kubectl` builds `cmd.Env` from
   scratch (`KUBECONFIG`, `PATH`, `HOME`) instead of inheriting, and `runKind`
   drops any ambient `KUBECONFIG`.
4. **Isolation verification.** After creation, the kubeconfig must name our
   context as current *and* describe **exactly one** context. This is the load-
   bearing one: no downstream bug can reach another cluster, because no other
   cluster is described in the only file the child process can read.

`Create` also refuses to adopt a cluster that already exists — reusing one
would mean inheriting state we did not create and deleting something we did not
make.

**Pointing the agent at a real cluster is a different rule, not an exception to
that one.** Tier 3 runs against `simian-test` with admin credentials, because
that is what the cluster has and narrowing them was declined. So read-only is
enforced on our side and nowhere else, and it is enforced four times over:

- **No write tools are built.** `cmd/sre-agent` passes no `Config.Writes`, so
  `change-executor` does not exist in the roster and the orchestrator has no
  delegation target for a mutation.
- **And that is asserted, not assumed.** `refuseWriteTools` enumerates every
  tool on every toolset before the first turn and fails the run on any name in
  `kubewrite.ToolNames()`. It can only fire if a toolset grows a write surface
  underneath us, which is exactly the assumption worth checking at startup
  rather than trusting. It is an exact name match, not a heuristic on the word
  "delete": `k8s_drain_blockers` reads.
- **The tool surface is asked, not assumed either — and this is the one that
  had already gone stale.** The sentence that used to sit in the paragraph above
  read "lookout is a read-only binary", and by the time anyone checked it was
  false: `k8s_findings_diff`, `k8s_findings_ack` and `k8s_triage_status` all
  advertise `readOnlyHint:false`. Nothing broke — all three write the sentinel's
  own SQLite store, named by their `store` argument, and none of them touches a
  Kubernetes object — but nothing here could *tell*, because the name-match
  guard above only knows `kubewrite`'s names and ADK's `mcptoolset` throws MCP
  annotations away when it converts a tool. `lookout.Surface` is a second,
  annotation-preserving handshake with the same pinned kubeconfig, and
  `lookoutWriters` splits the two hazards: an unclassified write is fatal, a
  classified store write is **withheld** from the model and logged. Withholding
  costs this command nothing — all three need a sentinel store a one-shot
  assessment does not have — and it removes the one thing they could do here,
  which is create or overwrite a SQLite file at any path the model names. The
  classification lives in `internal/lookout`'s `storeWriters` and is checked
  against the captured surface by an **ungated** test, so a fourth writer stops
  a read-only run until a human reads its description.
- **The kubeconfig is the pin, so it must describe one cluster.**
  `verifySoleContext` applies kindcluster's layer 4 to a file we did not make,
  and here it is load-bearing rather than belt-and-braces, because the lookout
  subprocess now inherits `HOME` and could in principle find another kubeconfig
  (see the tier-3 notes on `credentialNames`). Both `-kubeconfig` and
  `-context` are required; the ambient `current-context` is never resolved.

None of this makes the *cluster* safe — admin credentials are admin
credentials. It makes our binary unable to use them, which is the part we
control.

One thing that is not a safety property and should not be mistaken for one: a
read-only assessment still sends namespace names, object names, event text and
log excerpts to Vertex. That is inherent to running the agent at all, and it
was accepted explicitly for this cluster.

**A `/dev/shm` OOM fixture will not do what you expect.** The first
`fault-oomkill` filled `/dev/shm` with `dd`, and produced `StartError` /
`RunContainerError` with "container init was OOM-killed" instead of a clean
`OOMKilled`. Kubernetes' `/dev/shm` is scoped to the **pod sandbox**, not the
container, so it survives container restarts: the first OOM left it full and
runc init died before the container ever ran. Allocate anonymous memory instead
(`head -c 400000000 /dev/zero | tail -n 1`), which the kernel reclaims on kill.
The settle condition also has to match the whole `containerStatuses[*]` blob,
because the kill shows up in `state.terminated` while the container is down and
in `lastState.terminated` after it restarts — matching only one of them is a
race.

**A crash loop is not observable as `CrashLoopBackOff` for about 170 seconds,
and the cluster is not up when `kind create` returns.** These two compound, and
together they cost a fixture.

Through the early restarts the container sits in
`state.terminated{reason: Error}` for the whole backoff window and never appears
in `state.waiting` at all — the kubelet restarts it before a status write lands.
`waiting{reason: CrashLoopBackOff}` only becomes observable once the backoff
outlasts a sync, which is the 1m20s step: 10 + 20 + 40 + 80 seconds of backoff
plus the container's own runtime. In front of that sat up to **75 seconds** of
scheduling delay, because `kind create cluster` returns while the control-plane
node still carries its not-ready taint. 75 + 170 overran the four-minute settle
budget, and `fault-sessions` — two replicas, so its evidence lands last — failed
to inject while `fault-crashloop` passed by seconds with the identical latent
race.

The fix is `--wait 120s` on `kind create` (`internal/kindcluster`) plus a settle
budget no tighter than `cmd/sre-eval-live`'s `-settle` default. The fix that was
*not* made is weakening the condition to the kubelet's `BackOff` event, which
shows up in ~25s: that would hand the agent a namespace whose pods still read
`Error`, and every crash-loop Want grades on the `CrashLoopBackOff` token. A
settle condition has to mean "the agent can now observe the fault", not "the
fault has begun".

**All writes are HITL-gated.** Upstream's main agent holds only read tools;
every mutation is delegated to a `change-executor` subagent that interrupts
before each write. Preserve that split — it is a structural guarantee, not a
prompt convention.

Three mechanisms hold it up, each in a different place so that no single edit
can undo it:

1. **The gate is in the executor, not on the tools.** Every tool in
   `internal/kubewrite` routes through `executor.apply`, which raises the
   confirmation itself. There is no ungated path and deliberately no unexported
   variant that skips it. Upstream gates by *listing* the mutating tools
   (`CHANGE_EXECUTOR_INTERRUPT_ON`) and that list is already missing two of its
   own — `kubectl_patch_configmap` and `kubectl_apply_custom_resource` are
   defined and unlisted. A gate you have to remember to extend is a gate that
   eventually is not extended.
2. **Only one agent is handed the tools, and the binding is Go-side.** `Build`
   gives `Config.Writes` to `change-executor` and to nothing else, and skips
   the spec entirely when `Writes` is empty.
   `TestOnlyTheWriteSpecialistIsOfferedWriteTools` asserts this off the
   `LLMRequest` of all nine agents rather than off the config, because the
   declaration surface is what the model acts on.
3. **The human end fails closed.** `internal/approval` denies on a nil
   `Approver`, denies when the `Approver` errors, errors rather than skips on
   an interrupt it cannot answer, and bounds one turn at
   `DefaultMaxRequests` (20) prompts — the person being prompted is the one
   resource in this system that cannot be scaled. `Prompt` approves on `y`/`yes`
   and on nothing else, so a blank line, an unparsed answer, EOF and a closed
   terminal all decline.

What the reviewer is shown is the exact `kubectl` command line, which is the
reason the write path shells out to `kubectl` rather than using client-go: the
executable form and the reviewable form should be the same string. A rendered
patch object is not checkable against what the library will actually send, and
the gate is only as good as what it shows.

Two refusals sit *before* the human, because the failure they guard against is
a reviewer skimming rather than an agent misbehaving.
`DefaultProtectedNamespaces` (ported from upstream) is never the answer to an
incident, so refusing it costs nothing and removes it from the set of things a
tired operator can wave through. `MaxReplicas` (500, not in upstream) is there
because a reviewer recognises the *shape* of `kubectl scale deployment/web -n
prod --replicas=N` and the digits are the part attention skips.

## Conventions

- Comments explain *why*, especially where a decision looks arbitrary but
  encodes a lesson from upstream (e.g. why `OverallSeverity` includes `info`,
  why pod names are normalized against the vowel-free k8s random alphabet).
- Ports of upstream logic carry the upstream test cases, not just new ones.
  Where behaviour must match exactly, prove it differentially against the
  Python implementation rather than by inspection.
