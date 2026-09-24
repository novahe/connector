#!/usr/bin/env python3
"""Root-only fault tests; every test creates and removes its own TAP switch.

CONNECTOR_CTL=/path/to/connector-ctl python3 test/e2e/debug_tap_safety_test.py
Python is only a test dependency. vswitch-tap-debug.sh remains a Bash script.
"""
import json
import os
from pathlib import Path
import shlex
import signal
import subprocess
import tempfile
import time
import unittest


CTL = os.environ.get("CONNECTOR_CTL", "/opt/sandbox/bin/connector-ctl")
SCRIPT = Path(__file__).resolve().parents[2] / "scripts/vswitch-tap-debug.sh"


def process_start(pid):
    try:
        return Path(f"/proc/{pid}/stat").read_text().rsplit(") ", 1)[1].split()[19]
    except FileNotFoundError:
        return None


def running(pid):
    try:
        return Path(f"/proc/{pid}/stat").read_text().rsplit(") ", 1)[1].split()[0] not in ("Z", "X")
    except FileNotFoundError:
        return False


class DebugTapSafety(unittest.TestCase):
    def setUp(self):
        if os.geteuid() or not Path(CTL).is_file():
            self.fail("root and CONNECTOR_CTL pointing to a connector binary are required")
        self.tmp = tempfile.TemporaryDirectory(prefix="dt-safety-test-")
        self.root = Path(self.tmp.name)
        self.state = self.root / "sessions"
        self.logs = []
        self.children = []
        self.socats = []
        self.namespaces = set()
        self.sw = "ds" + str(os.getpid())
        self.addCleanup(self.cleanup_resources)
        self.run_command(["ip", "netns", "add", self.sw], check=True)
        self.ctl("start", self.sw, "--netns=" + self.sw, "--ports=4", "--mode=tap",
                 "--mac-addr=02:db:ee:00:00:01", "--floating-ip-base=198.19.240.0")

    def run_command(self, args, **kwargs):
        return subprocess.run(args, text=True, capture_output=True, timeout=20, **kwargs)

    def ctl(self, *args):
        return self.run_command([CTL, "vswitch", *args], check=True).stdout

    def fields(self, record):
        return dict(line.split("=", 1) for line in record.read_text().splitlines())

    def records(self):
        return list(self.state.glob("session.*/session"))

    def slot(self, port=4):
        return json.loads(self.ctl("show", "slots", self.sw, str(port - 1)))[0]["state"]

    def wait_for(self, condition, timeout=8):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            if condition():
                return
            time.sleep(.03)
        self.fail("timed out waiting for test condition")

    def launch(self, *args, connector=CTL, extra_env=None):
        log_path = self.root / ("log-" + str(len(self.logs)))
        log = log_path.open("w")
        self.logs.append((log_path, log))
        env = dict(os.environ, CONNECTOR_CTL=connector, DEBUG_STATE_ROOT=str(self.state))
        env.update(extra_env or {})
        proc = subprocess.Popen([str(SCRIPT), *args], env=env, stdout=log, stderr=log)
        self.children.append(proc)
        return proc, log_path

    def session(self):
        proc, log = self.launch("--port", "4", self.sw, "sleep", "60")
        self.wait_for(lambda: "entered " in log.read_text() or proc.poll() is not None)
        self.assertIsNone(proc.poll(), log.read_text())
        record = self.records()[0]
        data = self.fields(record)
        self.socats.append((int(data["socat_pid"]), data["socat_start"]))
        self.namespaces.add(data["debug_ns"])
        return proc, record, data

    def recover(self, connector=CTL, extra_env=None):
        env = dict(os.environ, CONNECTOR_CTL=connector, DEBUG_STATE_ROOT=str(self.state))
        env.update(extra_env or {})
        return self.run_command([str(SCRIPT), "--cleanup-stale"], env=env)

    def wrapper(self, body):
        path = self.root / "connector-wrapper"
        path.write_text("#!/bin/bash\n" + body + "\nexec " + shlex.quote(CTL) + ' "$@"\n')
        path.chmod(0o700)
        return str(path)

    def consumer(self):
        ready = self.root / "consumer-ready"
        code = ("import os,fcntl,struct,time,pathlib; f=os.open('/dev/net/tun',os.O_RDWR); "
                f"fcntl.ioctl(f,0x400454ca,struct.pack('16sH22x',{(self.sw + '-t4').encode()!r},0x5002)); "
                f"pathlib.Path({str(ready)!r}).touch(); time.sleep(60)")
        proc = subprocess.Popen(["ip", "netns", "exec", self.sw, "python3", "-c", code],
                                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        self.children.append(proc)
        self.wait_for(lambda: ready.exists() or proc.poll() is not None)
        self.assertIsNone(proc.poll(), "VMM-style TUNSETIFF failed")
        return proc

    def test_unsaved_stopped_socat_is_gone_before_port_reuse(self):
        proc, record, data = self.session()
        proc.kill(); proc.wait()
        os.kill(int(data["socat_pid"]), signal.SIGSTOP)
        data["socat_pid"] = data["socat_start"] = ""
        record.write_text("".join(f"{k}={v}\n" for k, v in data.items()))
        result = self.recover()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(running(self.socats[0][0]))
        self.assertEqual(self.slot(), "free")
        self.assertFalse(self.records())
        self.ctl("attach", self.sw, "--port=4", "--inner-ip=169.254.0.21")
        self.consumer()  # Same flags as connector's open-port, must not EBUSY.

    def test_same_ip_new_tap_holder_is_not_released(self):
        proc, record, data = self.session()
        proc.kill(); proc.wait()
        os.kill(int(data["socat_pid"]), signal.SIGKILL)
        self.wait_for(lambda: not running(int(data["socat_pid"])))
        self.ctl("detach", self.sw, "--port=4", "--skip-device")
        self.ctl("attach", self.sw, "--port=4", "--inner-ip=169.254.0.21")
        consumer = self.consumer()
        before = self.run_command(["ip", "netns", "exec", self.sw, "ethtool", "-k", self.sw + "-t4"], check=True).stdout
        result = self.recover()
        after = self.run_command(["ip", "netns", "exec", self.sw, "ethtool", "-k", self.sw + "-t4"], check=True).stdout
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.slot(), "allocated")
        self.assertEqual(before, after)
        self.assertIsNone(consumer.poll())
        self.assertTrue(record.exists())
        consumer.kill(); consumer.wait()
        self.assertNotEqual(self.recover().returncode, 0)  # Suspicion is sticky.
        self.assertEqual(self.slot(), "allocated")

    def test_killed_attach_keeps_pending_record_and_stops_retry(self):
        wrapper = self.wrapper('if [[ "$1 $2" == "vswitch attach" ]]; then\n' +
            shlex.quote(CTL) + ' "$@" >/dev/null || exit $?\nkill -KILL $$\nfi')
        proc, log = self.launch(self.sw, "true", connector=wrapper)
        self.assertNotEqual(proc.wait(timeout=12), 0, log.read_text())
        allocated = [s["port"] for s in json.loads(self.ctl("show", "slots", self.sw)) if s["state"] == "allocated"]
        self.assertEqual(allocated, [4])
        self.assertEqual(len(self.records()), 1)
        self.assertEqual(self.fields(self.records()[0])["pending_port"], "4")
        self.assertNotEqual(self.recover().returncode, 0)
        self.assertTrue(self.records())

    def test_parent_killed_during_attach_cannot_race_cleanup(self):
        ready, gate, receipt = (self.root / name for name in ("ready", "gate", "receipt"))
        wrapper = self.wrapper('if [[ "$1 $2" == "vswitch attach" ]]; then\n' +
            shlex.quote(CTL) + ' "$@" >' + shlex.quote(str(receipt)) + ' || exit $?\n' +
            'touch ' + shlex.quote(str(ready)) + '\nwhile [[ ! -e ' + shlex.quote(str(gate)) + ' ]]; do sleep .03; done\n' +
            'cat ' + shlex.quote(str(receipt)) + '\nexit 0\nfi')
        proc, _ = self.launch(self.sw, "true", connector=wrapper)
        self.wait_for(ready.exists)
        proc.kill(); proc.wait()
        result = self.recover()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("attach still running", result.stderr)
        self.assertEqual(self.slot(), "allocated")
        gate.touch()
        self.wait_for(lambda: list(self.state.glob("session.*/attach.rc")))
        # The worker closes its lock immediately after publishing the receipt.
        self.wait_for(lambda: self.recover().returncode == 0)
        self.assertEqual(self.slot(), "free")
        self.assertFalse(self.records())

    def test_missing_snapshot_does_not_release_dirty_port(self):
        proc, record, _ = self.session()
        proc.kill(); proc.wait()
        snapshot = record.parent / "offloads"
        original = snapshot.read_text()
        snapshot.unlink()
        self.assertNotEqual(self.recover().returncode, 0)
        self.assertEqual(self.slot(), "allocated")
        self.assertTrue(record.exists())
        snapshot.write_text(original)
        result = self.recover()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.slot(), "free")

    def test_restore_failure_does_not_release_dirty_port(self):
        proc, record, _ = self.session()
        proc.kill(); proc.wait()
        snapshot = record.parent / "offloads"
        original = snapshot.read_text()
        snapshot.write_text(original + "test-nonexistent-feature: on\n")
        self.assertNotEqual(self.recover().returncode, 0)
        self.assertEqual(self.slot(), "allocated")
        snapshot.write_text(original)
        self.assertEqual(self.recover().returncode, 0)

    def test_query_failure_still_cleans_own_processes(self):
        proc, record, data = self.session()
        proc.kill(); proc.wait()
        wrapper = self.wrapper('[[ "$1 $2" != "vswitch show" ]] || exit 1')
        self.assertNotEqual(self.recover(wrapper).returncode, 0)
        self.assertFalse(running(int(data["socat_pid"])))
        self.assertFalse(Path("/var/run/netns", data["debug_ns"]).exists())
        self.assertTrue(record.exists())
        self.assertEqual(self.slot(), "allocated")
        self.assertEqual(self.recover().returncode, 0)

    def test_replaced_debug_namespace_stops_old_socat_and_keeps_port(self):
        proc, record, data = self.session()
        proc.kill(); proc.wait()
        ns = data["debug_ns"]
        for pid in self.run_command(["ip", "netns", "pids", ns], check=True).stdout.split():
            os.kill(int(pid), signal.SIGKILL)
        self.run_command(["ip", "netns", "del", ns], check=True)
        self.run_command(["ip", "netns", "add", ns], check=True)
        result = self.recover()
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(running(int(data["socat_pid"])))
        self.assertTrue(Path("/var/run/netns", ns).exists())
        self.assertEqual(self.slot(), "allocated")
        self.assertTrue(record.exists())

    def test_hung_connector_query_times_out_without_releasing_port(self):
        proc, record, data = self.session()
        proc.kill(); proc.wait()
        wrapper = self.wrapper('if [[ "$1 $2" == "vswitch show" ]]; then sleep 60; fi')
        started = time.monotonic()
        result = self.recover(wrapper, {"DEBUG_CTL_TIMEOUT": "1"})
        self.assertLess(time.monotonic() - started, 8)
        self.assertNotEqual(result.returncode, 0)
        self.assertFalse(running(int(data["socat_pid"])))
        self.assertEqual(self.slot(), "allocated")
        self.assertTrue(record.exists())
        self.assertEqual(self.recover().returncode, 0)

    def test_attach_timeout_after_allocation_keeps_pending_port(self):
        wrapper = self.wrapper('if [[ "$1 $2" == "vswitch attach" ]]; then\n' +
            shlex.quote(CTL) + ' "$@" >/dev/null || exit $?\nsleep 60\nfi')
        proc, log = self.launch(self.sw, "true", connector=wrapper,
                                extra_env={"DEBUG_CTL_TIMEOUT": "1"})
        self.assertNotEqual(proc.wait(timeout=8), 0, log.read_text())
        self.assertEqual(self.slot(), "allocated")
        self.assertEqual(len(self.records()), 1)
        self.assertEqual(self.fields(self.records()[0])["pending_port"], "4")
        self.assertNotEqual(self.recover().returncode, 0)

    def test_ready_failure_shows_connector_error(self):
        wrapper = self.wrapper('if [[ "$1 $2 $3 $4" == "vswitch status ' + self.sw +
            ' --ready" ]]; then echo "unknown flag: --ready" >&2; exit 1; fi')
        proc, log = self.launch(self.sw, "true", connector=wrapper)
        self.assertNotEqual(proc.wait(timeout=8), 0)
        self.assertIn("unknown flag: --ready", log.read_text())
        self.assertFalse(self.records())

    def test_list_works_without_connector_and_shows_fip_and_creation(self):
        proc, record, data = self.session()
        result = self.run_command([str(SCRIPT), "--list"], env=dict(
            os.environ, CONNECTOR_CTL="/does/not/exist", DEBUG_STATE_ROOT=str(self.state)))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("fip=" + data["floating_ip"], result.stdout)
        self.assertIn("created=" + data["created_at"], result.stdout)
        proc.kill(); proc.wait()
        self.assertEqual(self.recover().returncode, 0)

    def test_background_process_ignoring_term_is_reaped(self):
        pidfile = self.root / "background-pid"
        proc, log = self.launch(self.sw, "sh", "-c", '(trap "" TERM; exec sleep 60) & echo $! > "$1"', "sh", str(pidfile))
        self.assertEqual(proc.wait(timeout=12), 0, log.read_text())
        self.assertFalse(running(int(pidfile.read_text())))
        self.assertEqual(self.slot(), "free")
        self.assertFalse(self.records())

    def test_tail_exhausted_never_uses_front_ports(self):
        for port in (3, 4):
            self.ctl("attach", self.sw, f"--port={port}", "--inner-ip=169.254.0.21")
        for tries in ("8", "4096"):
            with self.subTest(tries=tries):
                proc, log = self.launch(self.sw, "true", extra_env={"DEBUG_TAIL_TRIES": tries})
                self.assertNotEqual(proc.wait(timeout=12), 0, log.read_text())
                self.assertEqual([self.slot(p) for p in (1, 2, 3, 4)], ["free", "free", "allocated", "allocated"])
                self.assertFalse(self.records())

    def cleanup_resources(self):
        # Only this test's processes/namespaces/switch. Never use global pkill.
        for proc in self.children:
            if proc.poll() is None:
                proc.kill()
            proc.wait(timeout=5)
        gate = self.root / "gate"
        gate.touch()  # Allow an interrupted attach fixture to finish its receipt.
        for record in self.records():
            data = self.fields(record)
            self.namespaces.add(data.get("debug_ns", ""))
            if data.get("socat_pid"):
                self.socats.append((int(data["socat_pid"]), data["socat_start"]))
        for pid, start in self.socats:
            if process_start(pid) == start:
                try:
                    os.kill(pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
        for ns in self.namespaces - {""}:
            for pid in self.run_command(["ip", "netns", "pids", ns]).stdout.split():
                try:
                    os.kill(int(pid), signal.SIGKILL)
                except ProcessLookupError:
                    pass
            self.run_command(["ip", "netns", "del", ns])
            resolver = Path("/etc/netns", ns, "resolv.conf")
            if resolver.exists():
                resolver.unlink()
                resolver.parent.rmdir()
        self.run_command([CTL, "vswitch", "stop", self.sw, "--force"])
        self.run_command(["ip", "netns", "del", self.sw])
        for _, log in self.logs:
            log.close()
        self.assertFalse(Path("/sys/fs/bpf", self.sw).exists())
        self.assertFalse(Path("/var/run/netns", self.sw).exists())
        self.tmp.cleanup()


if __name__ == "__main__":
    unittest.main(verbosity=2)
