#!/usr/bin/env python3
"""Render portable JSON manifests; apply only when explicitly requested."""
import argparse
import json
import re
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
NAME = "shared-nlb-controller"


def resource(api, kind, name, namespace=None, **fields):
    metadata = {"name": name}
    if namespace:
        metadata["namespace"] = namespace
    return dict(apiVersion=api, kind=kind, metadata=metadata, **fields)


def render(a):
    release = json.loads(Path(a.release).read_text(encoding="utf-8"))
    image = a.image or release["image"]
    if not re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}", image):
        raise ValueError("image must use an immutable sha256 manifest digest")
    if not re.fullmatch(r"[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?", a.namespace):
        raise ValueError("namespace must be a DNS label of at most 63 characters")
    if not re.fullmatch(r"[A-Za-z0-9._-]{1,64}", a.installation_id):
        raise ValueError("installation-id must contain 1-64 letters, digits, dots, underscores or hyphens")
    for value, prefix in [(a.compartment, "ocid1.compartment."), (a.subnet, "ocid1.subnet.")]:
        if not value.startswith(prefix) or any(c.isspace() for c in value):
            raise ValueError("supply an actual compartment and subnet OCID")
    ns = a.namespace
    cluster_name = "shared-nlb-nodes-" + ns
    rbac_api = "rbac.authorization.k8s.io/v1"
    subject = [{"kind": "ServiceAccount", "name": NAME, "namespace": ns}]
    rules = [
        {"apiGroups": ["nlb.independent.dev"], "resources": ["nlbpools", "tunnelbindings", "nlbpools/status", "tunnelbindings/status", "nlbpools/finalizers", "tunnelbindings/finalizers"], "verbs": ["get", "list", "watch", "update", "patch"]},
        {"apiGroups": [""], "resources": ["services", "pods"], "verbs": ["get", "list", "watch"]},
        {"apiGroups": ["discovery.k8s.io"], "resources": ["endpointslices"], "verbs": ["get", "list", "watch"]},
        {"apiGroups": ["coordination.k8s.io"], "resources": ["leases"], "verbs": ["get", "list", "watch", "create", "update", "patch"]},
        {"apiGroups": [""], "resources": ["events"], "verbs": ["create", "patch"]},
    ]
    gateway_api = getattr(a, "gateway_api", False)
    gateway_class = getattr(a, "gateway_class", "oci-native-udp")
    cluster_rules = [{"apiGroups": [""], "resources": ["nodes"], "verbs": ["get", "list", "watch"]}]
    if gateway_api:
        if not re.fullmatch(r"[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?", gateway_class):
            raise ValueError("gateway-class must be a Kubernetes DNS subdomain")
        rules[0]["resources"] += ["gatewaypools", "gatewaypools/status", "gatewaypools/finalizers"]
        rules[0]["verbs"] += ["create", "delete"]
        rules += [
            {"apiGroups": ["gateway.networking.k8s.io"], "resources": ["gateways", "gateways/status", "gateways/finalizers", "udproutes", "udproutes/status", "udproutes/finalizers"], "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"]},
            {"apiGroups": [""], "resources": ["configmaps"], "verbs": ["get", "create"]},
        ]
        cluster_rules += [{"apiGroups": ["gateway.networking.k8s.io"], "resources": ["gatewayclasses", "gatewayclasses/status"], "resourceNames": [gateway_class], "verbs": ["get", "patch", "update"]}]
    args = ["--namespace=" + ns, "--region=" + a.region, "--compartment=" + a.compartment, "--subnet=" + a.subnet, "--installation-id=" + a.installation_id, "--auth=" + a.auth]
    if gateway_api:
        args += ["--enable-gateway-api", "--gateway-class=" + gateway_class]
    container = {
        "name": "controller", "image": image, "imagePullPolicy": "IfNotPresent", "args": args,
        "env": [{"name": "OCI_RESOURCE_PRINCIPAL_VERSION", "value": "2.2"}, {"name": "OCI_RESOURCE_PRINCIPAL_REGION", "value": a.region}],
        "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]}},
        "resources": {"requests": {"cpu": "100m", "memory": "128Mi"}, "limits": {"cpu": "1", "memory": "512Mi"}},
        "ports": [{"name": "metrics", "containerPort": 8080}, {"name": "health", "containerPort": 8081}],
        "livenessProbe": {"httpGet": {"path": "/healthz", "port": "health"}},
        "readinessProbe": {"httpGet": {"path": "/readyz", "port": "health"}},
    }
    labels = {"app": NAME}
    pod = {"serviceAccountName": NAME, "automountServiceAccountToken": True,
           "dnsConfig": {"options": [{"name": "ndots", "value": "1"}, {"name": "use-vc"}]},
           "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
           "containers": [container],
           "affinity": {"podAntiAffinity": {"preferredDuringSchedulingIgnoredDuringExecution": [{"weight": 100, "podAffinityTerm": {"labelSelector": {"matchLabels": labels}, "topologyKey": "kubernetes.io/hostname"}}]}}}
    if a.pull_secret:
        pod["imagePullSecrets"] = [{"name": a.pull_secret}]
    objects = [resource("v1", "Namespace", ns), resource("v1", "ServiceAccount", NAME, ns),
               resource(rbac_api, "Role", NAME, ns, rules=rules),
               resource(rbac_api, "RoleBinding", NAME, ns, subjects=subject, roleRef={"apiGroup": rbac_api.split('/')[0], "kind": "Role", "name": NAME}),
               resource(rbac_api, "ClusterRole", cluster_name, rules=cluster_rules),
               resource(rbac_api, "ClusterRoleBinding", cluster_name, subjects=subject, roleRef={"apiGroup": rbac_api.split('/')[0], "kind": "ClusterRole", "name": cluster_name}),
               resource("apps/v1", "Deployment", NAME, ns, spec={"replicas": 2, "selector": {"matchLabels": labels}, "template": {"metadata": {"labels": labels}, "spec": pod}})]
    return {"apiVersion": "v1", "kind": "List", "items": objects}


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--release", default=str(ROOT / "release.json"))
    p.add_argument("--image", help="Private registry relocation, still pinned by digest")
    p.add_argument("--namespace", default="shared-nlb")
    p.add_argument("--compartment", required=True)
    p.add_argument("--subnet", required=True)
    p.add_argument("--region", required=True)
    p.add_argument("--installation-id", required=True)
    p.add_argument("--auth", choices=["workload", "instance"], default="workload")
    p.add_argument("--pull-secret")
    p.add_argument("--gateway-api", action=argparse.BooleanOptionalAction, default=True, help="Enable the Gateway API workflow (default); --no-gateway-api preserves the legacy binding interface")
    p.add_argument("--gateway-class", default="oci-native-udp", help="Exact GatewayClass name whose status this installation can manage")
    p.add_argument("--output", default="controller-install.json")
    p.add_argument("--apply", action="store_true")
    p.add_argument("--context", help="Required with --apply; explicit kubectl context")
    a = p.parse_args()
    if a.apply and not a.context:
        p.error("--apply requires an explicit --context")
    try:
        content = render(a)
    except (ValueError, KeyError, OSError) as e:
        p.error(str(e))
    out = Path(a.output)
    out.write_text(json.dumps(content, indent=2) + "\n", encoding="utf-8")
    print("Rendered " + str(out.resolve()))
    if a.apply:
        subprocess.run(["kubectl", "--context", a.context, "apply", "-f", str(out)], check=True)
        subprocess.run(["kubectl", "--context", a.context, "-n", a.namespace, "rollout", "status", "deployment/" + NAME, "--timeout=300s"], check=True)


if __name__ == "__main__":
    main()
