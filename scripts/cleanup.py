#!/usr/bin/env python3
"""Delete explicitly named, empty, retained NLBs after their pool is retired. Dry-run by default."""
import argparse
import json
import sys
import time
from pathlib import Path


def validate(lb, a, shard):
    expected = {"controller": "independent-shared-nlb", "installation": a.installation_id,
                "pool-uid": a.pool_uid, "shard": str(shard)}
    if lb.compartment_id != a.compartment or lb.subnet_id != a.subnet:
        raise ValueError("NLB compartment or subnet mismatch")
    if any((lb.freeform_tags or {}).get(k) != v for k, v in expected.items()):
        raise ValueError("NLB ownership tags mismatch")
    if lb.listeners or lb.backend_sets:
        raise ValueError("NLB is not empty: remove bindings with the running controller first")
    if lb.lifecycle_state != "ACTIVE":
        raise ValueError("NLB must be ACTIVE before deletion; inspect pending operations")


def main():
    p = argparse.ArgumentParser(description=__doc__)
    for key in ("compartment", "subnet", "region", "installation-id", "pool-uid", "retired-ledger"):
        p.add_argument("--" + key, required=True)
    p.add_argument("--nlb-id", action="append", required=True)
    p.add_argument("--profile", default="DEFAULT")
    p.add_argument("--config-file", default="~/.oci/config")
    p.add_argument("--apply", action="store_true")
    p.add_argument("--confirm-pool-retired", action="store_true", help="Confirm pool deleted and cannot reconcile; CRD ledger exported after bindings were removed")
    a = p.parse_args()
    if a.apply and not a.confirm_pool_retired:
        p.error("--apply requires --confirm-pool-retired")
    try:
        import oci
        ledger = json.loads(Path(a.retired_ledger).read_text(encoding="utf-8"))
        if ledger.get("kind") != "NLBPool" or ledger["metadata"]["uid"] != a.pool_uid:
            raise ValueError("ledger must be a single NLBPool with matching UID")
        if ledger["spec"]["compartmentId"] != a.compartment or ledger["spec"]["subnetId"] != a.subnet:
            raise ValueError("ledger scope mismatch")
        status = ledger.get("status", {})
        if status.get("pendingWork") or any(not v.get("tombstone", False) for v in status.get("allocations", {}).values()):
            raise ValueError("ledger contains pending work or live allocations")
        shards = {s["id"]: i for i, s in enumerate(status.get("shards", [])) if s.get("id")}
        if len(set(a.nlb_id)) != len(a.nlb_id) or any(x not in shards for x in a.nlb_id):
            raise ValueError("NLB IDs must be unique and recorded in the retired ledger")
        cfg = oci.config.from_file(str(Path(a.config_file).expanduser()), a.profile)
        cfg["region"] = a.region
        identity = oci.identity.IdentityClient(cfg)
        compartment = identity.get_compartment(a.compartment).data
        print("Scope: tenancy=" + cfg["tenancy"] + " region=" + a.region + " compartment=" + compartment.name)
        client = oci.network_load_balancer.NetworkLoadBalancerClient(cfg)
        # Validate every target before making any deletion request.
        for nlb in a.nlb_id:
            validate(client.get_network_load_balancer(nlb).data, a, shards[nlb])
            print(("DELETE candidate: " if a.apply else "DRY RUN: ") + nlb)
        if not a.apply:
            return
        for nlb in a.nlb_id:
            response = client.get_network_load_balancer(nlb)
            validate(response.data, a, shards[nlb])
            etag = response.headers.get("etag")
            if not etag:
                raise ValueError("missing ETag; refusing unguarded deletion")
            result = client.delete_network_load_balancer(nlb, if_match=etag)
            work = result.headers.get("opc-work-request-id")
            if not work:
                raise ValueError("delete returned no work request; inspect OCI before retrying")
            deadline = time.monotonic() + 600
            while True:
                state = client.get_work_request(work).data.status
                if state == "SUCCEEDED":
                    print("Deleted " + nlb)
                    break
                if state in ("FAILED", "CANCELED") or time.monotonic() >= deadline:
                    raise RuntimeError("inspect work request " + work + ": " + state)
                time.sleep(5)
    except (ValueError, KeyError, OSError, ImportError, RuntimeError) as e:
        print(str(e), file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
