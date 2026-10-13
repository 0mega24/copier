"""Wire-level data-control tests against a private fake compositor socket.

No user's desktop clipboard is read or changed. This checks libwayland XML
descriptors, event dispatch, MIME offers and fd-based binary transfers.
"""
import array
import base64
import importlib.util
import json
import os
from pathlib import Path
import socket
import signal
import struct
import tempfile
import threading
import time
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("wayland_helper", ROOT / "wayland_clipboard.py")
wl = importlib.util.module_from_spec(spec)
spec.loader.exec_module(wl)
wl.PROTOCOL_XMLS = [(ROOT / "protocols" / name).read_text() for name in ("wlr-data-control.xml", "ext-data-control.xml")]
wl.MAX = 32 * 1024 * 1024
wl.base64 = base64


def uint(n):
    return struct.pack("=I", n)


def string(s):
    b = s.encode() + b"\0"
    return uint(len(b)) + b + b"\0" * (-len(b) % 4)


def parse_string(data, offset=0):
    n, = struct.unpack_from("=I", data, offset)
    return data[offset + 4:offset + 4 + n - 1].decode(), offset + 4 + (n + 3) // 4 * 4


class FakeCompositor:
    def __init__(self, path, prefix):
        self.prefix = prefix
        self.listener = socket.socket(socket.AF_UNIX)
        self.listener.bind(path)
        self.listener.listen()
        self.error = None
        self.owned = {}
        self.send_lock = threading.Lock()
        self.formats = {"text/plain;charset=utf-8": b"hello\0world", "image/png": b"\x89PNG\0\xff",
                        "text/html": b"<b>hello</b>", "application/x-custom": bytes(range(256))}
        self.thread = threading.Thread(target=self.run, daemon=True)
        self.thread.start()

    def send(self, obj, opcode, body=b"", fd=None):
        message = uint(obj) + uint(((8 + len(body)) << 16) | opcode) + body
        with self.send_lock:
            if fd is None:
                self.conn.sendall(message)
            else:
                self.conn.sendmsg([message], [(socket.SOL_SOCKET, socket.SCM_RIGHTS, array.array("i", [fd]))])

    def run(self):
        try:
            self.conn, _ = self.listener.accept()
            objects = {1: "display"}
            buffered, fds = bytearray(), []
            next_server_id = 0xff000000
            while True:
                data, anc, _, _ = self.conn.recvmsg(65536, socket.CMSG_SPACE(256))
                if not data:
                    return
                buffered.extend(data)
                for level, kind, raw in anc:
                    if level == socket.SOL_SOCKET and kind == socket.SCM_RIGHTS:
                        values = array.array("i")
                        values.frombytes(raw[:len(raw) // values.itemsize * values.itemsize])
                        fds.extend(values)
                while len(buffered) >= 8:
                    obj, header = struct.unpack_from("=II", buffered)
                    size, opcode = header >> 16, header & 65535
                    if len(buffered) < size:
                        break
                    body = bytes(buffered[8:size])
                    del buffered[:size]
                    kind = objects[obj]
                    if kind == "display" and opcode == 1:
                        reg, = struct.unpack("=I", body)
                        objects[reg] = "registry"
                        self.send(reg, 0, uint(1) + string("wl_seat") + uint(1))
                        self.send(reg, 0, uint(2) + string(self.prefix + "_manager_v1") + uint(1 if self.prefix == "ext_data_control" else 2))
                    elif kind == "display" and opcode == 0:
                        callback, = struct.unpack("=I", body)
                        self.send(callback, 0, uint(123))
                        self.send(1, 1, uint(callback))
                    elif kind == "registry":
                        number, = struct.unpack_from("=I", body)
                        name, offset = parse_string(body, 4)
                        version, new_id = struct.unpack_from("=II", body, offset)
                        objects[new_id] = "seat" if number == 1 else "manager"
                        if number == 1:
                            self.send(new_id, 0, uint(0))
                    elif kind == "manager" and opcode == 1:
                        device, seat = struct.unpack("=II", body)
                        objects[device] = "device"
                        offer = next_server_id
                        next_server_id += 1
                        objects[offer] = "offer"
                        self.send(device, 0, uint(offer))
                        for name in self.formats:
                            self.send(offer, 0, string(name))
                        self.send(device, 1, uint(offer))
                    elif kind == "manager" and opcode == 0:
                        source, = struct.unpack("=I", body)
                        objects[source] = "source"
                        self.source_names = []
                    elif kind == "source" and opcode == 0:
                        name, _ = parse_string(body)
                        self.source_names.append(name)
                    elif kind == "device" and opcode == 0:
                        source, = struct.unpack("=I", body)
                        self.receivers = []
                        for name in self.source_names:
                            rd, wr = os.pipe()
                            self.send(source, 0, string(name), wr)
                            os.close(wr)
                            def receive(name=name, rd=rd):
                                raw = bytearray()
                                while chunk := os.read(rd, 65536):
                                    raw.extend(chunk)
                                os.close(rd)
                                self.owned[name] = bytes(raw)
                            thread = threading.Thread(target=receive, daemon=True)
                            thread.start()
                            self.receivers.append(thread)
                        def cancel():
                            for thread in self.receivers:
                                thread.join(3)
                            time.sleep(0.1)
                            self.send(source, 1)
                        threading.Thread(target=cancel, daemon=True).start()
                    elif kind == "offer" and opcode == 0:
                        name, _ = parse_string(body)
                        fd = fds.pop(0)
                        os.write(fd, self.formats[name])
                        os.close(fd)
                    else:
                        raise AssertionError((kind, opcode, body))
        except Exception as exc:
            self.error = exc
        finally:
            if hasattr(self, "conn"):
                self.conn.close()
            self.listener.close()


class DataControlTests(unittest.TestCase):
    def setUp(self):
        signal.alarm(15)

    def tearDown(self):
        signal.alarm(0)

    def test_arbitrary_formats_both_protocols(self):
        for prefix in ("ext_data_control", "zwlr_data_control"):
            with self.subTest(prefix=prefix), tempfile.TemporaryDirectory() as td:
                compositor = FakeCompositor(str(Path(td) / "test-wayland"), prefix)
                old = {key: os.environ.get(key) for key in ("XDG_RUNTIME_DIR", "WAYLAND_DISPLAY")}
                os.environ.update(XDG_RUNTIME_DIR=td, WAYLAND_DISPLAY="test-wayland")
                client = None
                try:
                    client = wl.WaylandClipboard("clipboard")
                    packet = client.capture()
                    self.assertEqual(packet["mime"], "text/plain;charset=utf-8")
                    self.assertEqual({k: base64.b64decode(v["data"]) for k, v in packet["formats"].items()}, compositor.formats)
                    rd, wr = os.pipe()
                    try:
                        client.own(packet, wr)
                        self.assertEqual(os.read(rd, 1), b"1")
                        self.assertEqual(compositor.owned, compositor.formats)
                    finally:
                        os.close(rd)
                finally:
                    if client:
                        client.close()
                    for key, value in old.items():
                        if value is None:
                            os.environ.pop(key, None)
                        else:
                            os.environ[key] = value
                    compositor.thread.join(3)
                if compositor.error:
                    raise compositor.error


if __name__ == "__main__":
    unittest.main()
