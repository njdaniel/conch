#!/usr/bin/env python3
"""Exercise the diagnostic CLI with a disposable pw-dump executable."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("linux-mic-capture-status.py")
PREFIX = "PipeWire:Interface:"
PID = "42424242"
REDACTED = "private-fixture-name-and-device-id"


def obj(kind, oid, **info):
    return {"type": PREFIX + kind, "id": oid, "info": info}


def fixtures():
    return [
        obj("Client", 31, props={"pipewire.sec.pid": PID, "application.name": REDACTED}),
        obj("Client", 32, props={"application.process.id": PID}),
        obj("Node", 41, state="running", props={"media.class": "Stream/Input/Audio", "client.id": "31"}),
        obj("Node", 42, state="idle", props={"media.class": "Stream/Input/Audio", "client.id": 31}),
        obj("Node", 43, state="running", props={"media.class": "Stream/Output/Audio", "client.id": 31}),
        obj("Node", 44, state="running", props={
            "media.class": "Stream/Input/Audio", "client.id": 32, "pipewire.sec.pid": PID,
        }),
        obj("Node", 51, props={"media.class": "Audio/Source", "node.name": REDACTED}),
        obj("Node", 52, props={"media.class": "Audio/Source", "node.name": REDACTED + "-other"}),
        obj("Node", 53, props={"media.class": "Audio/Sink"}),
        obj("Link", 61, state="active", **{"input-node-id": 41, "output-node-id": 51}),
        obj("Link", 62, state="active", **{"input-node-id": 41, "output-node-id": 52}),
        obj("Link", 63, state="active", **{"input-node-id": 41, "output-node-id": 53}),
        obj("Link", 64, state="paused", **{"input-node-id": 42, "output-node-id": 51}),
        obj("Link", 65, state="active", **{"input-node-id": 44, "output-node-id": 51}),
        {
            "type": PREFIX + "Metadata",
            "id": 71,
            "props": {"metadata.name": "default"},
            "metadata": [{"subject": 0, "key": "default.audio.source", "value": {"name": REDACTED}}],
        },
    ]


class CaptureStatus(unittest.TestCase):
    def run_collector(self, dump_code, pid=PID):
        with tempfile.TemporaryDirectory() as temporary:
            executable = Path(temporary, "pw-dump")
            executable.write_text(f"#!{sys.executable}\n{dump_code}\n", encoding="utf-8")
            executable.chmod(0o700)
            result = subprocess.run(
                [sys.executable, str(SCRIPT), pid],
                env={**os.environ, "PATH": temporary},
                capture_output=True, timeout=6, text=True, check=False,
            )
        # Across both streams, even on failure and for untrusted CLI input.
        for value in (REDACTED, PID):
            self.assertNotIn(value, result.stdout)
            self.assertNotIn(value, result.stderr)
        return result

    def fixture_output(self, graph, pid=PID):
        result = self.run_collector(f"print({json.dumps(graph)!r})", pid)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stderr, "")
        return json.loads(result.stdout)

    def assert_unavailable(self, code):
        result = self.run_collector(code)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stdout, "")
        self.assertEqual(result.stderr, "PipeWire graph unavailable, invalid, or over collection limits.\n")

    def test_counts_helper_capture_and_only_live_links(self):
        self.assertEqual(self.fixture_output(fixtures()), {
            "helper_clients": 1,
            "capture_streams": 2,
            "running_capture_streams": 1,
            "active_input_links": 3,
            "active_audio_source_links": 2,
            "default_audio_source": "known",
            "active_default_source_links": 1,
        })

    def test_no_metadata_and_invisible_helper_are_distinct(self):
        graph = fixtures()[:-1]
        report = self.fixture_output(graph)
        self.assertEqual(report["default_audio_source"], "unknown")
        self.assertIsNone(report["active_default_source_links"])
        self.assertEqual(report["active_audio_source_links"], 2)
        report = self.fixture_output(fixtures(), "42424243")
        self.assertEqual(report["helper_clients"], 0)
        self.assertEqual(report["capture_streams"], 0)
        self.assertEqual(report["active_default_source_links"], 0)

    def test_conflicting_default_records_are_unknown(self):
        graph = fixtures()
        graph[-1]["metadata"].append(
            {"subject": 0, "key": "default.audio.source", "value": {"name": "absent-" + REDACTED}}
        )
        report = self.fixture_output(graph)
        self.assertEqual(report["default_audio_source"], "unknown")
        self.assertIsNone(report["active_default_source_links"])

    def test_rejects_unparseable_output_without_printing_it(self):
        self.assert_unavailable(f"print({REDACTED!r})")
        self.assert_unavailable("print('[null]')")

    def test_query_failure_discards_partial_stdout_and_stderr(self):
        self.assert_unavailable(
            f"import sys; print('[]'); print({REDACTED!r}, file=sys.stderr); sys.exit(7)"
        )

    def test_limits_output_size_and_wait_time(self):
        self.assert_unavailable(f"import sys; sys.stdout.write({REDACTED!r} * 1_000_000)")
        self.assert_unavailable("import time; time.sleep(30)")

    def test_invalid_helper_pid_only_prints_usage(self):
        result = self.run_collector("raise AssertionError('must not be called')", REDACTED)
        self.assertEqual(result.returncode, 2)
        self.assertEqual(result.stdout, "")
        self.assertTrue(result.stderr.startswith("Usage: "))


if __name__ == "__main__":
    unittest.main()
