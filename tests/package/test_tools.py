#!/usr/bin/env python3
"""Offline checks for installation isolation and package/cleanup guardrails."""
import argparse
import importlib.util
import json
import tempfile
import unittest
import warnings
import zipfile
from pathlib import Path
from types import SimpleNamespace


ROOT = Path(__file__).resolve().parents[2]


def module(name):
    spec = importlib.util.spec_from_file_location(name, ROOT / "scripts" / (name + ".py"))
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


install, package, cleanup = (module(n) for n in ["install", "package", "cleanup"])
gateway_crds = module("install-gateway-crds")
notices = module("collect-notices")


class ToolsTests(unittest.TestCase):
    def test_notices_select_only_imported_module_ancestry(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d);dep=root/'dep';stdlib=root/'go';(dep/'imported'/'leaf').mkdir(parents=True);(dep/'unrelated').mkdir();stdlib.mkdir()
            (dep/'LICENSE').write_text('selected license');(dep/'imported'/'NOTICE').write_text('selected notice');(dep/'unrelated'/'LICENSE').write_text('unrelated must stay out');(stdlib/'LICENSE').write_text('Go license')
            graph=[{'ImportPath':'example.org/dep/imported/leaf','Dir':str(dep/'imported'/'leaf'),'Module':{'Path':'example.org/dep','Version':'v1.0.0','Dir':str(dep)}}, {'ImportPath':'runtime','Dir':str(stdlib),'Standard':True}]
            bundle=notices.collect(graph,stdlib,'1.26.8',{})
            self.assertEqual(len(bundle['components']),2)
            serialized=json.dumps(bundle);self.assertNotIn('unrelated must stay out',serialized);self.assertNotIn(str(root),serialized)
            self.assertEqual({x['path'] for x in bundle['components'][0]['notices']},{'LICENSE','imported/NOTICE'})
            for sha,value in bundle['texts'].items():self.assertEqual(sha,notices.digest(value['text'].encode()))
            graph[0]['Module']['Replace']={'Dir':str(dep)}
            with self.assertRaisesRegex(ValueError,'Replaced dependency'):notices.collect(graph,stdlib,'1.26.8',{})

    def test_notices_are_explicitly_packaged_and_match_pins(self):
        self.assertIn('THIRD_PARTY_NOTICES.json',package.TOP_FILES);self.assertIn('collect-notices.py',package.SCRIPTS)
        root=ROOT;bundle=json.loads((root/'THIRD_PARTY_NOTICES.json').read_text())
        for name,sha in bundle['sourceInputs'].items():self.assertEqual(notices.digest((root/name).read_bytes()),sha)
        self.assertEqual(bundle['target'],{'GOOS':'linux','GOARCH':'amd64','CGO_ENABLED':'0','entrypoint':'./cmd/controller'})
        self.assertTrue(any(x['module']=='go' and x['version']==bundle['goVersion'] for x in bundle['components']))
        for component in bundle['components']:
            for entry in component['notices']:
                self.assertTrue(package.safe_name(entry['path']))
                self.assertEqual(entry['sha256'],notices.digest(bundle['texts'][entry['sha256']]['text'].encode()))

    def test_install_namespace_isolation_and_no_secrets(self):
        with tempfile.TemporaryDirectory() as d:
            release = Path(d) / "release.json"
            release.write_text(json.dumps({"image": "example.invalid/controller@sha256:" + "a" * 64}))
            a = argparse.Namespace(release=str(release), image=None, namespace="isolated-test", installation_id="installation", compartment="ocid1.compartment.test", subnet="ocid1.subnet.test", region="test-region", auth="workload", pull_secret=None)
            objects = install.render(a)["items"]
            for obj in objects:
                self.assertNotEqual(obj["kind"], "CustomResourceDefinition")
                if obj["kind"] in ["ClusterRole", "ClusterRoleBinding"]:
                    self.assertIn(a.namespace, obj["metadata"]["name"])
                elif obj["kind"] != "Namespace":
                    self.assertEqual(obj["metadata"]["namespace"], a.namespace)
                for rule in obj.get("rules", []):
                    self.assertNotIn("secrets", rule["resources"])
            pod = next(o for o in objects if o["kind"] == "Deployment")["spec"]["template"]["spec"]
            c = pod["containers"][0]
            self.assertIn({"name": "OCI_RESOURCE_PRINCIPAL_VERSION", "value": "2.2"}, c["env"])
            self.assertIn("--namespace=isolated-test", c["args"])
            a.image = "example.invalid/controller:latest"
            with self.assertRaises(ValueError):
                install.render(a)

    def test_gateway_rbac_is_exact_class_and_no_secret_access(self):
        with tempfile.TemporaryDirectory() as d:
            release = Path(d) / "release.json"
            release.write_text(json.dumps({"image": "example.invalid/controller@sha256:" + "a" * 64}))
            a = argparse.Namespace(release=str(release), image=None, namespace="test", installation_id="installation", compartment="ocid1.compartment.test", subnet="ocid1.subnet.test", region="test-region", auth="workload", pull_secret=None, gateway_api=True, gateway_class="owned-class")
            objects = install.render(a)["items"]
            role = next(o for o in objects if o["kind"] == "ClusterRole")
            gateway_rule = next(r for r in role["rules"] if "gatewayclasses" in r["resources"])
            self.assertEqual(gateway_rule["resourceNames"], ["owned-class"])
            self.assertNotIn("list", gateway_rule["verbs"])
            self.assertNotIn("watch", gateway_rule["verbs"])
            for o in objects:
                for r in o.get("rules", []):
                    self.assertNotIn("secrets", r["resources"])
            args = next(o for o in objects if o["kind"] == "Deployment")["spec"]["template"]["spec"]["containers"][0]["args"]
            self.assertIn("--enable-gateway-api", args)
            a.gateway_api = False
            self.assertNotIn("gateway.networking.k8s.io", json.dumps(install.render(a)))

    def test_gateway_crds_preserve_newer_and_reject_unverified_existing(self):
        wanted = {"spec": {"group": "gateway.networking.k8s.io", "scope": "Namespaced", "names": {"kind": "Gateway"}, "versions": [{"name": "v1", "served": True, "schema": {"pinned": True}, "subresources": {"status": {}}}]}}
        same = json.loads(json.dumps(wanted))
        self.assertTrue(gateway_crds.compatible(same, wanted))
        same["spec"]["versions"][0]["schema"] = {"changed": True}
        self.assertFalse(gateway_crds.compatible(same, wanted))
        same["metadata"] = {"annotations": {"gateway.networking.k8s.io/bundle-version": "v1.7.0", "gateway.networking.k8s.io/channel": "standard"}}
        self.assertTrue(gateway_crds.compatible(same, wanted))
        same["metadata"]["annotations"]["gateway.networking.k8s.io/bundle-version"] = "v1.5.0"
        self.assertFalse(gateway_crds.compatible(same, wanted))
        same["metadata"]["annotations"]["gateway.networking.k8s.io/bundle-version"] = "v2.0.0"
        self.assertFalse(gateway_crds.compatible(same, wanted))
        same["spec"]["versions"][0]["served"] = False
        self.assertFalse(gateway_crds.compatible(same, wanted))

    def test_cleanup_refuses_foreign_and_nonempty(self):
        a = SimpleNamespace(installation_id="install", pool_uid="pool", compartment="comp", subnet="sub")
        lb = SimpleNamespace(compartment_id="comp", subnet_id="sub", freeform_tags={"controller": "independent-shared-nlb", "installation": "install", "pool-uid": "pool", "shard": "0"}, listeners={}, backend_sets={}, lifecycle_state="ACTIVE")
        cleanup.validate(lb, a, 0)
        lb.freeform_tags["installation"] = "foreign"
        with self.assertRaises(ValueError):
            cleanup.validate(lb, a, 0)
        lb.freeform_tags["installation"] = "install"
        lb.listeners["live-tunnel"] = {}
        with self.assertRaises(ValueError):
            cleanup.validate(lb, a, 0)

    def test_package_traversal_and_duplicate_rejected(self):
        for name in ["../escape", "/absolute", "C:/escape", "a\\escape", "a/../escape"]:
            self.assertFalse(package.safe_name(name), name)
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "bad.zip"
            with zipfile.ZipFile(path, "w") as z:
                z.writestr("../escape", "bad")
            with self.assertRaises(ValueError):
                package.verify(path, Path(d) / "extract")
            self.assertFalse((Path(d) / "escape").exists())
            with warnings.catch_warnings():
                warnings.simplefilter("ignore", UserWarning)
                with zipfile.ZipFile(path, "w") as z:
                    z.writestr("duplicate", "first")
                    z.writestr("duplicate", "second")
            with self.assertRaisesRegex(ValueError, "duplicate"):
                package.verify(path)

    def test_manifest_tamper_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "bad.zip"
            manifest = {"files": [{"path": "README.md", "size": 4, "sha256": "0" * 64}]}
            with zipfile.ZipFile(path, "w") as z:
                z.writestr("MANIFEST.json", json.dumps(manifest))
                z.writestr("SHA256SUMS", "")
                z.writestr("README.md", "evil")
            with self.assertRaisesRegex(ValueError, "checksum mismatch"):
                package.verify(path)


if __name__ == "__main__":
    unittest.main(verbosity=2)
