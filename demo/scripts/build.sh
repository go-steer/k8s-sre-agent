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

# Build every binary the demo needs into bin/.
#
# Six binaries from three modules, because the demo is the only place they all
# meet. Four come from this repo and are built from the tree you are sitting
# in. The other two — lookout and switchboard — are separate modules and are
# *installed* at a pinned version by default, so this works on a fresh clone of
# this repo and nothing else. Set LOOKOUT_SRC / SWITCHBOARD_SRC to a checkout
# to build one from source instead; see common.sh for the pins.
#
# lookout stays a separate binary rather than joining the others because it is
# consumed over MCP as a subprocess: the handshake is the contract the evals
# score tool coverage against, and linking it would pull client-go, three GCP
# service clients, Prometheus, OTel and SQLite into a binary that wants to stay
# small. dev/ci/presubmits/deps.sh fails the build if it enters the module
# graph — which `go install pkg@version` does not do, since it resolves in its
# own module context and never touches this one's go.mod.
#
#   ./scripts/build.sh            # everything
#   ./scripts/build.sh sre-monitor lookout

source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

need go "install Go"
mkdir -p "$BIN"

# name → package, all built from this repo.
declare -A LOCAL_TARGETS=(
  [sre-monitor]="./cmd/sre-monitor"
  [sre-agent]="./cmd/sre-agent"
  [sre-eval-live]="./cmd/sre-eval-live"
  [fake-ingress]="./demo/tools/fake-ingress"
)

# name → module path. The package is always <module>/cmd/<name>, and the
# version and optional checkout come from ${NAME}_VERSION / ${NAME}_SRC.
declare -A EXTERNAL_TARGETS=(
  [lookout]="$LOOKOUT_MODULE"
  [switchboard]="$SWITCHBOARD_MODULE"
)

build_local() {
  local name="$1" pkg="${LOCAL_TARGETS[$1]}"
  [[ -d "$SRE_SRC" ]] || die "$name: no checkout at $SRE_SRC (set SRE_SRC)"
  say "building $name from $SRE_SRC"
  # Built from the module's own directory rather than with a path outside the
  # main module, which `go build` refuses.
  ( cd "$SRE_SRC" && go build -o "$BIN/$name" "$pkg" )
}

build_external() {
  local name="$1" module="${EXTERNAL_TARGETS[$1]}"
  local key="${name^^}"
  local srcvar="${key}_SRC" vervar="${key}_VERSION"
  local src="${!srcvar}" version="${!vervar}"

  if [[ -n "$src" ]]; then
    [[ -d "$src" ]] || die "$name: $srcvar is set to $src, which is not a directory"
    say "building $name from $src ($srcvar)"
    ( cd "$src" && go build -o "$BIN/$name" "./cmd/$name" )
    return
  fi

  say "installing $name $version"
  # GOBIN puts it in bin/ alongside the rest. This needs network on a cold
  # module cache; point $srcvar at a checkout to build offline.
  GOBIN="$BIN" go install "$module/cmd/$name@$version" \
    || die "$name: go install $module/cmd/$name@$version failed (offline? set $srcvar to a checkout)"
}

build_one() {
  local name="$1"
  if [[ -n "${LOCAL_TARGETS[$name]:-}" ]]; then
    build_local "$name"
  elif [[ -n "${EXTERNAL_TARGETS[$name]:-}" ]]; then
    build_external "$name"
  else
    die "unknown target $name (have: ${!LOCAL_TARGETS[*]} ${!EXTERNAL_TARGETS[*]})"
  fi
}

targets=("$@")
if [[ ${#targets[@]} -eq 0 ]]; then
  # Sorted so the output is the same every run and a diff of two builds is
  # about the binaries rather than about map order.
  mapfile -t targets < <(printf '%s\n' "${!LOCAL_TARGETS[@]}" "${!EXTERNAL_TARGETS[@]}" | sort)
fi

for t in "${targets[@]}"; do build_one "$t"; done

say "built into $BIN"
ls -la "$BIN" | grep -v '^total\|\.gitkeep\|^d'
