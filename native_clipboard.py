"""GTK3 clipboard adapter. Embedded in copier; uses only Python's stdlib.

GTK owns all target representations together on X11 and Wayland. A detached
owner stays alive until another application claims the clipboard. No payload
is put in argv, the environment, or a temporary plaintext file.
"""
import base64
import ctypes as C
import ctypes.util
import json
import os
import select
import struct
import sys
import subprocess

OFFER_MIME = "application/vnd.copier.offer+json"
LOCAL_ONLY_MIME = "application/vnd.copier.local-only"


def resolve_payload(packet, name, fallback):
    lazy = packet.get("lazy")
    if not lazy or name in (OFFER_MIME, LOCAL_ONLY_MIME):
        return base64.b64decode(fallback["data"], validate=True)
    index = next(i for i, entry in enumerate(lazy["offer"]["entries"]) if entry["name"] == name)
    command = [lazy["executable"], "fetch-format", "--index", str(index)]
    if lazy.get("allow_http"):
        command.append("--allow-http")
    result = subprocess.run(command, input=json.dumps(lazy["offer"]).encode(),
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=300)
    if result.returncode:
        raise RuntimeError(result.stderr.decode(errors="replace"))
    return result.stdout

MAX = 32 * 1024 * 1024
SKIP = {"TARGETS", "TIMESTAMP", "MULTIPLE", "SAVE_TARGETS", "INCR",
        "DELETE", "INSERT_SELECTION", "INSERT_PROPERTY"}


def library(name):
    path = ctypes.util.find_library(name)
    if not path:
        raise RuntimeError("install GTK3 runtime libraries (libgtk-3-0 / gtk3)")
    return C.CDLL(path)


def init():
    gtk, gdk, glib = library("gtk-3"), library("gdk-3"), library("glib-2.0")
    def fn(lib, name, result, *args):
        f = getattr(lib, name)
        f.restype, f.argtypes = result, args
        return f
    ptr = C.c_void_p
    fn(gtk, "gtk_init_check", C.c_int, ptr, ptr)
    fn(gdk, "gdk_atom_intern", ptr, C.c_char_p, C.c_int)
    fn(gdk, "gdk_atom_name", ptr, ptr)
    fn(glib, "g_free", None, ptr)
    fn(gtk, "gtk_clipboard_get", ptr, ptr)
    fn(gtk, "gtk_clipboard_wait_for_targets", C.c_int, ptr, C.POINTER(ptr), C.POINTER(C.c_int))
    fn(gtk, "gtk_clipboard_wait_for_contents", ptr, ptr, ptr)
    fn(gtk, "gtk_selection_data_get_length", C.c_int, ptr)
    fn(gtk, "gtk_selection_data_get_format", C.c_int, ptr)
    fn(gtk, "gtk_selection_data_get_data_type", ptr, ptr)
    fn(gtk, "gtk_selection_data_get_data", ptr, ptr)
    fn(gtk, "gtk_selection_data_get_target", ptr, ptr)
    fn(gtk, "gtk_selection_data_free", None, ptr)
    fn(gtk, "gtk_selection_data_set", None, ptr, ptr, C.c_int, ptr, C.c_int)
    fn(gtk, "gtk_main", None)
    fn(gtk, "gtk_main_quit", None)
    fn(gtk, "gtk_events_pending", C.c_int)
    fn(gtk, "gtk_main_iteration", C.c_int)
    fn(gtk, "gtk_clipboard_set_with_data", C.c_int, ptr, ptr, C.c_uint, ptr, ptr, ptr)
    if not gtk.gtk_init_check(None, None):
        raise RuntimeError("GTK cannot connect to the desktop display")
    atom = gdk.gdk_atom_intern(sys.argv[2].upper().encode(), 0)
    cb = gtk.gtk_clipboard_get(atom)
    if not cb:
        raise RuntimeError("selection unavailable on this compositor")
    return gtk, gdk, glib, cb


def atom_name(gdk, glib, atom):
    p = gdk.gdk_atom_name(atom)
    if not p:
        return ""
    try:
        return C.string_at(p).decode("utf-8")
    finally:
        glib.g_free(p)


