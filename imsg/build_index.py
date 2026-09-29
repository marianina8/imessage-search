"""Build a searchable index from a copy of the macOS Messages database (chat.db).

Reads chat.db strictly read-only, decodes messages whose text lives only in
`attributedBody` (common on recent macOS/iOS), resolves phone numbers and
emails to contact names when the Contacts database is available, and writes
everything to a local SQLite file with a full-text (FTS5) index.

Usage:
    python -m imsg.build_index                       # uses data/chat.db
    python -m imsg.build_index --db ~/Library/Messages/chat.db
    python -m imsg.build_index --list-chats          # show chats and IDs
    python -m imsg.build_index --chat 5 --chat 12    # only some chats
"""

from __future__ import annotations

import argparse
import glob
import hashlib
import os
import re
import sqlite3
import sys
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DEFAULT_DB = ROOT / "data" / "chat.db"
DEFAULT_OUT = ROOT / "data" / "index.sqlite"
ADDRESSBOOK_GLOB = str(
    Path.home()
    / "Library/Application Support/AddressBook/Sources/*/AddressBook-v22.abcddb"
)

APPLE_EPOCH = datetime(2001, 1, 1, tzinfo=timezone.utc)


# --------------------------------------------------------------------------- #
# Dates
# --------------------------------------------------------------------------- #
def apple_date_to_datetime(value) -> datetime | None:
    """Messages stores dates as seconds (older) or nanoseconds (newer) since 2001."""
    if not value:
        return None
    seconds = value / 1_000_000_000 if value > 1e11 else value
    return (APPLE_EPOCH + timedelta(seconds=seconds)).astimezone()


# --------------------------------------------------------------------------- #
# attributedBody decoding
# --------------------------------------------------------------------------- #
try:
    from typedstream import unarchive_from_data
    from typedstream.archiving import TypedValue
    from typedstream.types.foundation import NSMutableString, NSString

    HAVE_TYPEDSTREAM = True
except ImportError:  # optional dependency
    HAVE_TYPEDSTREAM = False


def _decode_with_typedstream(blob: bytes) -> str:
    attributed = unarchive_from_data(blob)
    for item in attributed.contents:
        value = item.value if isinstance(item, TypedValue) else item
        if isinstance(value, (NSString, NSMutableString)):
            return value.value
    return ""


def _decode_fallback(blob: bytes) -> str:
    """Minimal parser for the NSString inside a typedstream attributedBody."""
    idx = blob.find(b"NSString")
    if idx == -1:
        return ""
    idx = blob.find(b"+", idx)  # '+' precedes the length-prefixed string
    if idx == -1:
        return ""
    idx += 1
    length = blob[idx]
    idx += 1
    if length == 0x81:  # 2-byte little-endian length
        length = int.from_bytes(blob[idx : idx + 2], "little")
        idx += 2
    elif length == 0x82:  # 4-byte little-endian length
        length = int.from_bytes(blob[idx : idx + 4], "little")
        idx += 4
    return blob[idx : idx + length].decode("utf-8", errors="replace")


def decode_attributed_body(blob: bytes | None) -> str:
    if not blob:
        return ""
    if HAVE_TYPEDSTREAM:
        try:
            text = _decode_with_typedstream(blob)
            if text:
                return text
        except Exception:
            pass
    try:
        return _decode_fallback(blob)
    except Exception:
        return ""


def clean(text: str) -> str:
    # Strip NULs and the U+FFFC object-replacement char used for attachments.
    return (text or "").replace("\x00", "").replace("￼", "").strip()


# --------------------------------------------------------------------------- #
# Contacts
# --------------------------------------------------------------------------- #
def normalize_handle(handle: str) -> str:
    if not handle:
        return ""
    if "@" in handle:
        return handle.strip().lower()
    digits = re.sub(r"\D", "", handle)
    return digits[-10:] if len(digits) >= 10 else digits


def load_contacts(pattern: str = ADDRESSBOOK_GLOB) -> dict[str, str]:
    """Map normalized phone/email -> display name from the macOS Contacts DBs."""
    names: dict[str, str] = {}
    for path in glob.glob(pattern):
        try:
            conn = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
            people = {}
            for pk, first, last, org in conn.execute(
                "SELECT Z_PK, ZFIRSTNAME, ZLASTNAME, ZORGANIZATION FROM ZABCDRECORD"
            ):
                name = " ".join(p for p in (first, last) if p) or org
                if name:
                    people[pk] = name
            for owner, number in conn.execute(
                "SELECT ZOWNER, ZFULLNUMBER FROM ZABCDPHONENUMBER"
            ):
                if owner in people and number:
                    names[normalize_handle(number)] = people[owner]
            for owner, email in conn.execute(
                "SELECT ZOWNER, ZADDRESS FROM ZABCDEMAILADDRESS"
            ):
                if owner in people and email:
                    names[normalize_handle(email)] = people[owner]
            conn.close()
        except sqlite3.Error as exc:
            print(f"  (skipping contacts DB {path}: {exc})", file=sys.stderr)
    return names


