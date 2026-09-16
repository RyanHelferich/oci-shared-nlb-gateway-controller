#!/usr/bin/env python3
"""Plan installation of pinned Gateway API CRDs; never overwrite an existing CRD."""
import argparse
import hashlib
import json
import re
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
VENDOR = ROOT / "deploy" / "gateway-api-v1.6.2"


def compatible(existing, wanted):
    """Accept known compatible bundles or an exact v1 schema, without changing them."""
    spec = existing.get("spec", {})
    if spec.get("group") != "gateway.networking.k8s.io" or spec.get("names", {}).get("kind") != wanted["spec"]["names"]["kind"] or spec.get("scope") != wanted["spec"]["scope"]:
        return False
    versions = [v for v in spec.get("versions", []) if v.get("name") == "v1" and v.get("served")]
    if len(versions) != 1:
        return False
    desired = next(v for v in wanted["spec"]["versions"] if v["name"] == "v1")
    if versions[0].get("schema") == desired.get("schema") and versions[0].get("subresources") == desired.get("subresources"):
        return True
    annotations = existing.get("metadata", {}).get("annotations", {})
    version = re.fullmatch(r"v?(\d+)\.(\d+)\.(\d+)", annotations.get("gateway.networking.k8s.io/bundle-version", ""))
    # Later v1.x bundles remain untouched. A future major needs explicit review.
    return bool(version and tuple(map(int, version.groups())) >= (1, 6, 2) and int(version[1]) == 1 and annotations.get("gateway.networking.k8s.io/channel") == "standard")


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--context", required=True)
    p.add_argument("--apply", action="store_true", help="Create missing CRDs only, after reviewing the default plan")
    a = p.parse_args()
    def kubectl(*args):
        return subprocess.check_output(["kubectl", "--context", a.context, *args], text=True)
    metadata = json.loads((VENDOR / "sources.json").read_text(encoding="utf-8"))
    missing = []
    # Check every file and existing schema before making any change.
    for entry in metadata["files"]:
        path = VENDOR / entry["file"]
        if hashlib.sha256(path.read_bytes()).hexdigest() != entry["sha256"]:
            raise ValueError("Pinned source checksum mismatch: " + entry["file"])
        if path.suffix != ".yaml":
            continue
        wanted = json.loads(kubectl("create", "--dry-run=client", "-f", str(path), "-o", "json"))
        name = wanted["metadata"]["name"]
        raw = kubectl("get", "crd", name, "--ignore-not-found", "-o", "json")
        if raw.strip():
            if not compatible(json.loads(raw), wanted):
                raise ValueError(name + ": existing CRD compatibility is unverified; no CRDs changed. Ask its platform owner to review/upgrade it. This tool never downgrades or overwrites CRDs.")
            print("KEEP existing compatible " + name)
        else:
            kubectl("create", "--dry-run=server", "-f", str(path))
            missing.append((name, path))
            print("CREATE missing " + name)
    if not a.apply:
        print("Plan only. Add --apply to create the listed missing CRDs.")
        return
    for name, path in missing:
        # create, not apply: a racing installation cannot be overwritten.
        kubectl("create", "-f", str(path))
        kubectl("wait", "--for=condition=Established", "crd/" + name, "--timeout=60s")
        print("Created " + name)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, OSError, subprocess.CalledProcessError) as e:
        raise SystemExit(str(e))
