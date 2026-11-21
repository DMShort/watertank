#!/usr/bin/env python3
"""Write manifest.json for a release (used by .github/workflows/release.yml).

usage: make_manifest.py <dist_dir> <version> <tag> [notes_file]
The Watershed Tank Tool reads this to find the newest tool, firmware and
esptool, and checks each download against these SHA-256 hashes.
"""
import hashlib, json, os, sys
from datetime import datetime, timezone

dist, version, tag = sys.argv[1], sys.argv[2], sys.argv[3]
notes = ""
if len(sys.argv) > 4 and os.path.exists(sys.argv[4]):
    notes = open(sys.argv[4], encoding="utf-8").read().strip()

def info(name):
    p = os.path.join(dist, name)
    with open(p, "rb") as f:
        data = f.read()
    return hashlib.sha256(data).hexdigest(), len(data)

def asset(name, ver=None):
    sha, _ = info(name)
    d = {"file": name, "sha256": sha}
    if ver:
        d["version"] = ver
    return d

# flashed at their own addresses so the settings area (NVS, 0x9000-0xDFFF) is kept
parts = []
for off, name in [("0x1000", "fw_bootloader.bin"), ("0x8000", "fw_partitions.bin"),
                  ("0xe000", "fw_boot_app0.bin"), ("0x10000", "fw_app.bin")]:
    sha, size = info(name)
    parts.append({"offset": off, "file": name, "sha256": sha, "size": size})

m = {
    "version": version,
    "tag": tag,
    "notes": notes,
    "published": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "tool": asset("WatershedTankTool.exe", version),
    "esptool": asset("esptool.exe", os.environ.get("ESPTOOL_VERSION", "")),
    "firmware": {"version": version, "board": "heltec-v2", "chip": "esp32", "parts": parts},
}
json.dump(m, sys.stdout, indent=2)
print()
