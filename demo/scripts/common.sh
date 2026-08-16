#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Shared settings and the cluster pin. Sourced by every script here.
#
# The pin is the important part. These scripts create clusters and break things
# on purpose, and this machine has dozens of kubectl contexts including live
# ones. So nothing here ever resolves the ambient current-context: KUBECONFIG
# points at a file we generated, every kubectl call passes --context
# explicitly, and the cluster name carries a prefix that the teardown checks
# before it deletes anything. Same four layers this repo's
# internal/kindcluster applies for the same reason — a rule enforced only by
# remembering it is not enforced.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin"
RUN="$ROOT/run"

# Where the sources are. The agent is this repo, one level up; the other two
# are sibling checkouts. Override if yours live elsewhere.
SRE_SRC="${SRE_SRC:-$ROOT/..}"
LOOKOUT_SRC="${LOOKOUT_SRC:-$ROOT/../../k8s-lookout}"
SWITCHBOARD_SRC="${SWITCHBOARD_SRC:-$ROOT/../../switchboard}"

# The throwaway cluster. The prefix is load-bearing: cluster.sh down refuses
# any name without it, so a typo cannot delete a cluster somebody needs.
CLUSTER_PREFIX="sre-demo"
CLUSTER_NAME="${CLUSTER_NAME:-${CLUSTER_PREFIX}-local}"
KUBECONFIG_PATH="$RUN/kubeconfig.yaml"
KUBE_CONTEXT="kind-${CLUSTER_NAME}"

# Where the demo's workloads live, and where the finding-diff store goes.
DEMO_NS="${DEMO_NS:-storefront}"
STORE_PATH="$RUN/findings.db"

# Chat. Empty INGRESS_URL makes monitor.sh start something local and point the
# monitor at it — real switchboard if Slack credentials are present, the
# stand-in otherwise. Set INGRESS_URL to talk to a switchboard you started
# yourself.
INGRESS_URL="${INGRESS_URL:-}"
INGRESS_ADDR="${INGRESS_ADDR:-127.0.0.1:8099}"
INGRESS_TOKEN="${INGRESS_TOKEN:-demo-ingress-token}"
SLACK_CONVERSATION="${SLACK_CONVERSATION:-}"

# Real Slack is one file. scripts/slack.env holds the two Slack tokens and the
# channel; it is gitignored, and its presence is the entire switch between the
# stand-in and a live workspace. slack.env.example is the checked-in shape.
SLACK_ENV="${SLACK_ENV:-$ROOT/scripts/slack.env}"
if [[ -f "$SLACK_ENV" ]]; then
  set -a
  # shellcheck disable=SC1090
  . "$SLACK_ENV"
  set +a
fi

# switchboard reads every credential from the environment rather than a flag,
# so nothing here ever puts a token on a command line where it would land in
# the process table.
#
# The daemon token is required and never used: `serve` refuses to start without
# one, but it only contacts core-agent for *inbound* messages and this
# deployment has none. A placeholder is the honest value — see
# go-steer/switchboard#23.
export SWITCHBOARD_DAEMON_TOKEN="${SWITCHBOARD_DAEMON_TOKEN:-unused-outbound-only}"
export SWITCHBOARD_INGRESS_TOKEN="$INGRESS_TOKEN"

# slack_configured reports whether we can reach a real workspace. Both tokens
# are needed even to post: the bot token does the posting, and the app token is
# what `serve` dials Socket Mode with before it will serve the ingress at all.
slack_configured() {
  [[ -n "${SWITCHBOARD_SLACK_BOT_TOKEN:-}" && -n "${SWITCHBOARD_SLACK_APP_TOKEN:-}" ]]
}

say()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m==> %s\033[0m\n' "$*" >&2; }
die()  { printf '\033[1;31m==> %s\033[0m\n' "$*" >&2; exit 1; }

# kc runs kubectl against the demo cluster and nothing else.
#
# Both flags every time, deliberately. --kubeconfig alone would still let
# kubectl fall back to that file's current-context, which is one merge away
# from being something else.
kc() {
  [[ -f "$KUBECONFIG_PATH" ]] || die "no kubeconfig at $KUBECONFIG_PATH — run: scripts/cluster.sh up"
  kubectl --kubeconfig "$KUBECONFIG_PATH" --context "$KUBE_CONTEXT" "$@"
}

need() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is not on PATH ($2)"
}

need_bin() {
  [[ -x "$BIN/$1" ]] || die "$BIN/$1 is missing — run: scripts/build.sh"
}

mkdir -p "$RUN"
