"""Differential check: upstream Python fingerprint vs our Go port."""
import json, sys, types
sys.path.insert(0, "/home/user/projects/langchain-samples/sre-agent")
from monitor_state import fingerprint, normalize_resource_name

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

json.dump({"norm": cases, "fp": fcases}, open("dev/golden.json","w"), indent=0)
print(f"wrote {len(cases)} normalize cases, {len(fcases)} fingerprint cases")
