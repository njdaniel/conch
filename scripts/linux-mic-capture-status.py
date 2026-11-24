#!/usr/bin/env python3
"""Summarize one local PipeWire graph snapshot for a native voice helper."""

import json
import os
import selectors
import subprocess
import sys
import time


MAX_BYTES = 4 * 1024 * 1024
TIMEOUT = 3
INTERFACE = "PipeWire:Interface:"
USAGE = "Usage: python3 scripts/linux-mic-capture-status.py HELPER_PID"


def snapshot():
    """Keep the raw graph in memory. Enforce both limits before parsing."""
    deadline = time.monotonic() + TIMEOUT
    output = bytearray()
    with subprocess.Popen(
        ["pw-dump", "--no-colors"],
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
    ) as dump, selectors.DefaultSelector() as selector:
        try:
            selector.register(dump.stdout, selectors.EVENT_READ)
            while True:
                if not selector.select(max(0, deadline - time.monotonic())):
                    raise ValueError("timeout")
                if time.monotonic() >= deadline:
                    raise ValueError("timeout")
                chunk = os.read(dump.stdout.fileno(), min(65536, MAX_BYTES + 1 - len(output)))
                if not chunk:
                    break
                output.extend(chunk)
                if len(output) > MAX_BYTES:
                    raise ValueError("size limit")
            if dump.wait(timeout=max(0, deadline - time.monotonic())) != 0:
                raise ValueError("query failed")
        finally:
            if dump.poll() is None:
                dump.kill()
    return output


def object_id(value):
    # Some PipeWire versions render numeric properties as strings.
    if isinstance(value, str) and len(value) <= 10 and value.isascii() and value.isdecimal():
        value = int(value)
    if type(value) is int and 0 <= value < 2**32 - 1:
        return value
    return None


def as_object(value):
    return value if isinstance(value, dict) else {}


def default_source_names(graph):
    names = set()
    for entry in graph:
        if entry.get("type") != INTERFACE + "Metadata" or as_object(entry.get("props")).get("metadata.name") != "default":
            continue
        records = entry.get("metadata")
        if not isinstance(records, list):
            raise ValueError("invalid metadata")
        for item in records:
            record = as_object(item)
            if record.get("subject") == 0 and record.get("key") == "default.audio.source":
                name = as_object(record.get("value")).get("name")
                if isinstance(name, str) and name:
                    names.add(name)
    return names


def summarize(raw, helper_pid):
    graph = json.loads(raw)
    if not isinstance(graph, list) or any(not isinstance(entry, dict) for entry in graph):
        raise ValueError("invalid graph")

    nodes = {}
    clients = set()
    links = []
    for entry in graph:
        oid = object_id(entry.get("id"))
        info = as_object(entry.get("info"))
        props = as_object(info.get("props"))
        kind = entry.get("type")
        if oid is None:
            continue
        if kind == INTERFACE + "Client" and object_id(props.get("pipewire.sec.pid")) == helper_pid:
            clients.add(oid)
        elif kind == INTERFACE + "Node":
            nodes[oid] = (info, props)
        elif kind == INTERFACE + "Link" and info.get("state") == "active":
            links.append((object_id(info.get("input-node-id")), object_id(info.get("output-node-id"))))

    capture = {
        oid for oid, (_, props) in nodes.items()
        if props.get("media.class") == "Stream/Input/Audio" and object_id(props.get("client.id")) in clients
    }
    defaults = default_source_names(graph)
    source_nodes = {
        oid for oid, (_, props) in nodes.items()
        if props.get("media.class") == "Audio/Source"
    }
    default_nodes = {
        oid for oid in source_nodes
        if isinstance(nodes[oid][1].get("node.name"), str)
        and nodes[oid][1]["node.name"] in defaults
    }
    # Several conflicting default records are ambiguous, not known.
    unambiguous = len(defaults) == 1 and len(default_nodes) == 1
    active_inputs = [output for target, output in links if target in capture]
    return {
        "helper_clients": len(clients),
        "capture_streams": len(capture),
        "running_capture_streams": sum(nodes[oid][0].get("state") == "running" for oid in capture),
        "active_input_links": len(active_inputs),
        "active_audio_source_links": sum(oid in source_nodes for oid in active_inputs),
        "default_audio_source": "known" if unambiguous else "unknown",
        "active_default_source_links": (
            sum(oid in default_nodes for oid in active_inputs) if unambiguous else None
        ),
    }


def main(args):
    if args == ["--help"]:
        print(USAGE)
        return 0
    helper_pid = object_id(args[0]) if len(args) == 1 else None
    if not helper_pid:
        print(USAGE, file=sys.stderr)
        return 2
    try:
        report = summarize(snapshot(), helper_pid)
    except (OSError, ValueError, subprocess.TimeoutExpired, RecursionError):
        print("PipeWire graph unavailable, invalid, or over collection limits.", file=sys.stderr)
        return 1
    print(json.dumps(report, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
