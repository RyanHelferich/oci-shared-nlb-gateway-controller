#!/usr/bin/env python3
"""Fail when a public source tree contains common disclosure mistakes or broken local links."""
import ipaddress
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
TEXT_SUFFIXES = {".go", ".md", ".json", ".yaml", ".yml", ".py", ".sh", ".mod", ".sum", ".txt"}
TEXT_NAMES = {"Dockerfile", "LICENSE", "NOTICE", ".gitignore", ".dockerignore"}
FORBIDDEN_DIRS = [ROOT / "internal", ROOT / "tests" / "oci", ROOT / "dist"]
PATTERNS = {
    "customer project name": re.compile("live" + "kit", re.I),
    "local user or vault path": re.compile("(?:Ryan" + "H|Obsidian" + " Vaults|OCI-" + "FY27)", re.I),
    "absolute Windows path": re.compile(r"\b[A-Za-z]:\\"),
    "private key": re.compile(r"-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----"),
    "live OCI identifier": re.compile(r"ocid1\.[a-z-]+\.[a-z0-9-]*\.[a-z0-9-]*\.[a-z0-9]{16,}", re.I),
}
PUBLIC_REPOSITORY = "github.com/RyanHelferich/oci-shared-nlb-gateway-controller"
LINK = re.compile(r"(?<!!)\[[^\]]+\]\(([^)]+)\)")
IPV4 = re.compile(r"(?<![0-9.])(?:[0-9]{1,3}\.){3}[0-9]{1,3}(?![0-9.])")


def text_files():
    for path in ROOT.rglob("*"):
        if not path.is_file() or ".cache" in path.parts or ".git" in path.parts:
            continue
        if path.suffix.lower() in TEXT_SUFFIXES or path.name in TEXT_NAMES:
            yield path


def main():
    errors = []
    for forbidden in FORBIDDEN_DIRS:
        if forbidden.exists():
            errors.append(f"forbidden public directory exists: {forbidden.relative_to(ROOT)}")
    for path in text_files():
        relative = path.relative_to(ROOT)
        text = path.read_text(encoding="utf-8", errors="replace")
        for label, pattern in PATTERNS.items():
            inspected = text.replace(PUBLIC_REPOSITORY, "github.com/PUBLIC_OWNER/oci-shared-nlb-gateway-controller") if label == "local user or vault path" else text
            if pattern.search(inspected):
                errors.append(f"{relative}: possible {label}")
        for value in IPV4.findall(text):
            try:
                address = ipaddress.ip_address(value)
            except ValueError:
                continue
            documented = any(address in network for network in (
                ipaddress.ip_network("192.0.2.0/24"),
                ipaddress.ip_network("198.51.100.0/24"),
                ipaddress.ip_network("203.0.113.0/24"),
            ))
            vendored_gateway_example = (relative.as_posix() == "deploy/gateway-api-v1.6.2/gateways.yaml"
                                        and value == ".".join(("1", "2", "3", "4")))
            if address.is_global and not documented and not vendored_gateway_example:
                errors.append(f"{relative}: non-documentation public IPv4 address {address}")
        if path.suffix.lower() == ".md":
            for raw in LINK.findall(text):
                target = raw.strip().strip("<>").split("#", 1)[0]
                if not target or re.match(r"(?:https?|mailto):", target, re.I):
                    continue
                resolved = (path.parent / target).resolve()
                if not resolved.is_relative_to(ROOT.resolve()) or not resolved.exists():
                    errors.append(f"{relative}: broken or escaping local link {raw}")
    if errors:
        raise SystemExit("Public-tree audit failed:\n- " + "\n- ".join(sorted(set(errors))))
    print("Public-tree audit passed")


if __name__ == "__main__":
    main()
