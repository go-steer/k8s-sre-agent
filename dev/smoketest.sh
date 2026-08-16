#!/usr/bin/env bash
#
# Manual smoke test for cmd/sre-agent — the read-only tier-3 entry point.
#
# Walks the whole path by hand: preflight, hermetic tests, a minified
# kubeconfig, the three refusals firing where they should, and finally one real
# assessment against a cluster you name.
#
#   dev/smoketest.sh <context> [namespace,namespace,...]
#   dev/smoketest.sh simian-test online-boutique
#
# The refusal checks are the point of running this by hand. They cost nothing,
# need no credentials, and they are the part where a silent regression would be
# invisible in the output of a successful run.
#
# Nothing here mutates a cluster. The only files written are under $WORKDIR.

set -euo pipefail

CONTEXT="${1:-}"
NAMESPACES="${2:-}"
WORKDIR="${SMOKE_WORKDIR:-$HOME/sre-smoke}"
REPEAT="${SMOKE_REPEAT:-1}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ -z "$CONTEXT" ]]; then
	cat >&2 <<'EOF'
usage: dev/smoketest.sh <kube-context> [namespace,namespace,...]

  <kube-context>   the context to assess. Required — this command never
                   resolves the ambient current-context, and neither does the
                   binary it is testing.
  [namespaces]     comma-separated. Defaults to every namespace that currently
                   has at least one pod, capped at three.

env:
  SMOKE_WORKDIR    where the kubeconfig, lookout binary and transcript go
                   (default ~/sre-smoke)
  SMOKE_REPEAT     assessment cycles per namespace, all into one file
                   (default 1). Two cycles against a cluster you have pinned
                   is the only way to separate agent variance from cluster
                   drift — see note 3 at the end.
  SRE_LOOKOUT_BIN  an existing lookout binary; otherwise one is built from
                   ../k8s-lookout
EOF
	exit 2
fi

cd "$REPO"
mkdir -p "$WORKDIR"
umask 077

pass() { printf '  \033[32mok\033[0m   %s\n' "$1"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; exit 1; }
step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

# ---------------------------------------------------------------- preflight

step "Preflight"

command -v go >/dev/null || fail "go is not on PATH"
pass "go $(go version | awk '{print $3}')"

command -v kubectl >/dev/null || fail "kubectl is not on PATH"
pass "kubectl present"

# Vertex. internal/llm reads the project from ANTHROPIC_VERTEX_PROJECT_ID or
# GOOGLE_CLOUD_PROJECT and the region from CLOUD_ML_REGION or
# GOOGLE_CLOUD_LOCATION. Sourced rather than required so this works the way the
# rest of the repo's commands are run. Values are never printed.
if [[ -z "${ANTHROPIC_VERTEX_PROJECT_ID:-}${GOOGLE_CLOUD_PROJECT:-}" && -f "$HOME/scripts/claude-env.sh" ]]; then
	# shellcheck disable=SC1091
	source "$HOME/scripts/claude-env.sh" >/dev/null 2>&1 || true
fi
if [[ -z "${ANTHROPIC_VERTEX_PROJECT_ID:-}${GOOGLE_CLOUD_PROJECT:-}" ]]; then
	fail "no Vertex project in the environment — source ~/scripts/claude-env.sh first"
fi
pass "Vertex project configured"

# lookout is a runtime dependency, spawned over stdio MCP. It is deliberately
# not in go.mod (it would drag ADK v1 into an ADK v2 binary), so it has to be
# built separately or pointed at.
if [[ -n "${SRE_LOOKOUT_BIN:-}" && -x "${SRE_LOOKOUT_BIN}" ]]; then
	pass "lookout: $SRE_LOOKOUT_BIN"
elif [[ -d "$REPO/../k8s-lookout" ]]; then
	echo "  building lookout from ../k8s-lookout ..."
	(cd "$REPO/../k8s-lookout" && go build -o "$WORKDIR/lookout" ./cmd/lookout)
	export SRE_LOOKOUT_BIN="$WORKDIR/lookout"
	pass "lookout built: $SRE_LOOKOUT_BIN"
