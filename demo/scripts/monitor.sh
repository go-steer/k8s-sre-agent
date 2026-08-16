#!/usr/bin/env bash
# Run the monitoring loop against the demo cluster.
#
#   ./scripts/monitor.sh                 # loop until ^C
#   ./scripts/monitor.sh --once          # one cycle and exit
#   ./scripts/monitor.sh --once --quiet-ok   # force a digest even when nothing changed
#
# Anything after those is passed straight to sre-monitor, so the flags it
# already has (-interval, -floor, -max-escalations, -max-cost) work here.
#
# Chat: this starts a listener and points the monitor at it. Which one depends
# on scripts/slack.env — present, and it is real switchboard posting to your
# channel; absent, and it is tools/fake-ingress printing to the terminal. Set
# INGRESS_URL to use a switchboard you started yourself.

source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

need_bin sre-monitor
need_bin lookout
[[ -f "$KUBECONFIG_PATH" ]] || die "no cluster — run: scripts/cluster.sh up"
[[ -n "${ANTHROPIC_VERTEX_PROJECT_ID:-}${GOOGLE_CLOUD_PROJECT:-}${CLOUD_ML_REGION:-}" ]] ||
  warn "no Vertex environment detected; source your credentials first (the model call will fail otherwise)"

args=()
once=0
quiet_ok=0
for a in "$@"; do
  case "$a" in
    --once)     once=1 ;;
    --quiet-ok) quiet_ok=1 ;;
    *)          args+=("$a") ;;
  esac
done
[[ $once -eq 1 ]] && args+=(-once)
# heartbeat=1 makes every quiet cycle emit a digest. Useful for a demo — you
# get a message without waiting for something to break — and wrong in
# production, where the point of the differ is that a steady state is silent.
[[ $quiet_ok -eq 1 ]] && args+=(-heartbeat 1)

conversation="${SLACK_CONVERSATION:-}"
if ! slack_configured; then
  conversation="${conversation:-C0DEMO}"
fi
[[ -n "$conversation" ]] ||
  die "SLACK_CONVERSATION is unset — set it in $SLACK_ENV to the channel ID (Slack: View channel details, the C… at the bottom), and invite the bot with /invite @switchboard"

url="$INGRESS_URL"
if [[ -z "$url" ]]; then
  # Which listener to start is decided by whether real credentials exist, not
  # by a flag. One switch, one place: drop scripts/slack.env in and the same
  # digest goes to Slack instead of the terminal.
  if slack_configured; then
    say "starting switchboard for real Slack (scripts/slack.env)"
    "$ROOT/scripts/switchboard.sh" &
  else
    need_bin fake-ingress
    say "no Slack credentials: starting the local stand-in on $INGRESS_ADDR"
    FAKE_INGRESS_TOKEN="$INGRESS_TOKEN" "$BIN/fake-ingress" -addr "$INGRESS_ADDR" &
  fi
  ingress_pid=$!
  # Kill it on the way out however we leave, including ^C — an orphaned
  # listener makes the next run fail to bind, which reads as a broken script.
  trap 'kill "$ingress_pid" 2>/dev/null || true' EXIT INT TERM
  url="http://$INGRESS_ADDR"

  # Wait for the port rather than sleeping a fixed second. switchboard has a
  # Slack handshake to do first, and a monitor that posts before the listener
  # is up loses the cycle it was started for.
  for _ in $(seq 40); do
    (exec 3<>"/dev/tcp/${INGRESS_ADDR%:*}/${INGRESS_ADDR##*:}") 2>/dev/null && break
    kill -0 "$ingress_pid" 2>/dev/null || die "the ingress exited during startup — see its log above"
    sleep 0.5
  done
fi

say "monitoring $CLUSTER_NAME; digests to $conversation via $url"
SRE_LOOKOUT_BIN="$BIN/lookout" \
SRE_SWITCHBOARD_TOKEN="$INGRESS_TOKEN" \
"$BIN/sre-monitor" \
  -kubeconfig "$KUBECONFIG_PATH" \
  -context "$KUBE_CONTEXT" \
  -cluster "$CLUSTER_NAME" \
  -store "$STORE_PATH" \
  -namespace "$DEMO_NS" \
  -switchboard-url "$url" \
  -switchboard-conversation "$conversation" \
  "${args[@]}"