def capture():
    gtk, gdk, glib, cb = init()
    gobject = library("gobject-2.0")
    gobject.g_signal_connect_data.argtypes = [C.c_void_p, C.c_char_p, C.c_void_p,
                                             C.c_void_p, C.c_void_p, C.c_int]
    gobject.g_signal_connect_data.restype = C.c_ulong
    generation = [0]
    @C.CFUNCTYPE(None, C.c_void_p, C.c_void_p, C.c_void_p)
    def changed(_cb, _event, _user):
        generation[0] += 1
    gobject.g_signal_connect_data(cb, b"owner-change", changed, None, None, 0)
    targets, count = C.c_void_p(), C.c_int()
    if not gtk.gtk_clipboard_wait_for_targets(cb, C.byref(targets), C.byref(count)):
        return {"mime": "text/plain", "data": ""}
    if count.value > 256:
        glib.g_free(targets)
        raise RuntimeError("clipboard advertises too many targets")
    atoms = C.cast(targets, C.POINTER(C.c_void_p))
    names = [atom_name(gdk, glib, atoms[i]) for i in range(count.value)]
    glib.g_free(targets)
    if LOCAL_ONLY_MIME in names and sys.argv[1] != "read-publish":
        data = gtk.gtk_clipboard_wait_for_contents(cb, gdk.gdk_atom_intern(LOCAL_ONLY_MIME.encode(), 0))
        if not data:
            raise RuntimeError("local-only clipboard marker unavailable")
        try:
            n = gtk.gtk_selection_data_get_length(data)
            if n < 0 or n > 128:
                raise RuntimeError("invalid local-only marker")
            raw = C.string_at(gtk.gtk_selection_data_get_data(data), n)
            return {"mime": LOCAL_ONLY_MIME, "data": base64.b64encode(raw).decode()}
        finally:
            gtk.gtk_selection_data_free(data)
    if OFFER_MIME in names:
        # Background clipboard observation reads the announcement, not the
        # advertised data targets. Reading a URI target must not fetch files.
        data = gtk.gtk_clipboard_wait_for_contents(cb, gdk.gdk_atom_intern(OFFER_MIME.encode(), 0))
        if not data:
            raise RuntimeError("clipboard offer unavailable")
        try:
            n = gtk.gtk_selection_data_get_length(data)
            if n < 0 or n > 1024 * 1024:
                raise RuntimeError("invalid clipboard offer size")
            raw = C.string_at(gtk.gtk_selection_data_get_data(data), n)
            return {"mime": OFFER_MIME, "data": base64.b64encode(raw).decode()}
        finally:
            gtk.gtk_selection_data_free(data)
    before = generation[0]
    formats, size = {}, 0
    for name in sorted(set(names) - SKIP - {LOCAL_ONLY_MIME}):
        atom = gdk.gdk_atom_intern(name.encode(), 0)
        data = gtk.gtk_clipboard_wait_for_contents(cb, atom)
        if not data:
            raise RuntimeError("clipboard changed or target could not be read: " + name)
        try:
            n = gtk.gtk_selection_data_get_length(data)
            bits = gtk.gtk_selection_data_get_format(data)
            if n < 0 or bits not in (8, 16, 32):
                raise RuntimeError("unreadable clipboard target: " + name)
            size += n
            if size > MAX or len(formats) >= 128:
                raise RuntimeError("clipboard bundle exceeds 32 MiB / 128 formats")
            raw = C.string_at(gtk.gtk_selection_data_get_data(data), n) if n else b""
            # GTK stores 32-bit X11 units as native longs (8 bytes on amd64).
            # Use fixed little-endian units on the wire across architectures.
            if bits == 32:
                units = struct.unpack("@" + "L" * (n // C.sizeof(C.c_ulong)), raw)
                raw = struct.pack("<" + "I" * len(units), *(x & 0xffffffff for x in units))
            elif bits == 16:
                units = struct.unpack("@" + "H" * (n // 2), raw)
                raw = struct.pack("<" + "H" * len(units), *units)
            formats[name] = {"data": base64.b64encode(raw).decode(), "bits": bits,
                             "type": atom_name(gdk, glib, gtk.gtk_selection_data_get_data_type(data))}
        finally:
            gtk.gtk_selection_data_free(data)
    if not formats:
        raise RuntimeError("clipboard has no transferable representations")
    while gtk.gtk_events_pending():
        gtk.gtk_main_iteration()
    if generation[0] != before:
        raise RuntimeError("clipboard owner changed during capture; retry")
    preferred = next((x for x in ("text/plain;charset=utf-8", "UTF8_STRING", "text/plain",
                                  "image/png", "text/html") if x in formats), sorted(formats)[0])
    packet = {"mime": preferred, "data": formats[preferred]["data"], "formats": formats}
    # The primary payload is duplicated for convenient stdin/stdout use, and is
    # included in the same total limit the relay enforces.
    if size + len(base64.b64decode(packet["data"])) > MAX:
        raise RuntimeError("clipboard bundle including default representation exceeds 32 MiB")
    return packet


class Entry(C.Structure):
    _fields_ = [("target", C.c_char_p), ("flags", C.c_uint), ("info", C.c_uint)]


def own(packet, ready_fd=None):
    gtk, gdk, glib, cb = init()
    formats = packet.get("formats") or {packet["mime"]: {"data": packet["data"],
                                                        "type": packet["mime"], "bits": 8}}
    # Text from CLI must advertise the customary X11 and Wayland text targets.
    if not packet.get("formats") and packet["mime"].startswith("text/plain"):
        for name in ("UTF8_STRING", "text/plain;charset=utf-8", "text/plain"):
            formats[name] = {"data": packet["data"], "type": name, "bits": 8}
    entries = (Entry * len(formats))()
    values = []
    for i, (name, f) in enumerate(sorted(formats.items())):
        entries[i] = Entry(name.encode(), 0, i)
        raw = base64.b64decode(f["data"], validate=True)
        if f["bits"] == 32:
            units = struct.unpack("<" + "I" * (len(raw) // 4), raw)
            raw = struct.pack("@" + "L" * len(units), *units)
        elif f["bits"] == 16:
            units = struct.unpack("<" + "H" * (len(raw) // 2), raw)
            raw = struct.pack("@" + "H" * len(units), *units)
        values.append((C.create_string_buffer(raw), len(raw), f["type"], f["bits"], name, f))
    @C.CFUNCTYPE(None, C.c_void_p, C.c_void_p, C.c_uint, C.c_void_p)
    def get_data(_cb, data, info, _user):
        raw, n, kind, bits, name, format_info = values[info]
        try:
            if packet.get("lazy") and name != OFFER_MIME:
                payload = resolve_payload(packet, name, format_info)
                if bits == 32:
                    units = struct.unpack("<" + "I" * (len(payload) // 4), payload)
                    payload = struct.pack("@" + "L" * len(units), *units)
                elif bits == 16:
                    units = struct.unpack("<" + "H" * (len(payload) // 2), payload)
                    payload = struct.pack("@" + "H" * len(units), *units)
                raw, n = C.create_string_buffer(payload), len(payload)
            gtk.gtk_selection_data_set(data, gdk.gdk_atom_intern(kind.encode(), 0), bits, raw, n)
        except Exception as exc:
            print("copier paste failed:", exc, file=sys.stderr)
            gtk.gtk_selection_data_set(data, gdk.gdk_atom_intern(kind.encode(), 0), bits, None, -1)
    @C.CFUNCTYPE(None, C.c_void_p, C.c_void_p)
    def cleared(_cb, _user):
        gtk.gtk_main_quit()
    if not gtk.gtk_clipboard_set_with_data(cb, entries, len(entries), get_data, cleared, None):
        raise RuntimeError("compositor rejected clipboard ownership")
    if ready_fd is not None:
        os.write(ready_fd, b"1")
        os.close(ready_fd)
    gtk.gtk_main()


def main():
    if sys.argv[1] in ("read", "read-publish"):
        print(json.dumps(capture_backend()))
        return
    packet = json.load(sys.stdin)
    read_fd, write_fd = os.pipe()
    pid = os.fork()
    if pid:
        os.close(write_fd)
        try:
            if not select.select([read_fd], [], [], 8)[0] or os.read(read_fd, 1) != b"1":
                try:
                    os.kill(pid, 15)
                except ProcessLookupError:
                    pass
                os.waitpid(pid, 0)
                raise RuntimeError("native clipboard owner failed to start")
        finally:
            os.close(read_fd)
        return
    os.close(read_fd)
    os.setsid()
    null = os.open(os.devnull, os.O_RDWR)
    os.dup2(null, 0)
    os.dup2(null, 1)
    os.close(null)
    try:
        own_backend(packet, write_fd)
    except Exception as exc:
        print("copier clipboard owner:", exc, file=sys.stderr)
        os._exit(1)
    os._exit(0)


def capture_backend():
    if os.environ.get("GDK_BACKEND") == "wayland":
        try:
            clipboard = WaylandClipboard(sys.argv[2])
        except ProtocolUnavailable:
            if not os.environ.get("DISPLAY"):
                raise
            os.environ["GDK_BACKEND"] = "x11"
        else:
            try:
                return clipboard.capture()
            finally:
                clipboard.close()
    return capture()


def own_backend(packet, ready_fd):
    if os.environ.get("GDK_BACKEND") == "wayland":
        try:
            clipboard = WaylandClipboard(sys.argv[2])
        except ProtocolUnavailable:
            if not os.environ.get("DISPLAY"):
                raise
            os.environ["GDK_BACKEND"] = "x11"
        else:
            try:
                clipboard.own(packet, ready_fd)
                return
            finally:
                clipboard.close()
    own(packet, ready_fd)


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print("copier native clipboard:", exc, file=sys.stderr)
        sys.exit(1)