def load_ios_contacts(path: Path) -> dict[str, str]:
    """Map normalized phone/email -> name from an iPhone AddressBook.sqlitedb."""
    names: dict[str, str] = {}
    try:
        conn = sqlite3.connect(f"file:{path}?mode=ro", uri=True)
        rows = conn.execute("""
            SELECT p.First, p.Last, p.Organization, v.property, v.value
            FROM ABMultiValue v JOIN ABPerson p ON p.ROWID = v.record_id
            WHERE v.property IN (3, 4) AND v.value IS NOT NULL
        """)
        for first, last, org, _prop, value in rows:
            name = " ".join(p for p in (first, last) if p) or org
            if name:
                names[normalize_handle(value)] = name
        conn.close()
    except sqlite3.Error as exc:
        print(f"  (couldn't read iPhone contacts: {exc})", file=sys.stderr)
    return names


# --------------------------------------------------------------------------- #
# Chats
# --------------------------------------------------------------------------- #
CHAT_SQL = """
SELECT
    c.ROWID AS chat_id,
    c.chat_identifier,
    c.display_name,
    COUNT(cmj.message_id) AS message_count,
    GROUP_CONCAT(DISTINCT h.id) AS participants
FROM chat c
LEFT JOIN chat_message_join cmj ON cmj.chat_id = c.ROWID
LEFT JOIN chat_handle_join chj ON chj.chat_id = c.ROWID
LEFT JOIN handle h ON h.ROWID = chj.handle_id
GROUP BY c.ROWID
ORDER BY message_count DESC
"""


def chat_label(row, contacts) -> str:
    if row["display_name"]:
        return row["display_name"]
    parts = [p for p in (row["participants"] or row["chat_identifier"] or "").split(",") if p]
    return ", ".join(contacts.get(normalize_handle(p), p) for p in parts) or "(unknown)"


# --------------------------------------------------------------------------- #
# Build
# --------------------------------------------------------------------------- #
MESSAGE_SQL = """
SELECT
    m.ROWID AS message_id,
    m.guid,
    m.date,
    m.is_from_me,
    m.service,
    m.text,
    m.attributedBody,
    m.cache_has_attachments,
    m.associated_message_type,
    h.id AS handle,
    cmj.chat_id
FROM message m
JOIN chat_message_join cmj ON cmj.message_id = m.ROWID
LEFT JOIN handle h ON h.ROWID = m.handle_id
{where}
ORDER BY cmj.chat_id, m.date, m.ROWID
"""

SCHEMA = """
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS chats;
DROP TABLE IF EXISTS messages_fts;
DROP TABLE IF EXISTS meta;
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE chats (
    chat_id INTEGER PRIMARY KEY,
    label TEXT,
    identifier TEXT,
    message_count INTEGER
);
CREATE TABLE messages (
    id INTEGER PRIMARY KEY,          -- original message ROWID
    guid TEXT,
    chat_id INTEGER,
    ts REAL,                          -- unix seconds, for range filters
    date TEXT,                        -- local ISO 8601
    is_from_me INTEGER,
    handle TEXT,
    sender TEXT,                      -- contact name, handle, or "Me"
    service TEXT,
    text TEXT,
    text_source TEXT,                 -- text | attributedBody | unresolved | attachment
    has_attachment INTEGER,
    is_reaction INTEGER,
    attachments TEXT                  -- attachment file names, "; "-separated
);
CREATE INDEX idx_messages_chat_ts ON messages(chat_id, ts);
CREATE INDEX idx_messages_sender ON messages(sender);
CREATE VIRTUAL TABLE messages_fts USING fts5(
    text, content='messages', content_rowid='id', tokenize='porter unicode61'
);
"""


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def local_timezone_name() -> str:
    tz = os.environ.get("TZ", "").lstrip(":")
    if "/" in tz and not tz.startswith("/"):
        return tz
    try:
        link = os.readlink("/etc/localtime")
        if "zoneinfo/" in link:
            return link.split("zoneinfo/", 1)[1]
    except OSError:
        pass
    return time.strftime("%Z")


def load_attachments(src) -> dict[int, str]:
    try:
        rows = src.execute("""
            SELECT maj.message_id, COALESCE(a.transfer_name, a.filename, 'attachment')
            FROM message_attachment_join maj JOIN attachment a ON a.ROWID = maj.attachment_id
        """).fetchall()
    except sqlite3.Error:
        return {}
    out: dict[int, list[str]] = {}
    for mid, name in rows:
        out.setdefault(mid, []).append(Path(str(name)).name)
    return {k: "; ".join(v) for k, v in out.items()}


