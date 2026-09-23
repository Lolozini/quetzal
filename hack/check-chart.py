#!/usr/bin/env python3
"""Refuse a rendered chart that Kubernetes would accept but mishandle.

Reads `helm template` output on stdin and exits non-zero when either holds:

- Two containers in a pod declare the same port name. A port name has to be
  unique across the whole pod, not merely within its container: a probe that
  selects a port by name resolves a duplicate to whichever container is listed
  first. Naming the apiserver's metrics port "metrics" -- which the controller
  beside it already used -- silently pointed the controller's liveness probe at
  the apiserver, so a wedged controller would have kept passing its probe.
  Kubernetes only warns about this on apply, which is far too late.

- A namespaced object does not say which namespace it belongs to. Helm fills it
  in on install, but `helm template | kubectl apply` (and `kubectl diff`) put
  such an object in the kubectl context's default namespace instead: on the
  reference cluster that is `plex`, and a diff there reported the panel's
  Deployment as a new object in another application's namespace.
"""
import sys

try:
    import yaml
except ImportError:
    sys.exit("check-chart: PyYAML is required (pip install pyyaml)")

POD_KINDS = {"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}

CLUSTER_SCOPED = {
    "ClusterRole", "ClusterRoleBinding", "Namespace", "PersistentVolume",
    "StorageClass", "CustomResourceDefinition", "PriorityClass",
    "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding",
    "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration",
}


def pod_specs(doc):
    """Yield (description, podSpec) for every pod template in a document."""
    kind = doc.get("kind")
    if kind not in POD_KINDS:
        return
    name = (doc.get("metadata") or {}).get("name", "?")
    spec = doc.get("spec") or {}
    if kind == "CronJob":
        spec = (spec.get("jobTemplate") or {}).get("spec") or {}
    tmpl = (spec.get("template") or {}).get("spec")
    if tmpl:
        yield f"{kind}/{name}", tmpl


def main():
    problems = []
    objects = pods = 0
    for doc in yaml.safe_load_all(sys.stdin):
        if not doc:
            continue
        objects += 1
        meta = doc.get("metadata") or {}
        if doc.get("kind") not in CLUSTER_SCOPED and not meta.get("namespace"):
            problems.append(f"{doc.get('kind')}/{meta.get('name', '?')}: no metadata.namespace "
                            "(kubectl would put it in its context's default)")
        for where, pod in pod_specs(doc):
            pods += 1
            seen = {}
            # initContainers share the namespace with containers.
            for c in list(pod.get("initContainers") or []) + list(pod.get("containers") or []):
                for port in c.get("ports") or []:
                    pname = port.get("name")
                    if pname:
                        seen.setdefault(pname, []).append(f"{c['name']}:{port.get('containerPort')}")
            for pname, owners in sorted(seen.items()):
                if len(owners) > 1:
                    problems.append(f"{where}: port name {pname!r} declared by {', '.join(owners)} "
                                    "(a probe selecting it resolves to the first container)")
    if not objects:
        sys.exit("check-chart: nothing on stdin")
    for p in problems:
        print(p)
    if problems:
        sys.exit(f"check-chart: {len(problems)} problem(s)")
    print(f"check-chart: {objects} object(s), {pods} pod template(s), no problems")


if __name__ == "__main__":
    main()
