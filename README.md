# k8s-sre-agent

[![ci](https://github.com/go-steer/k8s-sre-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/go-steer/k8s-sre-agent/actions/workflows/ci.yml)

An autonomous Kubernetes SRE agent in Go. It watches a cluster, diagnoses what
is wrong in the operator's own terms, and can fix things — but never without a
human approving the exact command first.

> **Early.** The baselines below the fold predate `mast v0.4.0`, which fixed a
> silent defect in how tool parameters reached Claude, so they were measured on
> a substrate that no longer exists. Treat them as history until they are re-run.

## What it does

A cycle runs every few minutes and costs no model tokens: two `k8s-lookout`
scans collect cluster state, and a single small-model call turns them into a
typed `HealthReport`. Findings are fingerprinted, so the interesting event is
not "something is wrong" but "something *changed*" — a fingerprint that was not
there last cycle, or one that has gone away.

A change is what escalates to the full agent: an orchestrator that delegates
across eight read-only specialists — pod, scaling, performance and log
inspection, plus security, reliability, config and job audits — each with its
own allowlist of diagnostic tools. Because a status snapshot structurally cannot
see some failures (a workload that is 2/2 Ready and cannot reach its database),
a slower floor sweeps every namespace with the full agent daily regardless.

Writes are the part worth being careful about. The orchestrator holds no write
tools at all. Every mutation is delegated to a `change-executor` subagent whose
fourteen tools each interrupt before running, showing the exact `kubectl`
command for a human to approve or reject. That specialist is not merely unused
in a monitoring deployment — it is *not built*, so there is no delegation target
for a mutation to route to. The split is structural rather than a matter of
prompt wording, which is the only way it survives a model that decides to be
helpful.

Digests go to Slack through [`switchboard`](https://github.com/go-steer/switchboard),
including the approval prompts.

### How it is put together

Three substrates, and only the SRE domain layer is written here.

| Layer | Source | Role |
| --- | --- | --- |
| Runtime | `google.golang.org/adk/v2` | Runner, Chat/Task agent modes, HITL as a session event, durable sessions |
| Substrate | [`go-steer/mast`](https://github.com/go-steer/mast) | Permissions, providers, specialists, budget, pricing |
| K8s reads | [`go-steer/k8s-lookout`](https://github.com/go-steer/k8s-lookout) | Deterministic, token-dense diagnostics, consumed **over MCP** |
| Domain | this repo | Write tools + the HITL gate, specialist specs, scheduler, evals |

lookout is a *runtime* dependency, not a compile-time one — it is spawned as
`lookout mcp` and spoken to over stdio JSON-RPC. That is not fastidiousness.
The MCP handshake is the contract: the agent may use exactly the tools it
advertises, and every eval that scores tool coverage assumes so. An import
would let this code reach around the tool surface the model is measured
against, and nothing would flag it — so `dev/ci/presubmits/deps.sh` fails the
build if lookout ever enters the module graph.

The nine specialists are configuration (`internal/sre/specs/*.tmpl`), not Go
wiring, so changing what the agent knows how to do is a template edit.

## Try it

The fastest honest look is the demo: a throwaway `kind` cluster, a fault
injected on purpose, the monitor noticing it, and a digest in chat. No real
cluster is touched — the scripts create their own kubeconfig and refuse to
resolve an ambient context.

```sh
demo/scripts/build.sh                       # → demo/bin/
demo/scripts/cluster.sh up                  # throwaway kind cluster, healthy
export GOOGLE_CLOUD_PROJECT=... GOOGLE_CLOUD_LOCATION=...

demo/scripts/monitor.sh --once --quiet-ok   # a cycle on a healthy cluster
demo/scripts/fault.sh inject                # break it
demo/scripts/monitor.sh --once              # → detects, escalates, posts
demo/scripts/fault.sh clear                 # fix it
demo/scripts/monitor.sh --once              # → reports it resolved

demo/scripts/cluster.sh down
```

Roughly $0.50 all in: quiet cycles are fractions of a cent, an escalation is
around $0.25. Slack is optional — without credentials the digest prints to the
terminal via a stand-in that speaks switchboard's contract. Three faults ship,
deliberately different *kinds* of broken; `demo/README.md` has the details and
explains why the two quiet ones are the interesting ones to watch.

## Building

Requires Go 1.26+, and a `lookout` binary for anything that reads a real
cluster.

```sh
GOBIN=/tmp go install github.com/go-steer/k8s-lookout/cmd/lookout@v0.20.0

go build ./...
go test ./...        # hermetic: no cluster, no credentials, no network

dev/ci/presubmits/all.sh   # everything CI runs, ~30s
```

`all.sh` is build, vet, gofmt, golangci-lint, `go mod tidy`, the unit suite
under `-race`, govulncheck, and the dependency invariants — the same scripts
`.github/workflows/ci.yml` runs, so a local pass means a green build. None of
them need a cluster, credentials or a model.

`mast` is an ordinary module requirement. `k8s-lookout` deliberately is not —
it is spawned as a subprocess, never linked, so it is installed as its own
binary rather than resolved by this module's graph. `go install pkg@version`
resolves in its own module context and leaves `go.mod` alone, which is what
lets `dev/ci/presubmits/deps.sh` keep failing the build if lookout ever does
enter the graph.

The `v0.20.0` pin is deliberate: v0.21.0 added a `k8s_list_resources` tool and
`internal/kuberead` still registers one under that name, so the two would
collide on the same agent. The pin moves when that package goes.

Model calls go to Claude on Vertex AI, and need application-default credentials
plus a project and location in the environment — `ANTHROPIC_VERTEX_PROJECT_ID`
or `GOOGLE_CLOUD_PROJECT`, and `CLOUD_ML_REGION` or `GOOGLE_CLOUD_LOCATION`.

## Running it yourself

**One read-only assessment** of a cluster that already exists. Both flags are
required and the kubeconfig must describe exactly one context — nothing here
resolves the ambient current-context, and a kubeconfig with more than one is
refused rather than guessed at.

```sh
kubectl config view --minify --flatten --context=my-cluster > ~/kubeconfig-mine
SRE_LOOKOUT_BIN=/tmp/lookout go run ./cmd/sre-agent \
  -kubeconfig ~/kubeconfig-mine -context my-cluster \
  -namespace my-namespace -out /tmp/assessment.json -v
```

This grants no write tools, so it cannot change anything. It does send namespace
names, object names, event text and log excerpts to the model — worth knowing
before pointing it at production.

**The monitoring loop**, which is the deployed shape: cheap cycles, escalation
on a changed fingerprint, and a daily full-agent floor.

```sh
SRE_LOOKOUT_BIN=/tmp/lookout go run ./cmd/sre-monitor \
  -kubeconfig ~/kubeconfig-mine -context my-cluster \
  -cluster prod-euw1 -store /var/lib/sre/findings.db \
  -namespace my-namespace -interval 5m -floor 24h \
  -switchboard-url http://127.0.0.1:8099 -switchboard-conversation C0123ABCD
```

`-max-cost`, `-max-escalations` and `-max-turns` bound the spend; `-once` runs a
single cycle, for a cron or a smoke test.

## Evals

Behaviour claims about an agent are worth what their measurement is worth, so
scoring is in the repo rather than in a screenshot. Three tiers, on the same
four evaluators:

```sh
# Tier 1 — 31 offline scenarios against a recorded lookout tool surface.
go run ./cmd/sre-eval -out /tmp/eval.json -concurrency 3 -v

# Tier 2 — eleven fault fixtures injected into a kind cluster this creates,
# and deletes on the way out, including on ^C.
SRE_LOOKOUT_BIN=/tmp/lookout go run ./cmd/sre-eval-live -out /tmp/live.json -v

# The same fixtures against the bounded pass, to score the cheap path too.
SRE_LOOKOUT_BIN=/tmp/lookout go run ./cmd/sre-eval-live -bounded -v
```

Every run prints what it cost, per model and per agent. Tier 3 is the
`cmd/sre-agent` invocation above: a real cluster, no ground truth, nothing
scored — it measures what a fixture cannot.

Fault injection never runs against a cluster it did not create. That is enforced
in four independent places, and the guard tests are *not* gated behind Docker —
a safety check that only runs when Docker is up is a safety check that does not
run in CI.

`AGENTS.md` carries the design record: the baselines, what each number means,
and the arguments behind the decisions. Most of it exists because a claim was
checked and turned out to be wrong.

## Acknowledgements

This project was inspired by LangChain's Python
[`sre-agent`](https://github.com/langchain-samples/sre-agent), which is where
the idea and the shape of the specialist roster come from. It is worth reading
on its own terms, and it is the better place to start if you want the concept
rather than this particular set of tradeoffs.

The code here is independently written, not translated. The runtime is ADK v2
rather than LangGraph, so much of what that project builds by hand —
checkpointing, interrupts, call limits — is native and simply absent from this
tree. The reads go through `k8s-lookout`'s deterministic checks instead of
`kubectl` wrappers, which changes what the specialists are even able to ask
for, and the prompts are written against that tool surface. `AGENTS.md`
records where behaviour deliberately differs and why.

One behaviour is worth checking rather than asserting: finding identity, the
fingerprint everything downstream keys on, has to agree with the Python or the
monitoring diff and any cross-implementation comparison quietly go wrong.
`dev/diffcheck.py` fetches that project at a pinned commit, executes its
`monitor_state.py` over 994 inputs, and `internal/monitor/golden_test.go`
replays the answers against this implementation. The vectors are committed, so
the comparison runs on every push rather than when someone remembers, and each
file records the upstream commit, the hash of the file it came from, and its
licence. `sre-agent` is MIT, Copyright (c) LangChain, Inc.

## License

Apache 2.0 — see [LICENSE](./LICENSE).
