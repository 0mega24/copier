"""Full binary smoke test: relay, CLI, persistence, conflict recovery and UI."""
import base64
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
import unittest
import urllib.request
import signal
from test_wayland_protocol import FakeCompositor

ROOT = Path(__file__).resolve().parents[1]


class CLITests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.build_dir = tempfile.TemporaryDirectory()
        cls.binary = str(Path(cls.build_dir.name) / "copier")
        subprocess.run(["go", "build", "-o", cls.binary, "."], cwd=ROOT, check=True, timeout=60)

    @classmethod
    def tearDownClass(cls):
        cls.build_dir.cleanup()

    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        root = Path(self.directory.name)
        self.env = {k: v for k, v in os.environ.items() if k not in ("DISPLAY", "WAYLAND_DISPLAY", "COPIER_URL", "COPIER_BOARD", "COPIER_TOKEN")}
        self.env.update(COPIER_TOKEN="0123456789abcdef0123456789abcdef",
                        XDG_CONFIG_HOME=str(root / "config"), XDG_STATE_HOME=str(root / "state"))
        with socket.socket() as s:
            s.bind(("127.0.0.1", 0))
            self.port = s.getsockname()[1]
        self.url = f"http://127.0.0.1:{self.port}"
        self.server = None
        self.addCleanup(self.stop_server)
        self.addCleanup(self.stop_helpers)
        self.start_server()
        self.command("join", self.url)

    def start_server(self):
        self.server = subprocess.Popen([self.binary, "serve", "--listen", f"127.0.0.1:{self.port}"], env=self.env, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        request = urllib.request.Request(self.url + "/v1/boards/default", headers={"Authorization": "Bearer " + self.env["COPIER_TOKEN"]})
        for _ in range(50):
            if self.server.poll() is not None:
                self.fail(self.server.stderr.read().decode())
            try:
                with urllib.request.urlopen(request, timeout=0.2):
                    return
            except OSError:
                time.sleep(0.02)
        self.fail("relay did not become ready")

    def stop_server(self):
        if self.server:
            self.server.terminate()
            try:
                self.server.wait(timeout=6)
            except subprocess.TimeoutExpired:
                self.server.kill()
                self.server.wait()
            self.server.stderr.close()
            self.server = None

    def stop_helpers(self):
        for record in Path(self.directory.name).rglob("*.pid.json"):
            try:
                pid = json.loads(record.read_text())["pid"]
                os.kill(pid, signal.SIGTERM)
            except (OSError, ValueError, KeyError):
                pass

    def command(self, *args, data=None, code=0):
        result = subprocess.run([self.binary, *args], input=data, env=self.env, capture_output=True, timeout=15)
        self.assertEqual(result.returncode, code, result.stderr.decode())
        return result

    def test_desktop_server_laptop_cli_and_restart(self):
        self.command("copy", data=b"desktop\0copy")
        self.assertEqual(self.command("paste").stdout, b"desktop\0copy")
        self.command("copy", data=b"server answer")
        self.assertEqual(self.command("paste").stdout, b"server answer")
        self.assertEqual(self.command("paste", "--revision", "1").stdout, b"desktop\0copy")
        png = base64.b64decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aP14AAAAASUVORK5CYII=")
        path = Path(self.directory.name) / "screenshot.png"
        path.write_bytes(png)
        self.command("copy", "--file", str(path))
        self.assertEqual(self.command("paste", "--list-types").stdout, b"image/png\n")
        self.assertEqual(self.command("wl-paste", "--type", "image/png").stdout, png)
        self.command("copy", "--base", "0", data=b"conflicting copy", code=3)
        pending = list((Path(self.env["XDG_STATE_HOME"]) / "copier" / "pending").glob("*.json"))
        self.assertEqual(len(pending), 1)
        self.command("retry", "--file", str(pending[0]), code=3)
        self.command("retry", "--file", str(pending[0]), "--force")
        self.assertEqual(self.command("paste").stdout, b"conflicting copy")
        self.command("wl-copy", "wrapper", "copy")
        self.assertEqual(self.command("xclip", "-selection", "clipboard", "-o").stdout, b"wrapper copy")
        ui = self.command("ui", data=b"p\nh\nq\n")
        self.assertIn(b"wrapper copy", ui.stdout)
        before = json.loads(self.command("history", "--json").stdout)
        self.stop_server()
        self.start_server()
        after = json.loads(self.command("history", "--json").stdout)
        self.assertEqual(before, after)
        self.assertEqual(self.command("paste").stdout, b"wrapper copy")

    def test_file_objects_cross_machine(self):
        source = Path(self.directory.name) / "source"
        source.mkdir()
        file = source / "attachment 世界.txt"
        file.write_bytes(b"actual file bytes\x00binary")
        folder = source / "folder"
        folder.mkdir()
        (folder / "nested.txt").write_text("nested file")
        self.command("copy", "--files", str(file), str(folder))
        self.assertIn(b"text/uri-list", self.command("paste", "--list-types").stdout)
        # No source bytes are uploaded by copy or by local paste/list-types.
        cache = Path(self.env["XDG_STATE_HOME"]) / "copier" / "relay.json.chunks"
        self.assertEqual(list(cache.rglob("*.bin")), [])
        self.env["XDG_STATE_HOME"] = str(Path(self.directory.name) / "receiver-state")
        destination = Path(self.directory.name) / "destination"
        self.command("paste", "--files", str(destination))
        self.assertEqual((destination / "attachment 世界.txt").read_bytes(), b"actual file bytes\x00binary")
        self.assertEqual((destination / "folder" / "nested.txt").read_text(), "nested file")
        self.command("paste", "--files", str(destination), code=1)

    def test_wayland_file_manager_to_native_receiver(self):
        file = Path(self.directory.name) / "a screenshot.png"
        original = base64.b64decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aP14AAAAASUVORK5CYII=")
        file.write_bytes(original)
        runtime = Path(self.directory.name) / "wayland"
        runtime.mkdir()
        self.env.update(XDG_RUNTIME_DIR=str(runtime), WAYLAND_DISPLAY="test-wayland")
        compositor = FakeCompositor(str(runtime / "test-wayland"), "ext_data_control")
        compositor.formats = {"text/uri-list": (file.as_uri() + "\r\n").encode(),
                              "x-special/gnome-copied-files": ("copy\n" + file.as_uri()).encode()}
        self.command("copy", "--from-clipboard", "--backend", "wayland")
        compositor.thread.join(3)
        if compositor.error:
            raise compositor.error
        self.env["XDG_STATE_HOME"] = str(Path(self.directory.name) / "receiver-state")
        (runtime / "test-wayland").unlink()
        receiver = FakeCompositor(str(runtime / "test-wayland"), "zwlr_data_control")
        self.command("paste", "--local", "--backend", "wayland")
        receiver.thread.join(3)
        if receiver.error:
            raise receiver.error
        self.assertIn("text/uri-list", receiver.owned)
        cache = Path(self.directory.name) / "state" / "copier" / "relay.json.chunks"
        self.assertEqual(list(cache.rglob("*.bin")), [])
        from urllib.parse import urlparse, unquote
        destination = Path(unquote(urlparse(receiver.owned["text/uri-list"].decode().strip()).path))
        self.assertNotEqual(destination, file)
        self.assertEqual(destination.read_bytes(), original)
        self.assertTrue(receiver.owned["x-special/gnome-copied-files"].startswith(b"copy\n"))

    def test_three_gib_native_paste_is_lazy(self):
        file = Path(self.directory.name) / "large.iso"
        size = 3 * 1024 ** 3
        tail = b"large-file-tail!"
        with file.open("wb") as out:
            out.truncate(size)
            out.seek(size - len(tail))
            out.write(tail)
        self.command("copy", "--files", str(file))
        cache = Path(self.env["XDG_STATE_HOME"]) / "copier" / "relay.json.chunks"
        self.assertEqual(list(cache.rglob("*.bin")), [])
        metadata = json.loads(self.command("history", "--json").stdout)[-1]
        offer = json.loads(base64.b64decode(metadata["data"]))
        self.assertEqual(offer["entries"][0]["size"], size)
        self.assertNotIn(str(file), json.dumps(offer))
        self.env["XDG_STATE_HOME"] = str(Path(self.directory.name) / "receiver-state")
        runtime = Path(self.directory.name) / "wayland"
        runtime.mkdir()
        self.env.update(XDG_RUNTIME_DIR=str(runtime), WAYLAND_DISPLAY="test-wayland")
        receiver = FakeCompositor(str(runtime / "test-wayland"), "ext_data_control")
        self.command("paste", "--local", "--backend", "wayland")
        receiver.thread.join(3)
        if receiver.error:
            raise receiver.error
        self.assertEqual(list(cache.rglob("*.bin")), [])
        from urllib.parse import urlparse, unquote
        path = Path(unquote(urlparse(receiver.owned["text/uri-list"].decode().strip()).path))
        self.assertEqual(path.stat().st_size, size)
        self.assertEqual(list(cache.rglob("*.bin")), [])
        with path.open("rb") as inp:
            inp.seek(size - len(tail))
            self.assertEqual(inp.read(len(tail)), tail)
        transferred = list(cache.rglob("*.bin"))
        self.assertEqual(sum(p.stat().st_size for p in transferred), 1024 ** 2)

    def test_local_only_requires_no_relay_or_token(self):
        file = Path(self.directory.name) / "local.txt"
        file.write_text("local file")
        self.env.pop("COPIER_TOKEN", None)
        self.env["XDG_CONFIG_HOME"] = str(Path(self.directory.name) / "empty-config")
        runtime = Path(self.directory.name) / "wayland"
        runtime.mkdir()
        self.env.update(XDG_RUNTIME_DIR=str(runtime), WAYLAND_DISPLAY="test-wayland")
        receiver = FakeCompositor(str(runtime / "test-wayland"), "ext_data_control")
        self.command("copy", "--local-only", "--files", "--backend", "wayland", str(file))
        receiver.thread.join(3)
        if receiver.error:
            raise receiver.error
        self.assertIn(file.as_uri().encode(), receiver.owned["text/uri-list"])
        self.assertEqual(list((Path(self.directory.name) / "state" / "copier" / "relay.json.chunks").rglob("*.bin")), [])

    def test_remote_source_change_is_rejected(self):
        file = Path(self.directory.name) / "changes.bin"
        file.write_bytes(b"original")
        self.command("copy", "--files", str(file))
        file.write_bytes(b"modified")
        self.env["XDG_STATE_HOME"] = str(Path(self.directory.name) / "receiver-state")
        destination = Path(self.directory.name) / "received"
        self.command("paste", "--files", str(destination), code=1)
        self.assertFalse(destination.exists())


if __name__ == "__main__":
    unittest.main()
