"""macOS helpers for the setup wizard: permissions, iPhone detection, local
backups, and getting a copy of the Messages database into ./data/.

Two sources are supported:
  * "backup": a local (Finder) backup of an iPhone connected by cable. Local
    backups always contain the full Messages database, even when Messages in
    iCloud is on. Encrypted backups need the backup password.
  * "mac": the Messages app on this Mac (~/Library/Messages/chat.db), which
    has whatever Messages in iCloud has synced.
"""

from __future__ import annotations

import getpass
import json
import os
import plistlib
import shutil
import sqlite3
import subprocess
import sys
import threading
import time
from datetime import datetime, timezone
from pathlib import Path

HOME = Path.home()
MESSAGES_DB = HOME / "Library/Messages/chat.db"
BACKUP_ROOT = HOME / "Library/Application Support/MobileSync/Backup"

# Files inside an unencrypted backup are named sha1("<domain>-<relative path>").
SMS_DB_HASH = "3d0d7e5fb2ce288813306e4d4636395e047a3d28"          # HomeDomain-Library/SMS/sms.db
ADDRESSBOOK_HASH = "31bb7ba8914766d4ba40d6dfb6113c8b614be442"     # HomeDomain-Library/AddressBook/AddressBook.sqlitedb

APPLE_MOBILE_BACKUP = Path(
    "/System/Library/PrivateFrameworks/MobileDevice.framework/Versions/Current/"
    "AppleMobileDeviceHelper.app/Contents/Resources/AppleMobileBackup"
)

IS_MAC = sys.platform == "darwin"


# --------------------------------------------------------------------------- #
# Permissions and opening things
# --------------------------------------------------------------------------- #
def full_disk_access() -> bool | None:
    """True/False if we can tell whether this process has Full Disk Access, None if unknown."""
    probes = [
        HOME / "Library/Application Support/com.apple.TCC",
        HOME / "Library/Messages",
        BACKUP_ROOT,
        HOME / "Library/Safari",
    ]
    for p in probes:
        if p.exists():
            try:
                os.listdir(p)
                return True
            except PermissionError:
                return False
            except OSError:
                continue
    return None


def host_app() -> str:
    """The app the user needs to grant Full Disk Access to (the one running us)."""
    prog = os.environ.get("TERM_PROGRAM", "")
    return {
        "Apple_Terminal": "Terminal",
        "iTerm.app": "iTerm",
        "vscode": "Visual Studio Code",
        "WarpTerminal": "Warp",
        "ghostty": "Ghostty",
    }.get(prog, "Terminal")


OPEN_TARGETS = {
    "fda": ["open", "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles"],
    "messages": ["open", "-a", "Messages"],
    "finder": ["open", "-a", "Finder"],
}


def open_target(name: str) -> bool:
    cmd = OPEN_TARGETS.get(name)
    if not cmd or not IS_MAC:
        return False
    subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    return True


def relaunch_terminal(launcher: Path) -> bool:
    """Quit Terminal and reopen the launcher so a new Full Disk Access grant takes effect.

    Runs in a detached helper so it survives Terminal quitting. The caller should
    exit the server right after, so Terminal has no running process to ask about.
    """
    if not IS_MAC or host_app() != "Terminal" or not launcher.exists():
        return False
    script = (
        "sleep 2; osascript -e 'tell application \"Terminal\" to quit'; "
        f"sleep 2; open {json.dumps(str(launcher))}"
    )
    subprocess.Popen(["/bin/sh", "-c", script], start_new_session=True,
                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, stdin=subprocess.DEVNULL)
    return True


def full_name() -> str:
    if IS_MAC:
        try:
            name = subprocess.run(["id", "-F"], capture_output=True, text=True, timeout=5).stdout.strip()
            if name:
                return name.split()[0]
        except Exception:
            pass
    return getpass.getuser().capitalize()


# --------------------------------------------------------------------------- #
# Connected devices
# --------------------------------------------------------------------------- #
def _walk(node, found):
    if isinstance(node, dict):
        name = str(node.get("_name", ""))
        if any(k in name for k in ("iPhone", "iPad", "iPod")):
            serial = node.get("serial_num") or node.get("USB Serial Number") or ""
            found.append({"name": name, "serial": serial})
        for v in node.values():
            _walk(v, found)
    elif isinstance(node, list):
        for v in node:
            _walk(v, found)


