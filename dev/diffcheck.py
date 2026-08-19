"""Differential check: upstream Python fingerprint vs our Go port.

Executes upstream's monitor_state.py over a grid of inputs and writes the
answers to internal/monitor/testdata/golden.json, which TestGoldenAgainstPython
replays against the Go implementations. A mismatch there means the two disagree
about finding identity — invisible at runtime, and corrupting to both the
monitoring diff and any eval scored across the two.

By default it fetches upstream itself into a temporary directory and removes it
afterwards, so this needs no local checkout:

    python3 dev/diffcheck.py

monitor_state.py imports only the standard library, which is why that works
with no venv and nothing installed.

The fetch is pinned to UPSTREAM_COMMIT rather than tracking main, and both the
commit and the blob hash of monitor_state.py are recorded in the output. That
pin is the point of the whole exercise. Regenerating against whatever upstream
happens to be that day would move the goalposts and still pass: the vectors are
evidence of matching a *specific* upstream, so which one has to be written
down. To adopt a newer upstream, bump the two constants below and regenerate,
and the diff shows what changed.

The generated file is committed, so the comparison runs in CI on every push
rather than only when someone remembers. It was briefly untracked, while
upstream published no licence and output produced by running their code was
the one artifact here genuinely derived from that project; they added MIT in
langchain-samples/sre-agent#15, which settles it. The vectors carry their
provenance and that licence in the "source" block they are written with.

    python3 dev/diffcheck.py --ref main --allow-drift   # see what moved first
    python3 dev/diffcheck.py --src ~/projects/langchain-samples/sre-agent

--src reads a checkout you already have instead of fetching, for working
offline. It is still checked against the pinned blob hash, so an unpinned or
locally-modified copy is refused rather than quietly re-baselining the vectors.
"""

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

import argparse
import hashlib
import json
import os
import subprocess
import sys
import tempfile
import types

UPSTREAM_REPO = "https://github.com/langchain-samples/sre-agent"

# Upstream main as of 2026-08-19, the first commit carrying their LICENSE.
# monitor_state.py itself last changed in d593249 ("Add durable state, stateful
# monitoring, and utilization telemetry", 2026-08-04) and is byte-identical to
# the previous pin at 68d3514, so moving the tree pin here changed the metadata
# and not one vector. The tree pin is what gets fetched, the blob hash is what
# is actually verified.
UPSTREAM_COMMIT = "a03bebec0c5ea90b16cee7d1ab4bfe4425895c60"
MONITOR_STATE_BLOB = "d69b09e4c464b86f74f5ef592c68f9d4e3fc995d"

# Recorded in every generated file. Upstream was unlicensed until 2026-08-19;
# stamping the terms the vectors were produced under means a reader does not
# have to date the file to know what applies to it.
UPSTREAM_LICENSE = "MIT (Copyright (c) LangChain, Inc.)"

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_OUT = os.path.join(REPO_ROOT, "internal", "monitor", "testdata", "golden.json")


def git_blob_sha(path):
    """The hash git would give this file, computed without needing git."""
    data = open(path, "rb").read()
    return hashlib.sha1(b"blob %d\0" % len(data) + data).hexdigest()


def fetch_upstream(dest, ref):
    """Shallow-fetch one commit of upstream into dest. GitHub serves an exact
    sha here, so this stays a single-commit download rather than a full clone."""
    run = lambda *a: subprocess.run(a, check=True, capture_output=True, text=True)
    try:
        run("git", "init", "-q", dest)
        run("git", "-C", dest, "remote", "add", "origin", UPSTREAM_REPO)
        run("git", "-C", dest, "fetch", "-q", "--depth", "1", "origin", ref)
        run("git", "-C", dest, "checkout", "-q", "FETCH_HEAD")
    except subprocess.CalledProcessError as e:
        sys.exit(f"fetching {UPSTREAM_REPO}@{ref} failed:\n{e.stderr.strip()}\n"
                 f"(offline? pass --src /path/to/a/checkout)")
    return subprocess.run(["git", "-C", dest, "rev-parse", "HEAD"],
                          check=True, capture_output=True, text=True).stdout.strip()


def load_upstream(src):
    """Import monitor_state from src, after checking it is the pinned file."""
    module = os.path.join(src, "monitor_state.py")
    if not os.path.exists(module):
        sys.exit(f"no monitor_state.py under {src}")
    return module, git_blob_sha(module)