else
	fail "no lookout binary — set SRE_LOOKOUT_BIN or check out ../k8s-lookout"
fi

# ------------------------------------------------------------------- build

step "Build and hermetic tests"

go build ./... || fail "go build"
pass "go build ./..."

go vet ./... || fail "go vet"
pass "go vet ./..."

# The whole suite, because the guards that make this command safe to point at a
# real cluster are ungated and live here.
go test ./... >"$WORKDIR/test.log" 2>&1 || { cat "$WORKDIR/test.log"; fail "go test"; }
pass "go test ./... (log: $WORKDIR/test.log)"

# ------------------------------------------------------------- kubeconfig

step "Kubeconfig for context '$CONTEXT'"

kubectl config get-contexts -o name | grep -qx "$CONTEXT" \
	|| fail "no context named '$CONTEXT' in your kubeconfig"
pass "context exists"

KUBECONFIG_PIN="$WORKDIR/kubeconfig-$CONTEXT"
# Overwrite rather than merge: `kubectl config view --minify` writes a fresh
# document, unlike `kind create --kubeconfig`, which merges. Regenerating is
# also what you want if the context's credentials were refreshed.
kubectl config view --minify --flatten --context="$CONTEXT" >"$KUBECONFIG_PIN"
pass "wrote $KUBECONFIG_PIN"

# Asked of kubectl rather than grepped: a `  name:` at two spaces is also how a
# cluster entry is written, and the whole question is which block it is in.
got_contexts=$(KUBECONFIG="$KUBECONFIG_PIN" kubectl config get-contexts -o name | wc -l)
[[ "$got_contexts" == "1" ]] || fail "minify produced $got_contexts contexts, expected 1"
pass "describes exactly one context"

# Prove the credentials reach the cluster before blaming the agent for not
# seeing anything. This is the failure that cost the first GKE run: kuberead
# worked and every lookout tool did not, because they build their child
# environments differently.
KUBECONFIG="$KUBECONFIG_PIN" kubectl --context="$CONTEXT" get ns >/dev/null 2>&1 \
	|| fail "kubectl cannot reach the cluster with this kubeconfig"
pass "kubectl reaches the cluster"

env -i KUBECONFIG="$KUBECONFIG_PIN" PATH="$PATH" HOME="$HOME" \
	"$SRE_LOOKOUT_BIN" health --namespace kube-system >/dev/null 2>&1 \
	|| fail "lookout cannot authenticate — check the exec credential plugin on PATH"
pass "lookout authenticates from a filtered environment"

# --------------------------------------------------------------- refusals

step "Refusals (no cluster access needed; each of these must fail)"

expect_refusal() {
	local what="$1"; shift
	local out
	if out=$(go run ./cmd/sre-agent "$@" 2>&1); then
		fail "$what — the command SUCCEEDED and must not have"
	fi
	if [[ -n "${SMOKE_VERBOSE:-}" ]]; then printf '       %s\n' "$out"; fi
	pass "$what"
}

expect_refusal "no -context: refuses to resolve the ambient current-context" \
	-kubeconfig "$KUBECONFIG_PIN" -namespace default
expect_refusal "no -kubeconfig: refuses to fall back to ~/.kube/config" \
	-context "$CONTEXT" -namespace default
expect_refusal "no -namespace: refuses an implicit cluster-wide sweep" \
	-kubeconfig "$KUBECONFIG_PIN" -context "$CONTEXT"