def build(db_path: Path, out_path: Path, chat_ids: list[int], me_name: str, use_contacts: bool,
          ios_contacts: Path | None = None, progress=None, source_desc: str = "") -> dict:
    """Build the index. `progress(done, total)` is called periodically if given."""
    source_hash = sha256_file(db_path)
    src = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    src.row_factory = sqlite3.Row

    contacts = load_contacts() if use_contacts else {}  # this Mac's Contacts app
    if ios_contacts and Path(ios_contacts).exists():   # iPhone Contacts from a backup
        contacts.update(load_ios_contacts(Path(ios_contacts)))
    if contacts:
        print(f"Contacts resolved: {len(contacts)} phone numbers/emails")
    if not HAVE_TYPEDSTREAM:
        print("Note: pytypedstream not installed; using built-in fallback decoder.")

    out_path.parent.mkdir(parents=True, exist_ok=True)
    tmp_path = out_path.with_name(out_path.name + ".building")
    if tmp_path.exists():
        tmp_path.unlink()
    dst = sqlite3.connect(tmp_path)
    dst.executescript(SCHEMA)

    chats = list(src.execute(CHAT_SQL))
    for c in chats:
        if chat_ids and c["chat_id"] not in chat_ids:
            continue
        dst.execute(
            "INSERT INTO chats VALUES (?,?,?,?)",
            (c["chat_id"], chat_label(c, contacts), c["chat_identifier"], c["message_count"]),
        )

    where, params = "", []
    if chat_ids:
        where = f"WHERE cmj.chat_id IN ({','.join('?' * len(chat_ids))})"
        params = chat_ids

    total_src = src.execute(
        f"SELECT COUNT(*) FROM message m JOIN chat_message_join cmj ON cmj.message_id = m.ROWID {where}", params
    ).fetchone()[0]
    done = 0
    counts = {"text": 0, "attributedBody": 0, "unresolved": 0, "attachment": 0}
    attachments = load_attachments(src)
    batch = []
    for r in src.execute(MESSAGE_SQL.format(where=where), params):
        done += 1
        if progress and done % 2000 == 0:
            progress(done, total_src)
        text = clean(r["text"])
        source = "text"
        if not text and r["attributedBody"]:
            text = clean(decode_attributed_body(r["attributedBody"]))
            source = "attributedBody" if text else "unresolved"
        if not text and r["cache_has_attachments"]:
            source = "attachment"
        counts[source] = counts.get(source, 0) + 1

        dt = apple_date_to_datetime(r["date"])
        handle = r["handle"] or ""
        sender = me_name if r["is_from_me"] else contacts.get(normalize_handle(handle), handle or "(unknown)")
        batch.append((
            r["message_id"], r["guid"], r["chat_id"],
            dt.timestamp() if dt else None, dt.isoformat() if dt else "",
            int(bool(r["is_from_me"])), handle, sender, r["service"] or "",
            text, source, int(bool(r["cache_has_attachments"])),
            int(2000 <= (r["associated_message_type"] or 0) < 4000),  # tapbacks
            attachments.get(r["message_id"], ""),
        ))
        if len(batch) >= 5000:
            dst.executemany("INSERT OR IGNORE INTO messages VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)", batch)
            batch.clear()
    if batch:
        dst.executemany("INSERT OR IGNORE INTO messages VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)", batch)

    if progress:
        progress(total_src, total_src)
    dst.execute("INSERT INTO messages_fts(messages_fts) VALUES ('rebuild')")
    meta = {
        "built_at": datetime.now().astimezone().isoformat(timespec="seconds"),
        "source_desc": source_desc or f"Messages database file {db_path.name}",
        "source_file": str(db_path),
        "source_sha256": source_hash,
        "me_name": me_name,
        "timezone": local_timezone_name(),
        "tool": "iMessage Search (github.com/marianina8/imessage-search)",
    }
    dst.executemany("INSERT INTO meta VALUES (?, ?)", meta.items())
    dst.commit()
    total = dst.execute("SELECT COUNT(*) FROM messages").fetchone()[0]
    dst.close()
    src.close()
    os.replace(tmp_path, out_path)  # swap in atomically so a running server never sees half an index

    print(f"Indexed {total} messages -> {out_path}")
    for k, v in counts.items():
        print(f"  {k:15} {v}")
    return {"messages": total, "contacts": len(contacts), **counts}


def list_chats(db_path: Path, use_contacts: bool):
    src = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    src.row_factory = sqlite3.Row
    contacts = load_contacts() if use_contacts else {}
    print(f"{'chat_id':>7}  {'messages':>8}  label")
    for c in src.execute(CHAT_SQL):
        print(f"{c['chat_id']:>7}  {c['message_count']:>8}  {chat_label(c, contacts)}")


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", type=Path, default=DEFAULT_DB, help="path to a COPY of chat.db")
    ap.add_argument("--out", type=Path, default=DEFAULT_OUT, help="index file to write")
    ap.add_argument("--chat", type=int, action="append", default=[], help="only index this chat_id (repeatable)")
    ap.add_argument("--me", default="Me", help="display name for your own messages")
    ap.add_argument("--no-contacts", action="store_true", help="don't look up names in Contacts")
    ap.add_argument("--list-chats", action="store_true", help="list chats and exit")
    args = ap.parse_args(argv)

    db = args.db.expanduser()
    if not db.exists():
        sys.exit(f"chat.db not found at {db}. See README: 'Copy the database'.")
    if args.list_chats:
        list_chats(db, not args.no_contacts)
    else:
        ios = db.parent / "contacts.sqlitedb"
        build(db, args.out.expanduser(), args.chat, args.me, not args.no_contacts,
              ios_contacts=ios if ios.exists() and not args.no_contacts else None)


if __name__ == "__main__":
    main()
