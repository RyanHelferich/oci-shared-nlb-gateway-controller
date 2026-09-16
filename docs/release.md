# Release process

## 1. Select a version and source revision

Use semantic versioning. Update the controller build version in the Dockerfile when required and record the exact source revision.

## 2. Run checks

```sh
go test -count=1 ./...
go vet ./...
python tests/package/test_tools.py
python scripts/collect-notices.py --check
python scripts/audit-public.py
```

Review the complete diff and scan the repository for credentials, keys, account/customer identifiers, cloud inventory, and private evidence.

## 3. Build and publish an image

```sh
docker build --pull=false -t oci-shared-nlb-gateway-controller:VERSION .
docker save -o controller-image.tar oci-shared-nlb-gateway-controller:VERSION
sha256sum controller-image.tar
```

Push to an approved registry, then obtain the registry manifest digest. The local Docker image ID is not a registry manifest digest. Deploy by `repository@sha256:digest` and confirm the running pod's image ID.

## 4. Build a deterministic evaluation archive

Create a private `release-input.json` beside the image archive:

```json
{
  "version": "0.1.0",
  "image": "REGISTRY/REPOSITORY@sha256:REPLACE_WITH_64_HEX_DIGEST",
  "imageArchive": "controller-image.tar",
  "imageArchiveSha256": "REPLACE_WITH_64_HEX_ARCHIVE_SHA256",
  "architecture": "linux/amd64",
  "sourceRevision": "REPLACE_WITH_COMMIT_SHA",
  "builtAt": "REPLACE_WITH_UTC_TIMESTAMP",
  "evaluationStatus": "Experimental community release"
}
```

```sh
python scripts/package.py \
  --release release-input.json \
  --output dist/oci-shared-nlb-gateway-controller-0.1.0.zip

python scripts/package.py \
  --verify dist/oci-shared-nlb-gateway-controller-0.1.0.zip \
  --extract clean-room
```

The packager allowlists source, manifests, public documentation, portable scripts, notices, and the exact image archive. It generates per-file checksums and rejects unsafe ZIP paths and likely private material. This scanner is a guardrail; a maintainer must still review the source tree and image contents.

## 5. Release notes

State:

- supported Kubernetes, OKE, CNI, Gateway API, and architecture combinations;
- immutable image digest and archive checksum;
- changes to CRDs, allocation, IAM, health checks, or cleanup;
- completed test environments and measured results;
- unresolved advisories and limitations; and
- upgrade and rollback steps.

Do not describe a lab result as a production SLA.
