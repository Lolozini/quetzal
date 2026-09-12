#!/usr/bin/env python3
"""Refuse a rendered chart whose pod declares the same port name twice.

A container port name has to be unique across the whole pod, not merely within
its container: a probe that selects a port by name resolves a duplicate to
whichever container is listed first. Naming the apiserver's metrics port
"metrics" -- which the controller beside it in the same pod already used --
silently pointed the controller's liveness probe at the apiserver, so a wedged
controller would have kept passing its probe and never been restarted.

Kubernetes warns about this on apply, which is far too late and only if somebody
is reading. Reads `helm template` output on stdin; exits non-zero on a duplicate.
"""
import sys

try:
    import yaml
except ImportError:
    sys.exit("check-chart-ports: PyYAML is required (pip install pyyaml)")

POD_KINDS = {"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"}


def pod_specs(doc):
    """Yield (description, podSpec) for every pod template in a document."""
    kind = doc.get("kind")
    if kind not in POD_KINDS:
        return
    name = doc.get("metadata", {}).get("name", "?")
    spec = doc.get("spec", {})
    if kind == "CronJob":
        spec = spec.get("jobTemplate", {}).get("spec", {})
    tmpl = spec.get("template", {}).get("spec")
    if tmpl:
        yield f"{kind}/{name}", tmpl


def main():
    bad = 0
    checked = 0
    for doc in yaml.safe_load_all(sys.stdin):
        if not doc:
            continue
        for where, pod in pod_specs(doc):
            checked += 1
            seen = {}
            # initContainers share the namespace with containers.
            for c in list(pod.get("initContainers") or []) + list(pod.get("containers") or []):
                for port in c.get("ports") or []:
                    pname = port.get("name")
                    if pname:
                        seen.setdefault(pname, []).append(f"{c['name']}:{port.get('containerPort')}")
            for pname, owners in sorted(seen.items()):
                if len(owners) > 1:
                    print(f"{where}: port name {pname!r} declared by {', '.join(owners)}")
                    bad = 1
    if not checked:
        sys.exit("check-chart-ports: no pod template found on stdin")
    if bad:
        sys.exit("check-chart-ports: a probe selecting one of those names resolves to the first container")
    print(f"check-chart-ports: {checked} pod template(s), no duplicate port names")


if __name__ == "__main__":
    main()
