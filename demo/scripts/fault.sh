#!/usr/bin/env bash
# Break something in the demo namespace, on purpose, while the loop is running.
#
#   ./scripts/fault.sh inject [name]   # default: badimage
#   ./scripts/fault.sh clear
#   ./scripts/fault.sh list
#
# Each fault waits until it is actually observable before returning. That is
# the same rule this repo's internal/faults fixtures follow and it matters for
# the same reason: a demo that says "broken" while the cluster still looks fine
# gets a correct "healthy" from the next cycle and looks like the agent missed
# it.

source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

# A reference that cannot resolve anywhere. The registry host is real, so the
# failure is a not-found rather than a DNS error — the shape a typo'd tag has.
MISSING_IMAGE="ghcr.io/go-steer/no-such-image:v0.0.0-does-not-exist"

list() {
  cat <<'EOF'
badimage   storefront-web rolled to a tag that does not exist.
           Loud: the pods go ImagePullBackOff and the health scan names them.
           Shows the escalation path — new subject, agent assessment, digest.

noservice  the Service in front of storefront-web is deleted.
           Quiet: every pod stays Running and Ready, and nothing in object
           status is wrong. This is the fault class the bounded pass cannot
           see, so it is the one the daily floor exists to catch — run the
           monitor with -floor to make it visible.

silent     storefront-web keeps running but can no longer reach its database.
           Quiet: 2/2 Ready, zero restarts, clean events. The evidence is only
           in the container's log.
EOF
}

wait_for() {
  local what="$1" tries="${2:-60}"
  say "waiting for $what"
  for _ in $(seq "$tries"); do
    if eval "${3}"; then say "settled: $what"; return 0; fi
    sleep 2
  done
  die "timed out waiting for $what"
}

inject_badimage() {
  kc -n "$DEMO_NS" set image deploy/storefront-web "app=$MISSING_IMAGE" >/dev/null
  wait_for "pods to report an image pull failure" 60 \
    'kc -n "$DEMO_NS" get pods -o jsonpath="{.items[*].status.containerStatuses[*].state.waiting.reason}" 2>/dev/null | grep -q ImagePullBackOff'
}

inject_noservice() {
  kc -n "$DEMO_NS" delete service storefront-web >/dev/null
  wait_for "the Service to be gone while the pods stay healthy" 30 \
    '! kc -n "$DEMO_NS" get service storefront-web >/dev/null 2>&1'
}

inject_silent() {
  # The container produces the failure rather than describing it. Echoing
  # "ERROR: connection refused" on a timer would put the whole diagnosis in
  # spec.containers[].command, where any tool that reads a spec hands it back —
  # internal/faults' fault-invoicing fixture scored a perfect 1.00 that way
  # without ever reading a log, which measured nothing.
  kc -n "$DEMO_NS" patch deploy/storefront-web --type=json -p '[
    {"op":"replace","path":"/spec/template/spec/containers/0/command","value":
      ["sh","-c","echo storefront-web starting; i=0; while true; do i=$((i+1)); echo \"reconcile attempt $i\"; wget -q -T 3 -O /dev/null http://127.0.0.1:5432/healthz || true; sleep 2; done"]}
  ]' >/dev/null
  kc -n "$DEMO_NS" rollout status deploy/storefront-web --timeout=120s >/dev/null
  # --since, not --tail: the condition has to mean "still happening", or a
  # freshly started pod's first errors read as a startup blip that resolved.
  wait_for "the dependency failure to still be happening 25s in" 30 \
    'kc -n "$DEMO_NS" logs -l app=storefront-web --since=25s 2>/dev/null | grep -q "Connection refused"'
}

clear_all() {
  say "restoring $DEMO_NS to healthy"
  kc -n "$DEMO_NS" set image deploy/storefront-web "app=busybox:1.36" >/dev/null
  kc -n "$DEMO_NS" patch deploy/storefront-web --type=json -p '[
    {"op":"replace","path":"/spec/template/spec/containers/0/command","value":
      ["sh","-c","echo serving on :8080; while true; do sleep 5; done"]}
  ]' >/dev/null
  kc -n "$DEMO_NS" apply -f - <<EOF >/dev/null
apiVersion: v1
kind: Service
metadata: {name: storefront-web, namespace: $DEMO_NS}
spec:
  selector: {app: storefront-web}
  ports: [{port: 80, targetPort: 8080}]
EOF
  kc -n "$DEMO_NS" rollout status deploy/storefront-web --timeout=120s
  say "healthy — the next cycle should report the subjects resolved"
}

case "${1:-}" in
  inject)
    case "${2:-badimage}" in
      badimage)  inject_badimage ;;
      noservice) inject_noservice ;;
      silent)    inject_silent ;;
      *)         die "unknown fault ${2}; see: fault.sh list" ;;
    esac
    ;;
  clear) clear_all ;;
  list)  list ;;
  *)     die "usage: fault.sh inject [badimage|noservice|silent] | clear | list" ;;
esac
