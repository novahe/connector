#!/usr/bin/env python3
"""Private-switch lifecycle E2E for connector-ctl vswitch debug (Go)."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import unittest


CTL = os.environ.get("CONNECTOR_CTL", "/opt/sandbox/bin/connector-ctl")


class DebugGoE2E(unittest.TestCase):
    def run_cmd(self, *args, check=True):
        result = subprocess.run(args, capture_output=True, text=True, timeout=30)
        if check and result.returncode:
            self.fail(f"{' '.join(args)} failed ({result.returncode}): {result.stderr}")
        return result

    def test_corrupt_session_causes_cleanup_failure(self):
        if os.geteuid() != 0:
            self.skipTest("root is required")
        if not Path(CTL).is_file():
            self.skipTest("set CONNECTOR_CTL to the Go binary under test")
        with tempfile.TemporaryDirectory(prefix="debug-e2e-") as tmp:
            root = Path(tmp) / "sessions"
            session = root / "session.corrupt"
            session.mkdir(parents=True, mode=0o700)
            (session / "state.json").write_text("{invalid")
            result = self.run_cmd(CTL, "vswitch", "debug", "--state-root", str(root),
                                  "--cleanup-stale", check=False)
            self.assertNotEqual(result.returncode, 0, "corrupt session was silently skipped")
            self.assertTrue(session.exists())

    def test_private_tap_slot_is_released(self):
        if os.geteuid() != 0:
            self.skipTest("root is required")
        if not Path(CTL).is_file():
            self.skipTest("set CONNECTOR_CTL to the Go binary under test")
        with tempfile.TemporaryDirectory(prefix="debug-e2e-") as tmp:
            switch = f"dg{os.getpid() & 0xffff:04x}"
            mgmt = switch + "m"
            mgmt_dev = switch + "m0"
            state = str(Path(tmp) / "sessions")
            self.run_cmd("ip", "netns", "add", switch)
            self.run_cmd("ip", "netns", "add", mgmt)
            try:
                self.run_cmd(CTL, "vswitch", "start", switch, "--netns=" + switch,
                             "--ports=4", "--mode=tap", "--mac-addr=02:db:ee:00:00:02",
                             "--floating-ip-base=198.19.244.0",
                             "--mgmt-extract=" + mgmt + ":" + mgmt_dev + ":169.254.169.254/32")
                try:
                    failed_root = str(Path(tmp) / "failed-sessions")
                    failed = self.run_cmd(CTL, "vswitch", "debug", "--state-root", failed_root,
                                          "--port", "4", "--transit-geneve-vni", "16777216",
                                          switch, "--", "/bin/true", check=False)
                    self.assertNotEqual(failed.returncode, 0)
                    self.assertFalse(list(Path(failed_root).glob("session.*")),
                                     "pre-allocation attach error retained a session")
                    slot = self.run_cmd(CTL, "vswitch", "show", "slots", switch, "3")
                    self.assertEqual(json.loads(slot.stdout)[0]["state"], "free")
                    self.run_cmd("ip", "-n", mgmt, "addr", "replace", "169.254.169.254/32",
                                 "dev", mgmt_dev)
                    self.run_cmd("ip", "-n", mgmt, "route", "replace", "198.19.240.0/20",
                                 "dev", mgmt_dev)
                    tap = switch + "-t4"
                    before = None
                    if shutil.which("ethtool"):
                        before = self.run_cmd("ip", "netns", "exec", switch,
                                              "ethtool", "-k", tap).stdout
                    result = self.run_cmd(CTL, "vswitch", "debug", "--state-root", state,
                                          "--port", "4", switch, "--", "ping", "-c", "1",
                                          "-W", "2", "169.254.169.254", check=False)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    slots = self.run_cmd(CTL, "vswitch", "show", "slots", switch, "3")
                    self.assertEqual(json.loads(slots.stdout)[0]["state"], "free")
                    self.assertFalse(list(Path(state).glob("session.*")))
                    if before is not None:
                        after = self.run_cmd("ip", "netns", "exec", switch,
                                             "ethtool", "-k", tap).stdout
                        self.assertEqual(after, before, "debug changed shared TAP features")
                finally:
                    self.run_cmd(CTL, "vswitch", "stop", switch, "--force", check=False)
            finally:
                self.run_cmd("ip", "netns", "del", switch, check=False)
                self.run_cmd("ip", "netns", "del", mgmt, check=False)

    def test_live_slot_cannot_be_reallocated_and_sigkill_recovers(self):
        if os.geteuid() != 0:
            self.skipTest("root is required")
        if not Path(CTL).is_file():
            self.skipTest("set CONNECTOR_CTL to the Go binary under test")
        with tempfile.TemporaryDirectory(prefix="debug-e2e-") as tmp:
            switch = f"dk{os.getpid() & 0xffff:04x}"
            state = str(Path(tmp) / "sessions")
            self.run_cmd("ip", "netns", "add", switch)
            proc = None
            try:
                self.run_cmd(CTL, "vswitch", "start", switch, "--netns=" + switch,
                             "--ports=4", "--mode=tap", "--mac-addr=02:db:ee:00:00:03",
                             "--floating-ip-base=198.19.248.0")
                proc = subprocess.Popen([CTL, "vswitch", "debug", "--state-root", state,
                                         "--port", "4", switch, "--", "sleep", "60"],
                                        stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
                deadline = time.monotonic() + 15
                while time.monotonic() < deadline:
                    slots = self.run_cmd(CTL, "vswitch", "show", "slots", switch, "3")
                    records = list(Path(state).glob("session.*/state.json"))
                    running = records and json.loads(records[0].read_text()).get("phase") == "running"
                    if json.loads(slots.stdout)[0]["state"] == "allocated" and running:
                        break
                    if proc.poll() is not None:
                        self.fail(f"debug exited early: {proc.stderr.read()}")
                    time.sleep(.1)
                else:
                    self.fail("debug did not allocate port 4")
                duplicate = self.run_cmd(CTL, "vswitch", "attach", switch,
                                         "--port", "4", "--inner-ip", "169.254.0.25", check=False)
                self.assertNotEqual(duplicate.returncode, 0, "live debug slot was reallocated")
                debug_duplicate = self.run_cmd(CTL, "vswitch", "debug", "--state-root",
                                               str(Path(tmp) / "duplicate-sessions"),
                                               "--port", "4", switch, "--", "/bin/true", check=False)
                self.assertNotEqual(debug_duplicate.returncode, 0, "second debug reused port 4")
                second = self.run_cmd(CTL, "vswitch", "debug", "--state-root",
                                      str(Path(tmp) / "second-sessions"), switch,
                                      "--", "/bin/true", check=False)
                self.assertEqual(second.returncode, 0, second.stderr)
                self.assertIn("auto-selected tail port=3", second.stderr)
                proc.kill()
                proc.wait(timeout=5)
                snapshot = next(Path(state).glob("session.*/offload.json"))
                saved_snapshot = snapshot.read_bytes()
                snapshot.unlink()
                blocked = self.run_cmd(CTL, "vswitch", "debug", "--state-root", state,
                                       "--cleanup-stale", check=False)
                self.assertNotEqual(blocked.returncode, 0, "missing offload snapshot released the slot")
                slots = self.run_cmd(CTL, "vswitch", "show", "slots", switch, "3")
                self.assertEqual(json.loads(slots.stdout)[0]["state"], "allocated")
                snapshot.write_bytes(saved_snapshot)
                recovered = self.run_cmd(CTL, "vswitch", "debug", "--state-root", state,
                                         "--cleanup-stale", check=False)
                self.assertEqual(recovered.returncode, 0, recovered.stderr)
                slots = self.run_cmd(CTL, "vswitch", "show", "slots", switch, "3")
                self.assertEqual(json.loads(slots.stdout)[0]["state"], "free")
                self.assertFalse(list(Path(state).glob("session.*")))
            finally:
                if proc is not None and proc.poll() is None:
                    proc.kill()
                    proc.wait(timeout=5)
                if proc is not None and proc.stderr is not None:
                    proc.stderr.close()
                self.run_cmd(CTL, "vswitch", "stop", switch, "--force", check=False)
                self.run_cmd("ip", "netns", "del", switch, check=False)

    def test_transit_gateway_geneve_between_two_debug_taps(self):
        if os.geteuid() != 0:
            self.skipTest("root is required")
        if not Path(CTL).is_file():
            self.skipTest("set CONNECTOR_CTL to the Go binary under test")
        suffix = f"{os.getpid() & 0xffff:04x}"
        a, b = "ga" + suffix, "gb" + suffix
        ta, tb = "ta" + suffix, "tb" + suffix
        with tempfile.TemporaryDirectory(prefix="debug-geneve-") as tmp:
            procs = []
            for ns in (a, b):
                self.run_cmd("ip", "netns", "add", ns)
            try:
                self.run_cmd("ip", "link", "add", ta, "type", "veth", "peer", "name", tb)
                for dev in (ta, tb):
                    self.run_cmd("ip", "link", "set", dev, "mtu", "1600")
                for sw, dev, local, peer, fip, mac in (
                    (a, ta, "10.0.0.1", "10.0.0.2", "198.19.252.0", "02:db:ee:00:00:04"),
                    (b, tb, "10.0.0.2", "10.0.0.1", "198.19.253.0", "02:db:ee:00:00:05"),
                ):
                    self.run_cmd(CTL, "vswitch", "start", sw, "--netns=" + sw,
                                 "--ports=1", "--mode=tap", "--mac-addr=" + mac,
                                 "--floating-ip-base=" + fip, "--transit-dev=" + dev,
                                 "--transit-dev-addr=" + local + "/24:" + peer,
                                 "--geneve-port-base=52000")
                b_state = str(Path(tmp) / "b-sessions")
                b_log = Path(tmp) / "b.log"
                with b_log.open("w") as log:
                    proc = subprocess.Popen([
                        CTL, "vswitch", "debug", "--state-root", b_state, "--port", "1",
                        "--cidr", "10.1.0.2/24", "--gateway", "10.1.0.254",
                        "--transit-gateway-ip", "10.0.0.1", "--transit-geneve-vni", "100",
                        b, "--", "sleep", "60"], stdout=log, stderr=log)
                    procs.append(proc)
                    deadline = time.monotonic() + 15
                    while time.monotonic() < deadline:
                        records = list(Path(b_state).glob("session.*/state.json"))
                        if records and json.loads(records[0].read_text()).get("phase") == "running":
                            break
                        if proc.poll() is not None:
                            self.fail("receiver debug exited: " + b_log.read_text())
                        time.sleep(.1)
                    else:
                        self.fail("receiver debug did not start")
                    ping = self.run_cmd(
                        CTL, "vswitch", "debug", "--state-root", str(Path(tmp) / "a-sessions"),
                        "--port", "1", "--cidr", "10.1.0.1/24", "--gateway", "10.1.0.254",
                        "--transit-gateway-ip", "10.0.0.2", "--transit-geneve-vni", "100",
                        a, "--", "ping", "-c", "2", "-W", "2", "10.1.0.2", check=False)
                    self.assertEqual(ping.returncode, 0, ping.stderr + ping.stdout)
                    stats = json.loads(self.run_cmd(CTL, "vswitch", "stats", b).stdout)["ports"][0]
                    self.assertGreater(stats["transit_rx_packets"], 0)
                    self.assertGreater(stats["transit_tx_packets"], 0)
            finally:
                for proc in procs:
                    if proc.poll() is None:
                        proc.kill()
                    proc.wait(timeout=5)
                if Path(tmp, "b-sessions").exists():
                    self.run_cmd(CTL, "vswitch", "debug", "--state-root",
                                 str(Path(tmp) / "b-sessions"), "--cleanup-stale", check=False)
                for sw in (a, b):
                    self.run_cmd(CTL, "vswitch", "stop", sw, "--force", check=False)
                self.run_cmd("ip", "link", "del", ta, check=False)
                for ns in (a, b):
                    self.run_cmd("ip", "netns", "del", ns, check=False)


if __name__ == "__main__":
    unittest.main()
