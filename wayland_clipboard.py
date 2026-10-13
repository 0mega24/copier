"""Minimal libwayland data-control client for arbitrary MIME bundles.

Protocol descriptors are generated from the bundled upstream XML at runtime.
Both ext-data-control and wlr-data-control are supported. No focused window or
keyboard input is needed. Unsupported compositors may use the GTK XWayland path.
"""
import ctypes as C
import ctypes.util
import os
import select
import sys
import threading
import time
import xml.etree.ElementTree as ET


class ProtocolUnavailable(RuntimeError):
    pass


class WLInterface(C.Structure):
    pass


class WLMessage(C.Structure):
    _fields_ = [("name", C.c_char_p), ("signature", C.c_char_p),
                ("types", C.POINTER(C.POINTER(WLInterface)))]


WLInterface._fields_ = [("name", C.c_char_p), ("version", C.c_int),
                        ("method_count", C.c_int), ("methods", C.POINTER(WLMessage)),
                        ("event_count", C.c_int), ("events", C.POINTER(WLMessage))]


class WaylandClipboard:
    def __init__(self, selection):
        self.lib = C.CDLL(ctypes.util.find_library("wayland-client") or "libwayland-client.so.0")
        self.keep, self.callbacks, self.offers, self.globals = [], [], {}, {}
        self.selection, self.current, self.generation, self.cancelled = selection, None, 0, False
        self.error = None
        def fn(name, result, *args):
            f = getattr(self.lib, name)
            f.restype, f.argtypes = result, args
        p, u, i = C.c_void_p, C.c_uint, C.c_int
        fn("wl_display_connect", p, C.c_char_p)
        fn("wl_display_disconnect", None, p)
        fn("wl_display_roundtrip", i, p)
        fn("wl_display_dispatch", i, p)
        fn("wl_display_flush", i, p)
        fn("wl_display_get_fd", i, p)
        fn("wl_proxy_get_version", u, p)
        fn("wl_proxy_marshal_flags", p, p, u, C.POINTER(WLInterface), u, u)
        fn("wl_proxy_add_listener", i, p, C.POINTER(p), p)
        self.interfaces = {n: WLInterface.in_dll(self.lib, n + "_interface")
                           for n in ("wl_registry", "wl_seat")}
        self.load_protocols()
        self.display = self.lib.wl_display_connect(None)
        if not self.display:
            raise ProtocolUnavailable("cannot connect to Wayland")
        self.registry = self.marshal(self.display, 1, "wl_registry", None)
        self.listen(self.registry, [(self.global_event, [u, C.c_char_p, u]),
                                    (lambda *_: None, [u])])
        self.roundtrip()
        prefix = next((x for x in ("ext_data_control", "zwlr_data_control")
                       if x + "_manager_v1" in self.globals), None)
        if not prefix or "wl_seat" not in self.globals:
            self.close()
            raise ProtocolUnavailable("compositor lacks ext/wlr data-control; use XWayland or a compositor clipboard extension")
        self.prefix = prefix
        self.manager = self.bind(prefix + "_manager_v1", 1 if prefix == "ext_data_control" else 2)
        self.seat = self.bind("wl_seat", 1)
        # wl_seat emits capabilities even when no input devices are requested.
        self.listen(self.seat, [(lambda *_: None, [u])])
        self.device = self.marshal(self.manager, 1, prefix + "_device_v1", None, p(self.seat))
        self.listen(self.device, [(self.offer_event, [p]), (self.selection_event, [p]),
                                  (self.finished, []), (self.primary_event, [p])])
        self.roundtrip()
        self.roundtrip()
        if selection == "primary" and self.lib.wl_proxy_get_version(self.device) < 2 and prefix != "ext_data_control":
            self.close()
            raise ProtocolUnavailable("compositor lacks primary data-control support")

    def load_protocols(self):
        roots = [ET.fromstring(xml) for xml in PROTOCOL_XMLS]
        for root in roots:
            for node in root.findall("interface"):
                self.interfaces[node.attrib["name"]] = WLInterface(name=node.attrib["name"].encode(), version=int(node.attrib["version"]))
        codes = {"int": "i", "uint": "u", "fixed": "f", "string": "s", "object": "o", "new_id": "n", "array": "a", "fd": "h"}
        for root in roots:
            for node in root.findall("interface"):
                interface = self.interfaces[node.attrib["name"]]
                for tag, count, field in (("request", "method_count", "methods"), ("event", "event_count", "events")):
                    nodes = node.findall(tag)
                    messages = (WLMessage * len(nodes))()
                    for index, message in enumerate(nodes):
                        args = message.findall("arg")
                        types = (C.POINTER(WLInterface) * len(args))()
                        signature = message.attrib.get("since", "")
                        for j, arg in enumerate(args):
                            signature += ("?" if arg.attrib.get("allow-null") == "true" else "") + codes[arg.attrib["type"]]
                            if "interface" in arg.attrib:
                                types[j] = C.pointer(self.interfaces[arg.attrib["interface"]])
                        messages[index] = WLMessage(message.attrib["name"].encode(), signature.encode(), types)
                        self.keep.append(types)
                    setattr(interface, count, len(nodes))
                    setattr(interface, field, messages)
                    self.keep.append(messages)

    def marshal(self, proxy, opcode, constructor=None, *args):
        interface = C.pointer(self.interfaces[constructor]) if constructor else None
        version = self.lib.wl_proxy_get_version(proxy)
        if constructor:
            version = min(version, self.interfaces[constructor].version)
        return self.lib.wl_proxy_marshal_flags(proxy, opcode, interface, version, 0, *args)

    def listen(self, proxy, events):
        funcs = []
        for handler, argtypes in events:
            def wrapped(*args, handler=handler):
                try:
                    handler(*args[2:])
                except Exception as exc:
                    self.error = exc
            funcs.append(C.CFUNCTYPE(None, C.c_void_p, C.c_void_p, *argtypes)(wrapped))
        array = (C.c_void_p * len(funcs))(*(C.cast(f, C.c_void_p) for f in funcs))
        self.callbacks.extend(funcs)
        self.keep.append(array)
        if self.lib.wl_proxy_add_listener(proxy, array, None) != 0:
            raise RuntimeError("Wayland listener rejected")

    def global_event(self, number, name, version):
        self.globals.setdefault(name.decode(), (number, version))

    def bind(self, name, version):
        number, available = self.globals[name]
        return self.lib.wl_proxy_marshal_flags(self.registry, 0, C.pointer(self.interfaces[name]),
                                               min(version, available), 0, C.c_uint(number),
                                               C.c_char_p(name.encode()), C.c_uint(min(version, available)), None)

    def roundtrip(self):
        if self.lib.wl_display_roundtrip(self.display) < 0:
            raise RuntimeError("Wayland connection lost")
        if self.error:
            raise self.error

    def offer_event(self, proxy):
        self.offers[proxy] = []
        self.listen(proxy, [(lambda name: self.offers[proxy].append(name.decode()), [C.c_char_p])])

    def selection_event(self, proxy):
        if self.selection == "clipboard":
            self.current = proxy
            self.generation += 1

    def primary_event(self, proxy):
        if self.selection == "primary":
            self.current = proxy
            self.generation += 1

    def finished(self):
        self.error = RuntimeError("compositor revoked data-control access")
        self.cancelled = True

    def capture(self):
        if not self.current:
            return {"mime": "text/plain", "data": ""}
        generation = self.generation
        names = sorted(set(self.offers[self.current]))
        offer_mime = "application/vnd.copier.offer+json"
        local_mime = "application/vnd.copier.local-only"
        if local_mime in names and sys.argv[1] != "read-publish":
            names = [local_mime]
        elif offer_mime in names:
            names = [offer_mime]
        else:
            names = [name for name in names if name != local_mime]
        if not names or len(names) > 128:
            raise RuntimeError("clipboard advertises no targets or too many targets")
        formats, size = {}, 0
        for name in names:
            rd, wr = os.pipe()
            try:
                self.marshal(self.current, 0, None, C.c_char_p(name.encode()), C.c_int(wr))
                self.lib.wl_display_flush(self.display)
                os.close(wr)
                wr = -1
                data = bytearray()
                deadline = time.monotonic() + 8
                while True:
                    timeout = max(0, deadline - time.monotonic())
                    if not timeout or not select.select([rd], [], [], timeout)[0]:
                        raise RuntimeError("clipboard target timed out: " + name)
                    chunk = os.read(rd, 65536)
                    if not chunk:
                        break
                    data.extend(chunk)
                    if size + len(data) > MAX:
                        raise RuntimeError("clipboard bundle exceeds 32 MiB")
                size += len(data)
                formats[name] = {"data": base64.b64encode(data).decode(), "type": name, "bits": 8}
            finally:
                os.close(rd)
                if wr >= 0:
                    os.close(wr)
        self.roundtrip()
        if self.generation != generation:
            raise RuntimeError("clipboard changed during capture; retry")
        if names == [offer_mime]:
            return {"mime": offer_mime, "data": formats[offer_mime]["data"]}
        if names == [local_mime]:
            return {"mime": local_mime, "data": formats[local_mime]["data"]}
        preferred = next((n for n in ("text/plain;charset=utf-8", "UTF8_STRING", "text/plain", "image/png", "text/html") if n in formats), names[0])
        if size + len(base64.b64decode(formats[preferred]["data"])) > MAX:
            raise RuntimeError("clipboard bundle exceeds 32 MiB including default representation")
        return {"mime": preferred, "data": formats[preferred]["data"], "formats": formats}

    def own(self, packet, ready_fd):
        formats = packet.get("formats") or {packet["mime"]: {"data": packet["data"], "bits": 8, "type": packet["mime"]}}
        if not packet.get("formats") and packet["mime"].startswith("text/plain"):
            for name in ("UTF8_STRING", "text/plain;charset=utf-8", "text/plain"):
                formats[name] = {"data": packet["data"], "bits": 8, "type": name}
        payloads = {name.encode(): base64.b64decode(f["data"]) for name, f in formats.items()}
        source = self.marshal(self.manager, 0, self.prefix + "_source_v1", None)
        def send(name, fd):
            def write():
                try:
                    if packet.get("lazy"):
                        data = memoryview(resolve_payload(packet, name.decode(), formats[name.decode()]))
                    else:
                        data = memoryview(payloads[name])
                    while data:
                        data = data[os.write(fd, data):]
                except Exception as exc:
                    print("copier clipboard send failed:", exc, file=sys.stderr)
                finally:
                    os.close(fd)
            threading.Thread(target=write, daemon=True).start()
        def cancel():
            self.cancelled = True
        self.listen(source, [(send, [C.c_char_p, C.c_int]), (cancel, [])])
        for name in payloads:
            self.marshal(source, 0, None, C.c_char_p(name))
        self.marshal(self.device, 2 if self.selection == "primary" else 0, None, C.c_void_p(source))
        self.roundtrip()
        if self.cancelled:
            raise RuntimeError("clipboard ownership rejected")
        os.write(ready_fd, b"1")
        os.close(ready_fd)
        while not self.cancelled:
            if self.lib.wl_display_dispatch(self.display) < 0:
                break
            if self.error:
                raise self.error

    def close(self):
        if self.display:
            self.lib.wl_display_disconnect(self.display)
            self.display = None
