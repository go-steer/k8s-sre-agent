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

# Throwaway kind cluster for the demo.
#
#   ./scripts/cluster.sh up      # create it, healthy
#   ./scripts/cluster.sh status  # what is in it
#   ./scripts/cluster.sh down    # delete it
#
# It comes up *healthy* on purpose. The demo is about a monitoring loop
# noticing a change, so the interesting moment is fault.sh breaking something
# while the loop is already running — a cluster that starts broken only ever
# shows the cold-start path, where everything is new and everything escalates.

source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

need kind "https://kind.sigs.k8s.io"
need kubectl "https://kubernetes.io/docs/tasks/tools/"
need docker "the kind node runs in a container"

BASE_IMAGE="busybox:1.36"

up() {
  if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
    die "cluster $CLUSTER_NAME already exists — reuse it, or: scripts/cluster.sh down"
  fi
  # Refusing to merge into an existing file. `kind create --kubeconfig` merges
  # rather than failing, and a merged file describes more than one cluster —
  # which is exactly the pin these scripts depend on.
  [[ -e "$KUBECONFIG_PATH" ]] && die "$KUBECONFIG_PATH already exists; remove it or run: scripts/cluster.sh down"

  say "creating $CLUSTER_NAME"
  # --wait because kind returns while the control-plane node still carries its
  # not-ready taint, and up to 75s of scheduling delay in front of a workload
  # is enough to make the first cycle measure a cluster that has not settled.
  kind create cluster --name "$CLUSTER_NAME" --kubeconfig "$KUBECONFIG_PATH" --wait 120s

  verify_pin
  say "preloading $BASE_IMAGE so the demo does not depend on a registry"
  docker pull -q "$BASE_IMAGE" >/dev/null
  kind load docker-image "$BASE_IMAGE" --name "$CLUSTER_NAME" >/dev/null

  say "deploying a healthy $DEMO_NS"
  kc create namespace "$DEMO_NS" >/dev/null
  kc apply -n "$DEMO_NS" -f - <<EOF >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: storefront-web}
spec:
  replicas: 2
  selector: {matchLabels: {app: storefront-web}}
  template:
    metadata: {labels: {app: storefront-web}}
    spec:
      containers:
      - name: app
        image: $BASE_IMAGE
        imagePullPolicy: IfNotPresent
        command: ["sh","-c","echo serving on :8080; while true; do sleep 5; done"]
        resources:
          requests: {cpu: 10m, memory: 32Mi}
          limits: {cpu: 200m, memory: 64Mi}
---
apiVersion: v1
kind: Service
metadata: {name: storefront-web}
spec:
  selector: {app: storefront-web}
  ports: [{port: 80, targetPort: 8080}]
EOF
  kc -n "$DEMO_NS" rollout status deploy/storefront-web --timeout=120s

  say "ready — healthy. Break it with: scripts/fault.sh inject"
}

# verify_pin is kindcluster's isolation check, applied here.
#
# The file must name our context as current *and* describe exactly one context.
# The second half is what matters for the lookout subprocess: it resolves its
# cluster from KUBECONFIG's current-context with no per-call flag, so for that
# child the file is the only pin there is.
verify_pin() {
  local current names
  current="$(kubectl --kubeconfig "$KUBECONFIG_PATH" config current-context)"
  [[ "$current" == "$KUBE_CONTEXT" ]] || die "kubeconfig current-context is $current, want $KUBE_CONTEXT"
  names="$(kubectl --kubeconfig "$KUBECONFIG_PATH" config get-contexts -o name | wc -l)"
  [[ "$names" -eq 1 ]] || die "kubeconfig describes $names contexts; the demo requires exactly one"
  say "pinned to $KUBE_CONTEXT (sole context in $KUBECONFIG_PATH)"
}

status() {
  kc get all -n "$DEMO_NS" 2>/dev/null || die "no $DEMO_NS namespace — run: scripts/cluster.sh up"
}

down() {
  # The prefix check is the teardown guard. CLUSTER_NAME is overridable, and a
  # delete is the one operation here that cannot be undone.
  [[ "$CLUSTER_NAME" == "$CLUSTER_PREFIX"* ]] || die "refusing to delete $CLUSTER_NAME: not a $CLUSTER_PREFIX-* cluster"
  say "deleting $CLUSTER_NAME"
  kind delete cluster --name "$CLUSTER_NAME"
  # The store's sidecars go with it. sre-monitor keeps the floor's last-sweep
  # timestamp in "$STORE_PATH.floor", and leaving it behind would make the next
  # cluster's first cycle think it had already swept a cluster that no longer
  # exists — the floor is the only path for the absence class, so a stale mark
  # silently costs a whole category of fault.
  rm -f "$KUBECONFIG_PATH" "$STORE_PATH" "$STORE_PATH".*
  say "gone (kubeconfig and finding store removed too)"
}

case "${1:-}" in
  up)     up ;;
  status) status ;;
  down)   down ;;
  *)      die "usage: cluster.sh up|status|down" ;;
esac
