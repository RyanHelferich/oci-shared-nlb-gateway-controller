# Security policy

## Supported versions

This project is currently an evaluation release. Security fixes target the latest published release and the default branch.

## Report a vulnerability

Use GitHub's private security advisory feature for this repository. Do not include live credentials, private keys, kubeconfigs, customer data, or full cloud inventories in a report. If a minimal reproducer needs identifiers, replace them with documentation values.

Until a private reporting channel is configured on the public repository, do not open a public issue containing exploit details or secrets.

## Security boundaries

- The controller manages OCI NLB resources in its configured compartment. OCI IAM is the cloud authorization boundary.
- Kubernetes RBAC is namespaced except for the exact GatewayClass status access and worker-node reads required by NodePortCluster mode.
- The controller does not read Kubernetes Secrets or WireGuard keys.
- The controller is outside the UDP data path. Workload authentication remains the tenant-data boundary.
- OCI tags and controller ownership checks reduce accidental crossover; tags are not an IAM boundary.
- Namespace administrators who can run pods as the controller service account can use that workload identity.

Review [docs/iam.md](docs/iam.md), restrict the controller namespace, pin images by digest, and repeat the acceptance suite after cluster, CNI, or controller upgrades.
