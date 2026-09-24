#!/usr/bin/env python3
"""Temporary two-TAP proof of concept against a connector TAPFD/1 switch.

This is deliberately a foreground diagnostic. It allocates one free TAP slot,
creates one disposable debug netns/TAP, probes the management VIP, and releases
only the slot it allocated. It never changes an existing sandbox port.
"""

import array
import ctypes
import fcntl
import json
import os
from pathlib import Path
import selectors
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time


SWITCH = "e2br0919"
SOCKET = "/data/kuasar-release-20260919/work/run/connector/e2br0919/tapfd.sock"
CONNECTOR = "/data/kuasar-release-20260919/bins/connector-ctl"
GUEST_CIDR = "169.254.0.21/30"
GATEWAY = "169.254.0.22"
VIP = "169.254.169.254"
VNET_HDR_SIZE = 12
TUNSETIFF = 0x400454CA
TUNSETVNETHDRSZ = 0x400454D8
IFF_TAP = 0x0002
IFF_NO_PI = 0x1000
CLONE_NEWNET = 0x40000000


def command(*args):
    return subprocess.run(args, check=True, text=True, capture_output=True).stdout.strip()


def tapfd_request(line, need_fd=False):
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
        client.settimeout(5)
        client.connect(SOCKET)
        client.sendall((line + "\n").encode())
        if need_fd:
            data, anc, _, _ = client.recvmsg(4096, socket.CMSG_SPACE(4 * 4))
            fds = array.array("i")
            for level, kind, payload in anc:
                if level == socket.SOL_SOCKET and kind == socket.SCM_RIGHTS:
                    fds.frombytes(payload[: len(payload) // fds.itemsize * fds.itemsize])
            if not data.startswith(b"TAPFD/1 OK ") or len(fds) != 1:
                for fd in fds:
                    os.close(fd)
                raise RuntimeError(f"TAPFD OPEN: {data!r}")
            return parse_fields(data), fds[0]
        data = client.recv(4096)
        if not data.startswith(b"TAPFD/1 OK "):
            raise RuntimeError(f"TAPFD request: {data!r}")
        return parse_fields(data)


def parse_fields(data):
    return dict(token.split("=", 1) for token in data.decode(errors="replace").split() if "=" in token)


def create_debug_tap(namespace):
    libc = ctypes.CDLL(None, use_errno=True)
    original = os.open("/proc/self/ns/net", os.O_RDONLY)
    target = os.open(f"/var/run/netns/{namespace}", os.O_RDONLY)
    fd = None
    try:
        if libc.setns(target, CLONE_NEWNET) != 0:
            raise OSError(ctypes.get_errno(), "enter debug netns")
        try:
            fd = os.open("/dev/net/tun", os.O_RDWR | os.O_CLOEXEC)
            ifreq = struct.pack("16sH22x", b"dbg0", IFF_TAP | IFF_NO_PI)
            fcntl.ioctl(fd, TUNSETIFF, ifreq)
            os.set_blocking(fd, False)
            return fd
        finally:
            if libc.setns(original, CLONE_NEWNET) != 0:
                raise OSError(ctypes.get_errno(), "restore original netns")
    except BaseException:
        if fd is not None:
            os.close(fd)
        raise
    finally:
        os.close(target)
        os.close(original)


def relay(debug_fd, switch_fd, stop, counts):
    selector = selectors.DefaultSelector()
    selector.register(debug_fd, selectors.EVENT_READ, "debug_to_switch")
    selector.register(switch_fd, selectors.EVENT_READ, "switch_to_debug")
    while not stop.is_set():
        for key, _ in selector.select(timeout=0.2):
            if key.data == "debug_to_switch":
                frame = os.read(debug_fd, 65535)
                if frame:
                    written = os.write(switch_fd, bytes(VNET_HDR_SIZE) + frame)
                    if written != len(frame) + VNET_HDR_SIZE:
                        raise RuntimeError("short write to switch TAP")
                    counts[0] += 1
            else:
                data = os.read(switch_fd, 65535 + VNET_HDR_SIZE)
                if len(data) > VNET_HDR_SIZE:
                    frame = bytearray(data[VNET_HDR_SIZE:])
                    flags, gso_type, _hdr_len, _gso_size, csum_start, csum_offset, _num_buffers = struct.unpack_from("<BBHHHHH", data)
                    if gso_type != 0:
                        raise RuntimeError(f"unsupported GSO frame type {gso_type}")
                    if flags & 1:
                        checksum_pos = csum_start + csum_offset
                        if checksum_pos + 2 > len(frame):
                            raise RuntimeError("virtio checksum offset is outside frame")
                        payload = frame[csum_start:]
                        if len(payload) % 2:
                            payload += b"\x00"
                        total = sum(struct.unpack(f"!{len(payload) // 2}H", payload))
                        while total >> 16:
                            total = (total & 0xFFFF) + (total >> 16)
                        struct.pack_into("!H", frame, checksum_pos, (~total) & 0xFFFF)
                        counts[2] += 1
                    written = os.write(debug_fd, frame)
                    if written != len(frame):
                        raise RuntimeError("short write to debug TAP")
                    counts[1] += 1


def main():
    namespace = f"tap-poc-{os.getpid()}"
    port = None
    switch_fd = None
    debug_fd = None
    netns_created = False
    stop = threading.Event()
    worker = None
    server = None
    serve_dir = None
    capture = None
    capture_path = f"/tmp/{namespace}-mgmt.pcap"
    floating_ip = None
    counts = [0, 0, 0]
    worker_error = []

    def on_signal(_signum, _frame):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, on_signal)
    try:
        before = json.loads(command(CONNECTOR, "vswitch", "show", "slots", SWITCH))
        allocated = {entry["port"] for entry in before if entry["state"] == "allocated"}
        print(f"existing allocated ports: {sorted(allocated)}", flush=True)
        prepared = tapfd_request(f"TAPFD/1 PREPARE VSWITCH={SWITCH} INNER_IP={GUEST_CIDR.split('/')[0]}")
        port = int(prepared["port"])
        floating_ip = prepared["floating_ip"]
        if port in allocated:
            raise RuntimeError(f"PREPARE returned already allocated port {port}")
        print(f"allocated debug port {port}, floating_ip={floating_ip}", flush=True)
        opened, switch_fd = tapfd_request(f"TAPFD/1 OPEN VSWITCH={SWITCH} PORT={port}", need_fd=True)
        fcntl.ioctl(switch_fd, TUNSETVNETHDRSZ, struct.pack("i", VNET_HDR_SIZE))
        print(f"opened TAP fd, mac={opened['mac']}", flush=True)
        command("ip", "netns", "add", namespace)
        netns_created = True
        debug_fd = create_debug_tap(namespace)
        command("ip", "-n", namespace, "link", "set", "dbg0", "address", opened["mac"])
        command("ip", "-n", namespace, "addr", "add", GUEST_CIDR, "dev", "dbg0")
        command("ip", "-n", namespace, "link", "set", "lo", "up")
        command("ip", "-n", namespace, "link", "set", "dbg0", "up")
        command("ip", "-n", namespace, "route", "add", "default", "via", GATEWAY, "dev", "dbg0")

        def worker_main():
            try:
                relay(debug_fd, switch_fd, stop, counts)
            except BaseException as error:
                worker_error.append(error)

        worker = threading.Thread(target=worker_main, daemon=True)
        worker.start()
        print("running ICMP probe", flush=True)
        print(command("ip", "netns", "exec", namespace, "ping", "-c", "2", "-W", "2", VIP), flush=True)
        serve_dir = tempfile.TemporaryDirectory(prefix="tap-poc-http-")
        (Path(serve_dir.name) / "ok.txt").write_text("tap-poc-ok\n")
        server = subprocess.Popen(
            ["ip", "netns", "exec", f"{SWITCH}_mgmt", "python3", "-m", "http.server", "18080",
             "--bind", VIP, "--directory", serve_dir.name],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        time.sleep(0.2)
        if server.poll() is not None:
            raise RuntimeError(f"management HTTP server exited {server.returncode}")
        print("management listener self-check: " + command(
            "ip", "netns", "exec", f"{SWITCH}_mgmt", "curl", "--noproxy", "*",
            "-fsS", "--max-time", "2", f"http://{VIP}:18080/ok.txt",
        ), flush=True)
        capture = subprocess.Popen(
            ["ip", "netns", "exec", f"{SWITCH}_mgmt", "tcpdump", "-n", "-i", f"{SWITCH}m0",
             "-w", capture_path, "host", floating_ip],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
        )
        time.sleep(0.2)
        print("running controlled management HTTP probe", flush=True)
        print(command(
            "ip", "netns", "exec", namespace, "curl", "--noproxy", "*",
            "-fsS", "--retry", "2", "--retry-delay", "1", "--max-time", "3",
            f"http://{VIP}:18080/ok.txt",
        ), flush=True)
        print("running MMDS token probe (unregistered debug port should receive HTTP 503)", flush=True)
        mmds = subprocess.run((
            "ip", "netns", "exec", namespace, "curl", "--noproxy", "*",
            "-sS", "-X", "PUT", "-H", "X-metadata-token-ttl-seconds: 60",
            "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "3",
            f"http://{VIP}/latest/api/token",
        ), text=True, capture_output=True)
        print(f"MMDS token probe: curl_exit={mmds.returncode} HTTP={mmds.stdout} stderr={mmds.stderr.strip()}", flush=True)
        if worker_error:
            raise RuntimeError(f"relay failed: {worker_error[0]}")
    finally:
        stop.set()
        if worker is not None:
            worker.join(timeout=1)
        print(f"relay frames debug->switch={counts[0]} switch->debug={counts[1]} checksum_completed={counts[2]}", flush=True)
        if port is not None:
            stats = subprocess.run([CONNECTOR, "vswitch", "stats", SWITCH, f"--port={port}"], text=True, capture_output=True)
            print(f"port stats: {stats.stdout.strip() or stats.stderr.strip()}", flush=True)
        if server is not None:
            server.terminate()
            try:
                server.wait(timeout=2)
            except subprocess.TimeoutExpired:
                server.kill()
                server.wait()
        if capture is not None:
            capture.terminate()
            try:
                capture.wait(timeout=2)
            except subprocess.TimeoutExpired:
                capture.kill()
                capture.wait()
            print(f"management capture: {capture_path}", flush=True)
        if serve_dir is not None:
            serve_dir.cleanup()
        if debug_fd is not None:
            os.close(debug_fd)
        if switch_fd is not None:
            os.close(switch_fd)
        if netns_created:
            subprocess.run(["ip", "netns", "del", namespace], check=False)
        if port is not None:
            try:
                print(tapfd_request(f"TAPFD/1 RELEASE VSWITCH={SWITCH} PORT={port}"), flush=True)
            except Exception as error:
                print(f"RELEASE FAILED for port {port}: {error}", file=sys.stderr)


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, subprocess.CalledProcessError, KeyboardInterrupt) as error:
        print(f"PoC failed: {error}", file=sys.stderr)
        sys.exit(1)
