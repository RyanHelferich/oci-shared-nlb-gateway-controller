#!/usr/bin/env python3
"""Build an allowlisted evaluation archive, or verify and safely extract one."""
import argparse
import hashlib
import json
import re
import zipfile
from pathlib import Path, PurePosixPath

ROOT = Path(__file__).resolve().parents[1]
TOP_FILES = ["README.md", "LICENSE", "NOTICE", "SUPPORT.md", "CONTRIBUTING.md",
             "CHANGELOG.md", "Dockerfile", ".dockerignore", "go.mod", "go.sum",
             "THIRD_PARTY_NOTICES.json"]
SCRIPTS = ["generate-crds.py", "install.py", "install-gateway-crds.py", "package.py", "cleanup.py", "collect-notices.py", "audit-public.py"]
SECRET_PATTERNS = [rb"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----",
                   rb"ocid1\.[a-z-]+\.[a-z0-9]*\.[a-z0-9]*\.[a-z0-9]{16,}",
                   rb"(?i)(?:authorization:\s*bearer|oci_resource_principal_rpst\s*[:=])\s*[a-z0-9]"]


def digest(data):
    return hashlib.sha256(data).hexdigest()


def safe_name(name):
    p = PurePosixPath(name)
    return bool(name) and not p.is_absolute() and ".." not in p.parts and "\\" not in name and ":" not in name and str(p) == name


def build(release_file, output):
    release = json.loads(Path(release_file).read_text(encoding="utf-8"))
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[a-zA-Z0-9.-]+)?", release["version"]):
        raise ValueError("release version must be an explicit semantic version")
    if not re.fullmatch(r"[^\s]+@sha256:[0-9a-f]{64}", release["image"]):
        raise ValueError("release image must be pinned by manifest digest")
    archive = Path(release["imageArchive"])
    if not archive.is_absolute():
        archive = Path(release_file).resolve().parent / archive
    image_bytes = archive.read_bytes()
    if digest(image_bytes) != release["imageArchiveSha256"]:
        raise ValueError("image archive checksum does not match release metadata")
    paths = [ROOT / f for f in TOP_FILES]
    for directory, suffixes in [("api", {".go"}), ("cmd", {".go"}), ("internalcontroller", {".go"}), ("internalgateway", {".go"}), ("internalgatewaypool", {".go"}), ("deploy", {".json", ".yaml"}), ("examples", {".md", ".yaml"}), ("docs", {".md", ".drawio", ".png"})]:
        paths += sorted(p for p in (ROOT / directory).rglob("*") if p.is_file() and p.suffix in suffixes)
    paths += [ROOT / "deploy" / "gateway-api-v1.6.2" / "LICENSE"]
    paths += [ROOT / "scripts" / f for f in SCRIPTS]
    paths += [ROOT / "tests" / "package" / "test_tools.py"]
    files = {}
    for path in paths:
        if path.is_symlink() or not path.resolve().is_relative_to(ROOT.resolve()):
            raise ValueError("symlink or path outside project: " + str(path))
        data = path.read_bytes()
        if any(re.search(pattern, data) for pattern in SECRET_PATTERNS):
            raise ValueError("potential secret or live OCI identifier in " + str(path))
        files[path.relative_to(ROOT).as_posix()] = data
    # Metadata contains no local source/archive path or raw environment identifiers.
    public_release = {k: release[k] for k in ["version", "image", "imageArchiveSha256"]}
    for field in ["sourceRevision", "builtAt", "architecture", "evaluationStatus"]:
        if field in release:
            public_release[field] = release[field]
    public_release["imageArchive"] = "images/controller-image.tar"
    release_bytes = (json.dumps(public_release, indent=2) + "\n").encode()
    if any(re.search(pattern, release_bytes) for pattern in SECRET_PATTERNS):
        raise ValueError("potential secret or live OCI identifier in release metadata")
    files["release.json"] = release_bytes
    files[public_release["imageArchive"]] = image_bytes
    manifest = {"format": 1, "version": release["version"], "files": [{"path": n, "size": len(d), "sha256": digest(d)} for n, d in sorted(files.items())]}
    files["MANIFEST.json"] = (json.dumps(manifest, indent=2) + "\n").encode()
    files["SHA256SUMS"] = "".join(digest(d) + "  " + n + "\n" for n, d in sorted(files.items())).encode()
    out = Path(output)
    out.parent.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(out, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=6) as z:
        for name, data in sorted(files.items()):
            info = zipfile.ZipInfo(name, (2026, 1, 1, 0, 0, 0))
            info.compress_type = zipfile.ZIP_DEFLATED
            info.external_attr = 0o100644 << 16
            z.writestr(info, data)
    verify(out)
    print("Archive SHA256: " + digest(out.read_bytes()))


def verify(archive, extract=None):
    with zipfile.ZipFile(archive) as z:
        names = z.namelist()
        if len(names) != len(set(names)) or any(not safe_name(n) for n in names):
            raise ValueError("duplicate or unsafe archive paths")
        if any((i.external_attr >> 16) & 0o170000 == 0o120000 for i in z.infolist()):
            raise ValueError("archive symlinks are forbidden")
        manifest = json.loads(z.read("MANIFEST.json"))
        entries = manifest["files"]
        expected = {x["path"] for x in entries}
        if len(expected) != len(entries) or set(names) != expected | {"MANIFEST.json", "SHA256SUMS"}:
            raise ValueError("archive file set differs from manifest")
        for item in entries:
            data = z.read(item["path"])
            if len(data) != item["size"] or digest(data) != item["sha256"]:
                raise ValueError("manifest checksum mismatch: " + item["path"])
        sums = "".join(digest(z.read(n)) + "  " + n + "\n" for n in sorted(names) if n != "SHA256SUMS")
        if z.read("SHA256SUMS").decode() != sums:
            raise ValueError("SHA256SUMS mismatch")
        release = json.loads(z.read("release.json"))
        if digest(z.read(release["imageArchive"])) != release["imageArchiveSha256"]:
            raise ValueError("image checksum mismatch")
        if extract:
            dest = Path(extract).resolve()
            if dest.exists() and any(dest.iterdir()):
                raise ValueError("extraction destination must be new or empty")
            dest.mkdir(parents=True, exist_ok=True)
            for name in names:
                target = dest / name
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(z.read(name))
            print("Extracted verified archive to " + str(dest))
        print("Verified " + str(archive) + " (" + str(len(entries)) + " payload files)")


def main():
    p = argparse.ArgumentParser(description=__doc__)
    mode = p.add_mutually_exclusive_group(required=True)
    mode.add_argument("--release", help="Build using release metadata; archive path resolves relative to metadata file")
    mode.add_argument("--verify", help="Verify an existing archive")
    p.add_argument("--output")
    p.add_argument("--extract", help="Verify, then extract to a new or empty directory")
    a = p.parse_args()
    try:
        if a.release:
            if not a.output or a.extract:
                p.error("build requires --output and does not accept --extract")
            build(a.release, a.output)
        else:
            verify(a.verify, a.extract)
    except (ValueError, KeyError, OSError, zipfile.BadZipFile) as e:
        p.error(str(e))


if __name__ == "__main__":
    main()