def connected_devices() -> list[dict]:
    """iPhones/iPads plugged in by USB (seen even before 'Trust' is tapped)."""
    if not IS_MAC:
        return []
    found: list[dict] = []
    for data_type in ("SPUSBHostDataType", "SPUSBDataType"):
        try:
            out = subprocess.run(["system_profiler", data_type, "-json"],
                                 capture_output=True, text=True, timeout=20).stdout
            _walk(json.loads(out or "{}"), found)
        except Exception:
            continue
        if found:
            break
    seen, unique = set(), []
    for d in found:
        key = (d["name"], d["serial"])
        if key not in seen:
            seen.add(key)
            unique.append(d)
    return unique


def udid_from_serial(serial: str) -> str:
    s = serial.replace("-", "")
    return f"{s[:8]}-{s[8:]}" if len(s) == 24 else s


# --------------------------------------------------------------------------- #
# Local backups
# --------------------------------------------------------------------------- #
def _plist(path: Path) -> dict:
    try:
        with path.open("rb") as f:
            return plistlib.load(f)
    except Exception:
        return {}


def _iso(value) -> str:
    if isinstance(value, datetime):
        if value.tzinfo is None:
            value = value.replace(tzinfo=timezone.utc)
        return value.astimezone().isoformat()
    return ""


def list_backups() -> list[dict]:
    backups = []
    if not BACKUP_ROOT.exists():
        return backups
    try:
        entries = list(BACKUP_ROOT.iterdir())
    except PermissionError:
        return backups
    for d in entries:
        if not d.is_dir() or not (d / "Manifest.db").exists():
            continue
        info = _plist(d / "Info.plist")
        manifest = _plist(d / "Manifest.plist")
        status = _plist(d / "Status.plist")
        last = info.get("Last Backup Date") or status.get("Date")
        if isinstance(last, datetime) and last.tzinfo is None:
            last = last.replace(tzinfo=timezone.utc)  # plist dates are UTC
        backups.append({
            "id": d.name,
            "device": info.get("Device Name") or info.get("Display Name") or "iPhone",
            "product": info.get("Product Type", ""),
            "udid": info.get("Unique Identifier", d.name),
            "date": _iso(last),
            "ts": last.timestamp() if isinstance(last, datetime) else d.stat().st_mtime,
            "encrypted": bool(manifest.get("IsEncrypted")),
            "complete": status.get("SnapshotState", "finished") == "finished",
            "size_gb": None,
        })
    backups.sort(key=lambda b: -b["ts"])
    return backups


class BackupJob:
    """Runs Apple's own command-line backup tool in the background."""

    def __init__(self):
        self.proc: subprocess.Popen | None = None
        self.started = 0.0
        self.log: list[str] = []
        self.lock = threading.Lock()

    @staticmethod
    def available() -> bool:
        return IS_MAC and APPLE_MOBILE_BACKUP.exists()

    def start(self, serial: str = "") -> dict:
        if not self.available():
            return {"error": "Automatic backup isn't available on this Mac. Use Finder instead."}
        if self.running():
            return {"ok": True}
        cmd = [str(APPLE_MOBILE_BACKUP), "--backup"]
        if serial:
            cmd += ["--device", udid_from_serial(serial)]
        self.log = []
        self.started = time.time()
        self.proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                     stdin=subprocess.DEVNULL, text=True, bufsize=1)
        threading.Thread(target=self._read, daemon=True).start()
        return {"ok": True}

    def _read(self):
        assert self.proc and self.proc.stdout
        for line in self.proc.stdout:
            with self.lock:
                self.log.append(line.rstrip())
                self.log = self.log[-30:]

    def running(self) -> bool:
        return bool(self.proc and self.proc.poll() is None)

    def status(self) -> dict:
        if not self.proc:
            return {"state": "idle", "available": self.available()}
        code = self.proc.poll()
        with self.lock:
            tail = self.log[-8:]
        return {
            "state": "running" if code is None else ("done" if code == 0 else "failed"),
            "code": code,
            "elapsed": int(time.time() - self.started),
            "log": tail,
            "available": self.available(),
        }


