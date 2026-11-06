#!/usr/bin/env python3
"""Read-only D1 inventory. Does not bind keys, open media or read credentials."""

import argparse
import datetime
import glob
import json
import os
import platform
import shutil
import subprocess
import xml.etree.ElementTree as ET


def run(args):
    if shutil.which(args[0]) is None:
        return "missing_tool", ""
    try:
        result = subprocess.run(args, capture_output=True, text=True, timeout=8,
                                check=False)
    except subprocess.TimeoutExpired:
        return "timeout", ""
    except OSError:
        return "inaccessible", ""
    if result.returncode == 0:
        return "observed", result.stdout
    error = result.stderr.lower()
    if "not permitted" in error or "permission denied" in error:
        return "permission_denied", ""
    return "unavailable", ""


def interfaces(destination, path):
    status, xml = run(["gdbus", "introspect", "--session", "--dest", destination,
                       "--object-path", path, "--xml"])
    if status != "observed":
        return {"status": status}
    try:
        root = ET.fromstring(xml)
    except ET.ParseError:
        return {"status": "invalid_introspection"}
    found = {}
    for interface in root.findall("interface"):
        # gdbus --xml describes property types, not their runtime values.
        found[interface.attrib["name"]] = sorted(
            prop.attrib["name"] for prop in interface.findall("property"))
    return {"status": "observed", "interfaces": found}


def portal_property(interface, prop):
    status, value = run([
        "gdbus", "call", "--session", "--dest", "org.freedesktop.portal.Desktop",
        "--object-path", "/org/freedesktop/portal/desktop", "--method",
        "org.freedesktop.DBus.Properties.Get", interface, prop,
    ])
    return {"status": status, **({"value": value.strip()} if status == "observed" else {})}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--helper", help="optional trusted conch-voice binary; --version only")
    args = parser.parse_args()
    os_release = {}
    with open("/etc/os-release", encoding="utf-8") as source:
        for line in source:
            key, sep, value = line.strip().partition("=")
            if sep and key in {"ID", "VERSION_ID", "PRETTY_NAME"}:
                os_release[key] = value.strip('"')
    packages = ["cosmic-comp", "cosmic-session", "xdg-desktop-portal",
                "xdg-desktop-portal-cosmic", "xdg-desktop-portal-gtk",
                "xdg-desktop-portal-gnome", "xdg-desktop-portal-kde",
                "gnome-shell", "kwin-wayland", "xserver-xorg-core",
                "pipewire", "wireplumber", "gnome-keyring", "libsecret-1-0"]
    versions = {}
    for package in packages:
        status, version = run(["dpkg-query", "-W", "-f=${db:Status-Status} ${Version}", package])
        if status == "observed" and version.startswith("installed "):
            versions[package] = version.removeprefix("installed ").strip()
    portals = interfaces("org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop")
    properties = {}
    if portals["status"] == "observed":
        for short, names in {
            "GlobalShortcuts": ["version"],
            "ScreenCast": ["version", "AvailableSourceTypes", "AvailableCursorModes"],
            "Secret": ["version"],
        }.items():
            full = "org.freedesktop.portal." + short
            if full not in portals["interfaces"]:
                properties[short] = {"status": "not_advertised"}
            else:
                properties[short] = {name: portal_property(full, name) for name in names}
    devices = glob.glob("/dev/input/event*")
    output = {
        "recorded_at_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "evidence": "inventory_only_no_input_or_media_trial",
        "os": os_release,
        "kernel": platform.release(),
        "architecture": platform.machine(),
        "session": {name: os.environ.get(name, "unset") for name in
                    ["XDG_CURRENT_DESKTOP", "XDG_SESSION_TYPE"]},
        "display_environment_present": {name: bool(os.environ.get(name)) for name in
                                        ["DISPLAY", "WAYLAND_DISPLAY"]},
        "packages": versions,
        "portal": portals,
        "portal_properties": properties,
        "secret_service": interfaces("org.freedesktop.secrets", "/org/freedesktop/secrets"),
        "pipewire_connection": run(["pw-cli", "info", "0"])[0],
        "wireplumber_connection": run(["wpctl", "status"])[0],
        "input_device_visibility": {
            "event_nodes": len(devices),
            "readable_by_this_process": sum(os.access(path, os.R_OK) for path in devices),
            "note": "No device opened; visibility and access may be sandbox-restricted.",
        },
        "electron": {"status": "not_measured", "proposed_version": "45.1.0"},
        "chromium": {"status": "not_measured"},
    }
    if args.helper:
        status, version = run([os.path.abspath(args.helper), "--version"])
        output["helper"] = {"status": status, **({"version": version.strip()}
                                              if status == "observed" else {})}
    else:
        output["helper"] = {"status": "not_measured"}
    print(json.dumps(output, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
