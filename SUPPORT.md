# Support

This repository is an independently maintained community project. Oracle Support and OKE support do not provide support for this controller unless a separate agreement explicitly says otherwise.

Use GitHub issues for reproducible defects and documentation problems. Include:

- controller version and immutable image digest;
- Kubernetes, OKE, CNI, and service-proxy versions;
- selected backend mode;
- sanitized Gateway, UDPRoute, Service, GatewayPool, and condition output;
- relevant controller events and OCI request identifiers; and
- timestamped packet or application probe results.

Remove credentials, keys, kubeconfigs, tenancy/account identifiers, customer names, and private addresses that are not necessary to reproduce the issue.

Operators remain responsible for IAM, network rules, quotas, image distribution, monitoring, backup, upgrades, acceptance testing, and costs. See [docs/operations.md](docs/operations.md).
