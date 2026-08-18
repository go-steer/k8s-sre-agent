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

# deps.sh — presubmit: the dependency invariants this repo's
# architecture rests on. mast's slim-deps.sh in shape; the denylist
# below is what is specific to us.
#
# 1. k8s-lookout must never be linked. It is consumed by spawning
#    `lookout mcp` and speaking JSON-RPC over stdio, and that boundary
#    is the contract: the agent may use exactly the tools the MCP
#    handshake advertises. A compile-time import would let our code
#    reach around the tool surface the model is scored against, which
#    would quietly invalidate every eval that measures tool coverage.
#    It would also drag client-go, three GCP service clients, the OTel
#    exporters and a cgo-free SQLite into a binary that needs none of
#    them.
#
#    Note for anyone updating the design record: this rule used to be
#    justified by a version split — lookout depended on core-agent and
#    so on ADK v1, while mast is on ADK v2, and linking both majors
#    into one binary was the hazard. That stopped being true at
#    go-steer/k8s-lookout#256 ("own the OTel bootstrap, drop
#    core-agent and ADK"). The rule survives its original reason; the
#    reasons above are the ones that still hold.
#
# 2. ADK v1 must not appear alongside v2. Two majors of the runtime in
#    one binary is two session stores, two event types and two
#    runners — it compiles, and then behaves as though half the agent
#    is talking to a different framework. Cheap to assert, expensive
#    to debug.
#
# These scripts are exactly what CI runs (.github/workflows/ci.yml →
# dev/ci/presubmits/all.sh); run all.sh locally before pushing.

set -euo pipefail
cd "$(dirname "$0")/../../.."

fail=0

# The build graph, not just go.mod: a transitive arrival is the failure
# mode a `require` grep would miss.
deps="$(go list -deps ./... )"

if grep -qx 'github.com/go-steer/k8s-lookout\(/.*\)\?' <<<"${deps}"; then
  echo "FAIL: github.com/go-steer/k8s-lookout is linked into this module." >&2
  grep -x 'github.com/go-steer/k8s-lookout\(/.*\)\?' <<<"${deps}" | sed 's/^/  - /' >&2
  echo "  lookout is a subprocess spoken to over MCP; see the header of this script." >&2
  fail=1
fi

if v1="$(grep -E '^google\.golang\.org/adk(/|$)' <<<"${deps}" | grep -v '^google\.golang\.org/adk/v2' || true)"; [[ -n "${v1}" ]]; then
  echo "FAIL: ADK v1 packages are in the build graph alongside v2:" >&2
  sed 's/^/  - /' <<<"${v1}" >&2
  fail=1
fi

if ((fail)); then
  exit 1
fi
echo "deps: OK — no k8s-lookout, no ADK v1."
