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

# test.sh — presubmit: the whole module's unit tests under the race
# detector, matching mast's bar. -race is not decoration here: the
# monitor's cycle, the eval harnesses' bounded concurrency and the
# lookout subprocess plumbing all run goroutines that the hermetic
# tests exercise, and without -race those tests would pass on a data
# race they were written to catch.
#
# The suite is hermetic by construction — no cluster, no credentials,
# no network — which is why it can be the thing CI runs on every push.
# Anything that needs a model or a cluster lives in cmd/sre-eval* and
# is run deliberately, not here.
#
# These scripts are exactly what CI runs (.github/workflows/ci.yml →
# dev/ci/presubmits/all.sh); run all.sh locally before pushing.

set -euo pipefail
cd "$(dirname "$0")/../../.."

go test -race -timeout 5m ./...
