# Contributing

Contributions are welcome through issues and pull requests.

## Development requirements

- Go version pinned by the Dockerfile
- Python 3.10 or newer for tooling tests
- Docker or another compatible OCI image builder for image validation
- An isolated OCI/OKE account only for opt-in integration testing

Before opening a pull request, run:

```sh
go test -count=1 ./...
go vet ./...
python tests/package/test_tools.py
python scripts/collect-notices.py --check
```

Include tests for changes to allocation, ownership, finalization, endpoint selection, or cloud reconciliation. State which networking mode you tested and whether the result is offline, simulated, or live OCI evidence.

## Safety and disclosure

Do not commit credentials, kubeconfigs, WireGuard keys, tenancy or compartment identifiers, public addresses, customer names, raw cloud inventories, or private support material. Use documentation ranges such as `192.0.2.0/24` and obviously invalid OCID placeholders.

Do not run integration or cleanup tools against resources you do not own. Cleanup changes must keep exact compartment, subnet, installation, pool UID, tag, and emptiness checks.

## Compatibility changes

Treat CRD fields, allocation state, listener assignment, finalizers, tags, and status conditions as compatibility-sensitive. Describe migration and rollback for any incompatible change. Do not silently reuse retired ports or reinterpret an existing allocation.

## Legal

By contributing, you agree that your contribution is licensed under Apache License 2.0 and that you have the right to submit it.