def build_vectors(fingerprint, normalize_resource_name):
    names = [
        "nginx-deployment-6b474476c4-6nqxr","coredns-5d78c9869d-vwq2t","log-shipper-4tzvn",
        "web-0","postgres-2","redis","redis-cache","shard-10001","api-6b474476c4",
        "ip-10-0-1-23.ec2.internal","","a-bcdfg","x-2456789-bcdfg","kube-proxy-xk2p1",
        "my-app-7d8f9c-xkp2v","frontend-v2-abc12","a-b-c-d-vwxzq","etcd-0","zzz-99999",
        "svc-bcdfghjklm-qrstv","dash-board-2456f",
    ]
    kinds = ["Pod","pods","Deployment","Node","StatefulSet",""]
    cases = []
    for k in kinds:
        for n in names:
            cases.append({"kind": k, "name": n, "norm": normalize_resource_name(k, n)})

    fcases = []
    for k in kinds:
        for n in names[:12]:
            for ns in ["prod","","Staging"]:
                for reason in ["CrashLoopBackOff","OOM Killed!","",'weird__reason--x']:
                    f = types.SimpleNamespace(namespace=ns, kind=k, resource_name=n,
                                              reason=reason, title="Some Title Here")
                    fcases.append({"namespace":ns,"kind":k,"resource_name":n,"reason":reason,
                                   "title":"Some Title Here","fp":fingerprint(f)})
    # a few with no identity fields at all
    for t in ["Cluster has no NetworkPolicies","  Odd   Title!! ","",'ALL CAPS 123']:
        f = types.SimpleNamespace(namespace="", kind="", resource_name="", reason="", title=t)
        fcases.append({"namespace":"","kind":"","resource_name":"","reason":"","title":t,"fp":fingerprint(f)})
    return cases, fcases


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--src", metavar="DIR",
                    help="use this upstream checkout instead of fetching")
    ap.add_argument("--ref", default=UPSTREAM_COMMIT,
                    help="upstream commit or branch to fetch (default: the pin)")
    ap.add_argument("--out", default=DEFAULT_OUT, help=f"output path (default: {DEFAULT_OUT})")
    ap.add_argument("--allow-drift", action="store_true",
                    help="proceed even if monitor_state.py is not the pinned blob")
    args = ap.parse_args()

    with tempfile.TemporaryDirectory(prefix="diffcheck-") as tmp:
        if args.src:
            src, commit = os.path.expanduser(args.src), None
        else:
            commit = fetch_upstream(tmp, args.ref)
            src = tmp
        module, blob = load_upstream(src)

        if blob != MONITOR_STATE_BLOB:
            where = commit or src
            msg = (f"monitor_state.py at {where} hashes to {blob},\n"
                   f"but the vectors are pinned to {MONITOR_STATE_BLOB}.")
            if not args.allow_drift:
                sys.exit(f"{msg}\nUpstream moved, or this checkout is modified. Compare the two,\n"
                         f"then re-run with --allow-drift and bump the constants in this file\n"
                         f"in the same commit as the regenerated vectors.")
            print(f"warning: {msg}\nproceeding because --allow-drift was given", file=sys.stderr)

        # Don't leave a __pycache__ behind in somebody's --src checkout.
        sys.dont_write_bytecode = True
        sys.path.insert(0, src)
        from monitor_state import fingerprint, normalize_resource_name
        cases, fcases = build_vectors(fingerprint, normalize_resource_name)

    # No timestamp on purpose: two runs against the same upstream must produce
    # the same bytes, or every regeneration is a diff about nothing.
    out = {
        "source": {
            "repo": UPSTREAM_REPO,
            "commit": commit or f"(unfetched, --src {src})",
            "monitor_state_blob": blob,
            "license": UPSTREAM_LICENSE,
            "generated_by": "dev/diffcheck.py",
        },
        "norm": cases,
        "fp": fcases,
    }
    os.makedirs(os.path.dirname(args.out), exist_ok=True)
    with open(args.out, "w") as fh:
        json.dump(out, fh, indent=0)
    print(f"wrote {len(cases)} normalize cases, {len(fcases)} fingerprint cases to {args.out}")
    print(f"  from {UPSTREAM_REPO} blob {blob[:12]}"
          + (f" @ {commit[:12]}" if commit else ""))


if __name__ == "__main__":
    main()
