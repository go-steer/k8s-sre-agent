#!/usr/bin/env bash
# Build every binary the demo needs into bin/.
#
# Six binaries from three modules, because the demo is the only place they all
# meet. Three come from this repo, one from switchboard, and one from
# k8s-lookout — and that last one cannot join the others: k8s-lookout depends on
# core-agent, which is on ADK v1, while this repo is on mast and ADK v2, and
# linking both majors into one binary is exactly what the "consume lookout over
# MCP" rule exists to prevent. Separate modules, separate builds, one bin/
# directory.
#
#   ./scripts/build.sh            # everything
#   ./scripts/build.sh sre-monitor lookout
#
# Set SRE_SRC / LOOKOUT_SRC / SWITCHBOARD_SRC if your checkouts are elsewhere.

source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

need go "install Go"

# name → "source-dir::package"
declare -A TARGETS=(
  [sre-monitor]="$SRE_SRC::./cmd/sre-monitor"
  [sre-agent]="$SRE_SRC::./cmd/sre-agent"
  [sre-eval-live]="$SRE_SRC::./cmd/sre-eval-live"
  [lookout]="$LOOKOUT_SRC::./cmd/lookout"
  [switchboard]="$SWITCHBOARD_SRC::./cmd/switchboard"
  [fake-ingress]="$SRE_SRC::./demo/tools/fake-ingress"
)

build_one() {
  local name="$1" spec="${TARGETS[$1]:-}"
  [[ -n "$spec" ]] || die "unknown target $name (have: ${!TARGETS[*]})"
  local dir="${spec%%::*}" pkg="${spec##*::}"
  [[ -d "$dir" ]] || die "$name: no checkout at $dir (set the *_SRC env var)"

  say "building $name from $dir"
  # Built from each module's own directory rather than with a path outside the
  # main module, which `go build` refuses.
  ( cd "$dir" && go build -o "$BIN/$name" "$pkg" )
}

targets=("$@")
if [[ ${#targets[@]} -eq 0 ]]; then
  # Sorted so the output is the same every run and a diff of two builds is
  # about the binaries rather than about map order.
  mapfile -t targets < <(printf '%s\n' "${!TARGETS[@]}" | sort)
fi

for t in "${targets[@]}"; do build_one "$t"; done

say "built into $BIN"
ls -la "$BIN" | grep -v '^total\|\.gitkeep\|^d'