# --------------------------------------------------------------------------- #
# Getting a database copy into ./data
# --------------------------------------------------------------------------- #
def mac_messages_stats(path: Path = MESSAGES_DB) -> dict:
    if not path.exists():
        return {"exists": False}
    try:
        conn = sqlite3.connect(f"file:{path}?mode=ro", uri=True, timeout=5)
        count, lo, hi = conn.execute("SELECT COUNT(*), MIN(date), MAX(date) FROM message WHERE date > 0").fetchone()
        conn.close()
    except sqlite3.Error as exc:
        return {"exists": True, "readable": False, "error": str(exc)}
    from .build_index import apple_date_to_datetime
    lo_dt, hi_dt = apple_date_to_datetime(lo), apple_date_to_datetime(hi)
    return {
        "exists": True, "readable": True, "count": count,
        "oldest": lo_dt.isoformat() if lo_dt else "", "newest": hi_dt.isoformat() if hi_dt else "",
    }


def snapshot_sqlite(src: Path, dest: Path):
    """Consistent copy of a live SQLite DB (folds in the -wal file)."""
    dest.parent.mkdir(parents=True, exist_ok=True)
    tmp = dest.with_suffix(".tmp")
    if tmp.exists():
        tmp.unlink()
    s = sqlite3.connect(f"file:{src}?mode=ro", uri=True)
    d = sqlite3.connect(tmp)
    s.backup(d)
    d.close()
    s.close()
    os.replace(tmp, dest)


def copy_from_mac(data_dir: Path) -> dict:
    snapshot_sqlite(MESSAGES_DB, data_dir / "chat.db")
    contacts = data_dir / "contacts.sqlitedb"
    if contacts.exists():
        contacts.unlink()  # Mac Contacts are read directly by the indexer
    return {"db": data_dir / "chat.db", "contacts": None}


class WrongPassword(Exception):
    pass


def extract_from_backup(backup_id: str, data_dir: Path, password: str = "") -> dict:
    bdir = BACKUP_ROOT / backup_id
    manifest = _plist(bdir / "Manifest.plist")
    data_dir.mkdir(parents=True, exist_ok=True)
    db_out, ab_out = data_dir / "chat.db", data_dir / "contacts.sqlitedb"
    for p in (db_out, ab_out):
        if p.exists():
            p.unlink()

    if manifest.get("IsEncrypted"):
        if not password:
            raise WrongPassword("This backup is encrypted. Enter the backup password.")
        try:
            from iphone_backup_decrypt import EncryptedBackup, RelativePath
        except ImportError as exc:
            raise RuntimeError(
                "Reading encrypted backups needs the 'iphone_backup_decrypt' package. "
                "Close this window and double-click Start again to install it."
            ) from exc
        try:
            backup = EncryptedBackup(backup_directory=str(bdir), passphrase=password)
            backup.extract_file(relative_path=RelativePath.TEXT_MESSAGES, output_filename=str(db_out))
        except Exception as exc:
            msg = str(exc).lower()
            if "password" in msg or "passphrase" in msg or "decrypt" in msg or "unwrap" in msg:
                raise WrongPassword("That password didn't unlock the backup. Try again.") from exc
            raise WrongPassword(
                f"Couldn't unlock this backup. Check the password, or make a new backup. (Details: {exc!r})"
            ) from exc
        try:
            backup.extract_file(relative_path=RelativePath.ADDRESS_BOOK, output_filename=str(ab_out))
        except Exception:
            pass
    else:
        sms = bdir / SMS_DB_HASH[:2] / SMS_DB_HASH
        if not sms.exists():
            raise RuntimeError("Couldn't find Messages in this backup. Try making a new backup.")
        shutil.copyfile(sms, db_out)
        ab = bdir / ADDRESSBOOK_HASH[:2] / ADDRESSBOOK_HASH
        if ab.exists():
            shutil.copyfile(ab, ab_out)
    return {"db": db_out, "contacts": ab_out if ab_out.exists() else None}


def find_launcher(root: Path) -> Path:
    return root / "Start iMessage Search.command"