# The load-bearing one, and the reason it gets a purpose-built file rather than
# your real kubeconfig: the check has to fire on the *count*, with -context
# correct and current-context correct, or it is indistinguishable from the pin
# above. This file names two contexts and carries no credentials, so a
# regression here cannot reach anything either way.
SYNTHETIC="$WORKDIR/kubeconfig-two-contexts"
rm -f "$SYNTHETIC"
KUBECONFIG="$SYNTHETIC" kubectl config set-context "$CONTEXT" --cluster=a --user=a >/dev/null
KUBECONFIG="$SYNTHETIC" kubectl config set-context decoy-prod --cluster=b --user=b >/dev/null
KUBECONFIG="$SYNTHETIC" kubectl config use-context "$CONTEXT" >/dev/null
expect_refusal "two-context kubeconfig, correct -context: still refused" \
	-kubeconfig "$SYNTHETIC" -context "$CONTEXT" -namespace default

# ------------------------------------------------------------- assessment

step "Assessment"

if [[ -z "$NAMESPACES" ]]; then
	NAMESPACES=$(KUBECONFIG="$KUBECONFIG_PIN" kubectl --context="$CONTEXT" \
		get pods -A --no-headers 2>/dev/null \
		| awk '{print $1}' | sort -u | head -3 | paste -sd, -)
	[[ -n "$NAMESPACES" ]] || fail "no namespace has any pods; name one explicitly"
	echo "  no namespaces given, using: $NAMESPACES"
fi

# Timestamped, because the script's own closing note tells you to run it twice
# and a fixed name silently destroys the first result when you do. That has now
# happened twice. `latest` is a convenience for "what did I just get"; the
# timestamped files are the record.
OUT="$WORKDIR/assessment-$(date -u +%Y%m%dT%H%M%SZ).json"
echo "  this calls Vertex and takes roughly one to four minutes per namespace"
if [[ "$REPEAT" != "1" ]]; then
	echo "  SMOKE_REPEAT=$REPEAT — $REPEAT cycles per namespace, in one file"
fi
echo

go run ./cmd/sre-agent \
	-kubeconfig "$KUBECONFIG_PIN" \
	-context "$CONTEXT" \
	-namespace "$NAMESPACES" \
	-repeat "$REPEAT" \
	-out "$OUT" \
	-v

ln -sfn "$OUT" "$WORKDIR/assessment-latest.json"

step "What to look at"

cat <<EOF
  transcript   $OUT
               $WORKDIR/assessment-latest.json -> the same file

  There is no ground truth here, so nothing is scored and the only way to read
  the result is to check it. Five things are worth doing by hand:

  1. Verify every finding. Take each '[severity] Kind/name reason=...' line and
     confirm it against kubectl. A finding that is true but whose 'reason' names
     a different layer's failure (Unschedulable on a workload whose ReplicaSet
     says FailedCreate) is the known open defect — it reads correctly and cannot
     be fingerprinted.

  2. Check what it declined to escalate. A Service with no endpoints behind a
     deliberately-zeroed Deployment should come back 'info', not 'critical'.
     Over-escalation is the failure mode both eval tiers measure.

  3. Run two cycles (SMOKE_REPEAT=2) and diff them. Both land in one file, so
     nothing is overwritten. Every checkable fact can be true in both and the
     two can still disagree about what is wrong: on 2026-08-14 one cycle called
     a failing pod the Deployment's current rollout and the next called the
     same pod an orphan from an old ReplicaSet, which inverts the fix. Compare
     the (kind, resource_name, reason) triples too — the same fault reported
     against a Deployment in one cycle and against its pod in the next is two
     fingerprints, and a finding diff would read that as one incident closing
     and another opening.

  4. Break something yourself, leave it broken across both cycles, and restore
     it after. A fault you injected is the only ground truth this tier has, and
     holding it steady is what separates agent variance from cluster drift.

  5. Read the 'STALLED:' suffix if there is one. A specialist that stops without
     reporting no longer kills the run — internal/sre/stall.go degrades it to a
     gap — so a stalled run looks exactly like a complete one except for that
     line, and the area it covered went unchecked. The text kept there is the
     specialist's own last words, which usually name the read-path gap that
     caused it.
EOF
