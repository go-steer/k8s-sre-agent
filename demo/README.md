# demo

Scripts for running the SRE agent end to end on a laptop: a throwaway cluster, a
fault you inject on purpose, the monitoring loop noticing it, and a digest
landing in chat.

Nothing here is a library, and nothing here imports the agent. It builds each
binary by shelling `go build` in the module that owns it and drops them all in
one `bin/`, which is the only thing the pieces need in common — `k8s-lookout` is
a subprocess and switchboard is an HTTP service, so neither is a Go dependency
of anything here. That is not incidental for lookout: it depends on `core-agent`
and ADK v1 while this repo is on `mast` and ADK v2, and linking both majors into
one binary is what the consume-lookout-over-MCP rule exists to prevent.

```
..                    the agent, the bounded pass, the scheduler (this repo)
../../k8s-lookout     the read path, consumed as a subprocess
../../switchboard     the chat gateway (optional — see "Chat")
```

The one Go package here, `tools/fake-ingress`, is stdlib-only and belongs to
this repo's module.

## Quick start

```sh
./scripts/build.sh                       # → bin/
./scripts/cluster.sh up                  # throwaway kind cluster, healthy
export GOOGLE_CLOUD_PROJECT=... GOOGLE_CLOUD_LOCATION=...   # Vertex

./scripts/monitor.sh --once --quiet-ok   # a cycle on a healthy cluster
./scripts/fault.sh inject                # break it
./scripts/monitor.sh --once              # → detects, escalates, posts
./scripts/fault.sh clear                 # fix it
./scripts/monitor.sh --once              # → reports it resolved

./scripts/cluster.sh down
```

`monitor.sh` without `--once` loops until `^C`, which is the realistic shape:
leave it running in one terminal and inject a fault in another.

**The floor runs once and then not again for a day**, including across `--once`
invocations — its last-sweep time is recorded in `run/findings.db.floor`, so the
first cycle above sweeps every namespace with the full agent and the later ones
do not. That is what makes the differ visible: cycle 3 above reports the fault
resolved without paying for a fourth assessment. To force a sweep, pass
`-floor 1s`; to start over, `scripts/cluster.sh down` removes the store and its
mark together. `sre-monitor` logs when the floor last swept and when it is next
due, so a cycle that skips it says so.

## What each script is for

| | |
|---|---|
| `build.sh [target…]` | Builds every binary into `bin/`. No args builds all six. |
| `cluster.sh up\|status\|down` | The throwaway kind cluster and a healthy `storefront` namespace. |
| `fault.sh inject [name]\|clear\|list` | Breaks something on purpose, and waits until it is observable. |
| `monitor.sh [--once] [--quiet-ok] [flags…]` | Runs the loop. Extra flags pass through to `sre-monitor`. |
| `switchboard.sh` | Runs switchboard against real Slack in the foreground. `monitor.sh` does this for you. |

## The faults

`fault.sh list` has the details. The three are deliberately different *kinds* of
broken, not three flavours of the same one:

- **badimage** — loud. Pods go `ImagePullBackOff`, the health scan names them,
  and the loop escalates. This is the path a demo wants to show.
- **noservice** — quiet. Every pod stays Running and Ready and nothing in object
  status is wrong; the Service in front is simply gone. The bounded pass cannot
  see this, so it produces no transition and the escalation trigger never fires.
  Run with `-floor 1s` to catch it — that is what the daily floor is for.
- **silent** — quiet. The workload is 2/2 Ready with zero restarts and cannot
  reach its database; the evidence is only in the container's log.

The last two are the interesting ones to demo *after* the first, because they
show what a status-snapshot scan structurally cannot do.

## Chat

**Real Slack is one file.** Copy `scripts/slack.env.example` to
`scripts/slack.env`, fill in the two tokens and the channel ID, and run the demo
exactly as above — `monitor.sh` starts switchboard itself and the digests land in
the channel. Nothing else changes, because nothing else *is* different: the
monitor has one notifier and `slack.env` only decides what it points at.

```sh
cp scripts/slack.env.example scripts/slack.env   # then edit it
./scripts/monitor.sh --once --quiet-ok           # → posts to your channel
```

`slack.env` is gitignored. It is sourced by `common.sh`, so every script here
picks it up, and the tokens reach switchboard through the environment — nothing
puts one on a command line.

**Without it** you need no workspace at all: `monitor.sh` starts
`tools/fake-ingress` instead and prints the digest to the terminal. That
stand-in speaks the same contract switchboard does — same path, same bearer
auth, same `{conversation,id}` response, same 409 — so the monitor cannot tell
the difference and the two paths exercise the same client code.

`scripts/switchboard.sh` runs switchboard in the foreground if you would rather
have it in its own terminal. Worth doing the first time: its startup log is
where a bad token says so.

Three things that will otherwise cost you an hour:

- **The bot must be in the channel.** `/invite @switchboard`, or every post
  comes back `404 no such conversation`.
- **Use the channel ID, not the name** — `C0123ABCD`, from *View channel
  details* at the bottom. Both Slack's API and `--ingress-allow` want the ID.
- **An outbound-only deployment still needs a Socket Mode app token.** `serve`
  dials the WebSocket before it will serve the ingress and exits when that
  connection stops, even though nothing here sends inbound messages. No
  `core-agent` daemon is needed, though — switchboard only contacts it for
  inbound, so `common.sh` supplies a placeholder daemon token. See
  go-steer/switchboard#23.

## Settings

Every one is an env var with a default, in `scripts/common.sh`:

| | |
|---|---|
| `SRE_SRC`, `LOOKOUT_SRC`, `SWITCHBOARD_SRC` | Where the sources are. `SRE_SRC` defaults to this repo, one level up; the other two to sibling checkouts. |
| `CLUSTER_NAME` | Must start with `sre-demo` — `cluster.sh down` refuses anything else. |
| `DEMO_NS` | Namespace the workloads go in. |
| `SLACK_ENV` | Path to the Slack credentials file. Default `scripts/slack.env`; its presence is the real-Slack switch. |
| `INGRESS_URL`, `INGRESS_TOKEN`, `SLACK_CONVERSATION` | Chat. Set `INGRESS_URL` to use a switchboard you started yourself. |

## The cluster pin

These scripts create clusters and break things, and this machine has dozens of
kubectl contexts including live ones. So nothing here resolves the ambient
current-context. `cluster.sh up` writes its own kubeconfig under `run/`, refuses
to merge into an existing one, and verifies the file names our context as
current *and* describes exactly one context. Every `kubectl` goes through `kc`,
which passes `--kubeconfig` and `--context` on every call. `cluster.sh down`
refuses a name without the `sre-demo` prefix.

That is the same four layers `../internal/kindcluster` applies, for the same
reason: a rule enforced only by remembering it is not enforced.

## Cost

A cycle on a quiet cluster is one small-model call — fractions of a cent. An
escalation runs the full agent and is around $0.25, so the demo above is roughly
$0.50 all in. `monitor.sh` passes flags through, so `-max-escalations 1` and
`-max-cost` bound a long-running session.
