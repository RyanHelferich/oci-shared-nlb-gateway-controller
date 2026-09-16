# Third-party notices

The repository includes the unmodified GatewayClass, Gateway, and UDPRoute CRDs from Gateway API v1.6.2 Standard under `deploy/gateway-api-v1.6.2/`. Their Apache License 2.0 text and per-file upstream URL/checksum metadata are included in that directory.

[THIRD_PARTY_NOTICES.json](../THIRD_PARTY_NOTICES.json) records license and notice files selected from the Linux/amd64 controller's pinned Go import graph and the Go toolchain. Identical texts are stored once and referenced by SHA256.

After dependency, module path, Go version, build target, or imported-package changes, regenerate and review the bundle:

```sh
python scripts/collect-notices.py
python scripts/collect-notices.py --check
```

Preserve the root [LICENSE](../LICENSE), [NOTICE](../NOTICE), this file, the Gateway API license, and `THIRD_PARTY_NOTICES.json` with source or binary redistribution.
