# Installation

Start with one non-production route and keep its previous endpoint available for rollback.

## 1. Prepare the cluster and OCI

- Choose a trusted workload namespace and a unique, stable installation ID.
- Create or select the public NLB subnet and confirm quota.
- Apply the reviewed [workload identity IAM policy](iam.md).
- Permit NLB-subnet UDP traffic to the planned NodePort range and TCP 10256 to workers for NodePortCluster.
- For PodIP only, permit NLB-to-pod UDP and workload TCP health and prove direct pod address routing.
- Push the controller image into the approved registry and record its immutable manifest digest.
- For Cilium, finish the [overlay preflight](../Test/02-Cilium-Overlay-OKE-Validation.md#required-configuration).

## 2. Install Gateway API and project CRDs

The repository pins only GatewayClass, Gateway, and UDPRoute from Gateway API v1.6.2 Standard. The helper verifies the bundled files and preserves a compatible newer v1 installation.

```sh
python scripts/install-gateway-crds.py --context YOUR_CONTEXT
python scripts/install-gateway-crds.py --context YOUR_CONTEXT --apply

kubectl --context YOUR_CONTEXT apply --server-side --dry-run=server \
  -f deploy/nlbpools.json \
  -f deploy/tunnelbindings.json \
  -f deploy/gatewaypools.json

kubectl --context YOUR_CONTEXT apply \
  -f deploy/nlbpools.json \
  -f deploy/tunnelbindings.json \
  -f deploy/gatewaypools.json
```

Back up existing CRDs and custom resources before upgrades. Do not downgrade Gateway API through this helper.

## 3. Render the controller

Rendering is local and does not require OCI credentials:

```sh
python scripts/install.py \
  --namespace YOUR_TRUSTED_NAMESPACE \
  --region YOUR_OCI_REGION \
  --compartment YOUR_COMPARTMENT_OCID \
  --subnet YOUR_NLB_SUBNET_OCID \
  --installation-id YOUR_STABLE_INSTALLATION_ID \
  --gateway-class oci-native-udp \
  --image YOUR_REGISTRY/controller@sha256:YOUR_MANIFEST_DIGEST \
  --output controller-install.json

kubectl --context YOUR_CONTEXT apply --server-side --dry-run=server -f controller-install.json
kubectl --context YOUR_CONTEXT apply -f controller-install.json
kubectl --context YOUR_CONTEXT -n YOUR_TRUSTED_NAMESPACE \
  rollout status deployment/shared-nlb-controller --timeout=300s
```

The renderer creates a namespace, ServiceAccount, namespaced Role, exact-name GatewayClass status permission, worker-node read permission, and a two-replica Deployment. It does not create IAM policies, networks, registry credentials, or application workloads. Add `--pull-secret` only for an existing Secret name; the controller itself receives no Secret-read permission.

## 4. Apply a canary

For an overlay cluster, copy [examples/overlay-nodeport.yaml](../examples/overlay-nodeport.yaml). Set the existing namespace, workload selector, and UDP port. The example creates:

- a small `GatewayPool` using 2 slots per NLB;
- a NodePort Service with `externalTrafficPolicy: Cluster`; and
- one automatically allocated `UDPRoute` in `NodePortCluster` mode.

```sh
kubectl --context YOUR_CONTEXT apply --server-side --dry-run=server -f canary.yaml
kubectl --context YOUR_CONTEXT apply -f canary.yaml
kubectl --context YOUR_CONTEXT -n YOUR_TRUSTED_NAMESPACE \
  get gatewaypools,gateways,udproutes,services
```

For directly routable pod networks, [examples/vcn-native-podip.yaml](../examples/vcn-native-podip.yaml) demonstrates PodIP mode and its same-pod TCP health port.

## 5. Publish the endpoint correctly

Wait for current-generation `Accepted`, `ResolvedRefs`, and `Programmed` route conditions. Follow `UDPRoute.spec.parentRefs` to its Gateway and `sectionName`. Publish:

1. the Gateway's `status.addresses` public IP; and
2. the matching listener's `spec.listeners[].port`.

Do not publish the Service port or assume the first Gateway listener belongs to the route.

## 6. Accept or roll back

Run authenticated bidirectional application probes while exercising pod movement, worker replacement, controller restart, and unrelated control routes. Compare recovery with a written target. Keep the former endpoint until the canary passes. See the [VCN-native](../Test/01-VCN-Native-OKE-Validation.md) and [Cilium overlay](../Test/02-Cilium-Overlay-OKE-Validation.md) validation reports plus [operations](operations.md).
