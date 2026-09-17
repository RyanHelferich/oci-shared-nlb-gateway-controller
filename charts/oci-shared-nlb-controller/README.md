# Helm pilot installation

Install Gateway API v1.6.2 first with `scripts/install-gateway-crds.py`. Helm installs the project CRDs from `crds/`, RBAC, two controller replicas, and optionally the `GatewayClass` and a `GatewayPool`.

This chart is an optional pilot interface. The renderer and canonical manifests remain supported. Do not run a Helm-managed and renderer-managed controller for the same namespace and installation ID at the same time.

The chart starts with `gatewayPool.occupancy: 8` so a pilot spreads traffic across more NLBs while throughput is measured. Increase it only after completing the [capacity plan](../../docs/capacity-planning.md); the CRD permits up to 50 and otherwise defaults to 45.

```sh
helm upgrade --install shared-nlb charts/oci-shared-nlb-controller \
  --namespace shared-nlb --create-namespace \
  --set-string image.repository=REGISTRY/oci-shared-nlb-gateway-controller \
  --set-string image.digest=sha256:IMAGE_DIGEST \
  --set-string oci.region=REGION \
  --set-string oci.compartmentId=COMPARTMENT_OCID \
  --set-string oci.subnetId=NLB_SUBNET_OCID \
  --set-string installationId=STABLE_INSTALLATION_ID \
  --set gatewayPool.create=true
```

Keep `installationId`, the namespace, GatewayClass name, and GatewayPool identity stable across upgrades. The OCI workload identity policy must name the rendered ServiceAccount and namespace.

Helm does not upgrade or delete CRDs automatically. Review CRD schema changes and apply them explicitly before a controller upgrade.

The chart passed Helm lint, local rendering, and an OKE API server-side dry run. A server-side dry run validates admission without creating a release. Live adoption of an existing controller requires preserving its namespace, ServiceAccount/IAM identity, installation ID, GatewayClass, and custom-resource state.
