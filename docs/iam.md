# OCI IAM

The controller should authenticate with OKE workload identity. Substitute real values through the organization's secret-free infrastructure workflow:

```text
Allow any-user to manage network-load-balancers in compartment id YOUR_COMPARTMENT_OCID where all {request.principal.type = 'workload', request.principal.namespace = 'YOUR_NAMESPACE', request.principal.service_account = 'shared-nlb-controller', request.principal.cluster_id = 'YOUR_CLUSTER_OCID'}
Allow any-user to use virtual-network-family in compartment id YOUR_COMPARTMENT_OCID where all {request.principal.type = 'workload', request.principal.namespace = 'YOUR_NAMESPACE', request.principal.service_account = 'shared-nlb-controller', request.principal.cluster_id = 'YOUR_CLUSTER_OCID'}
```

What each statement is for:

- `manage network-load-balancers` is the only permission the reconciling controller exercises. Every OCI call it makes is against the Network Load Balancer API (create/get/list NLBs, listeners, backend sets, backends, and work requests).
- `use virtual-network-family` is required because NLB lifecycle operations attach to the subnet, VNICs, and private IPs on the controller's behalf, and because the optional `--check-access-nlb` startup diagnostic calls `GetSubnet` and `GetVcn` directly.
- The controller never calls the Compute API. Worker instance OCIDs come from the Kubernetes `Node.spec.providerID` field, so no `read instances` statement is needed.

NLB lifecycle operations depend on subnet, VNIC, and private-IP attachments. Narrow changes must be verified through a full create, update, worker transition, and delete cycle.

`use virtual-network-family` can permit network changes within the compartment. `manage network-load-balancers` applies to NLBs throughout the compartment. OCI tags are controller ownership evidence, not an IAM boundary. Prefer a dedicated compartment, restrict access to the controller service account, and review the policy through the normal security process.

The controller also supports an instance-principal mode. On shared workers, that identity may be reachable by other workloads; workload identity is normally the clearer boundary.
