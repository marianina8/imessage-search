"""Try iMessage Search with fictional demo messages. Your real messages aren't touched.

    python -m imsg.demo          # builds the demo and opens it in your browser

The story: Sam and roommate Priya move out of their apartment, and landlord
Dana withholds part of the security deposit for a ceiling repair and cleaning.
Every name, number, and message is made up.
"""

from __future__ import annotations

import sqlite3
import sys
from datetime import datetime, timezone
from pathlib import Path

from . import build_index, server

ROOT = Path(__file__).resolve().parent.parent
DEMO_DIR = ROOT / "data" / "demo"
PORT = 8766

CONTACTS = {
    "dana": ("Dana", "Whitfield", "+15550102233"),
    "priya": ("Priya", "Shah", "+15550104455"),
    "jordan": ("Jordan", "Lee", "jordan.lee@example.com"),
}
CHATS = {  # chat_id: (display name, members)
    1: ("", ["dana"]),
    2: ("", ["priya"]),
    3: ("Moving crew", ["priya", "jordan"]),
}

# (local date & time, chat_id, sender key or "me", text, attachment file name)
MESSAGES = [
    ("2025-11-03 09:12", 1, "me", "Hi Dana, water is dripping from the bathroom ceiling again. Photos attached.", "IMG_2041.HEIC"),
    ("2025-11-03 09:15", 2, "me", "Leak is back in the bathroom, I texted Dana photos", None),
    ("2025-11-03 09:20", 2, "priya", "omg again. ok I'll be home Thursday for the plumber", None),
    ("2025-11-03 09:40", 1, "dana", "Ugh, sorry. I'll have Mike the plumber come by Thursday.", None),
    ("2025-11-03 09:41", 1, "me", "Thursday works. Priya will be home to let him in.", None),
    ("2025-11-06 17:05", 1, "dana", "Mike says it's the seal on the upstairs tub. Fixed for now.", None),
    ("2025-11-06 17:07", 1, "me", "Thanks! The ceiling paint is bubbling though. Will that get repaired?", None),
    ("2025-11-06 17:30", 1, "dana", "Yes, I'll get it patched and painted. Don't worry about it.", None),
    ("2025-12-14 10:22", 1, "me", "Hi Dana, the leak is back and the stain on the bathroom ceiling is bigger.", "IMG_2107.HEIC"),
    ("2025-12-14 12:48", 1, "dana", "I'm traveling until the 28th. Can it wait?", None),
    ("2025-12-14 12:50", 1, "me", "It drips every time someone upstairs showers. We've put a bucket under it.", None),
    ("2025-12-14 12:52", 1, "dana", "OK, I'll call Mike.", None),
    ("2025-12-14 12:55", 2, "priya", "bucket is in place lol", None),
    ("2025-12-19 08:15", 1, "dana", "Mike can't come until January. Keep using the bucket, sorry.", None),
    ("2025-12-19 08:30", 2, "me", "Dana says the plumber can't come till January", None),
    ("2025-12-19 08:33", 2, "priya", "that's ridiculous, it's been dripping for a week", None),
    ("2026-01-09 14:30", 1, "dana", "Mike replaced the seal and the drywall in the bathroom ceiling. Painting next week.", None),
    ("2026-01-09 14:31", 1, "me", "Great, thank you.", None),
    ("2026-02-02 19:10", 1, "me", "Hi Dana, a heads up that we won't renew. Our lease ends May 31 and we'll be out by then.", None),
    ("2026-02-02 19:15", 2, "me", "Told Dana we're not renewing", None),
    ("2026-02-02 19:16", 2, "priya", "🙌", None),
    ("2026-02-02 20:02", 1, "dana", "Thanks for letting me know. I'll start showing it in April.", None),
    ("2026-02-02 20:03", 1, "dana", "Please have it cleaned professionally when you leave. The deposit is $2,400.", None),
    ("2026-02-02 20:05", 1, "me", "Will do.", None),
    ("2026-03-15 11:00", 1, "me", "By the way, the bathroom ceiling was never painted. It still has the patch.", None),
    ("2026-03-15 13:24", 1, "dana", "I'll take care of it before the new tenants move in.", None),
    ("2026-04-10 09:05", 1, "dana", "Can I show the apartment Saturday at 11?", None),
    ("2026-04-10 09:20", 1, "me", "Saturday at 11 is fine.", None),
    ("2026-05-24 18:00", 3, "jordan", "I can bring the truck Thursday the 28th after 5", None),
    ("2026-05-24 18:05", 3, "priya", "you're a lifesaver", None),
    ("2026-05-24 18:06", 3, "me", "pizza is on us", None),
    ("2026-05-28 17:10", 3, "jordan", "here, parked out front", None),
    ("2026-05-28 21:00", 2, "priya", "cleaners booked for tomorrow 10am, $320. I'll venmo you half", None),
    ("2026-05-28 21:02", 2, "me", "perfect thanks", None),
    ("2026-05-28 22:30", 3, "me", "thank you both, every box is at the new place", None),
    ("2026-05-29 13:00", 2, "priya", "sent you $160 for the cleaners", None),
    ("2026-05-29 16:40", 1, "me", "We're all moved out! Professional cleaners came today. Receipt attached.", "Cleaning_receipt.pdf"),
    ("2026-05-29 16:41", 1, "me", "Keys are in the lockbox. Can we do the walkthrough tomorrow?", None),
    ("2026-05-29 18:02", 1, "dana", "I'll do it Monday. You don't need to be there.", None),
    ("2026-05-29 18:05", 1, "me", "I'd prefer to be there. I took a video of every room today.", "Moveout_walkthrough.MOV"),
    ("2026-05-29 18:30", 1, "dana", "Fine, Monday 10am.", None),
    ("2026-06-01 10:48", 1, "dana", "Walkthrough done. Looks mostly good.", None),
    ("2026-06-01 10:50", 1, "me", "Great. When should we expect the deposit?", None),
    ("2026-06-01 11:15", 1, "dana", "Within 21 days, per the law.", None),
    ("2026-06-18 15:32", 1, "dana", "I'm keeping $1,150 of the deposit: $900 for the bathroom ceiling repair and $250 for cleaning. The $1,250 refund was mailed today.", None),
    ("2026-06-18 15:40", 1, "me", "The ceiling damage came from the leak you were fixing. We reported it in November and again in December.", None),
    ("2026-06-18 15:41", 1, "me", "And we had it professionally cleaned. I sent you the receipt on May 29.", None),
    ("2026-06-18 15:45", 2, "me", "Dana is keeping $1,150 of the deposit, including $250 for cleaning!", None),
    ("2026-06-18 15:47", 2, "priya", "WHAT. we paid $320 for professional cleaners", None),
    ("2026-06-18 15:48", 2, "priya", "and the ceiling was her leak", None),
    ("2026-06-18 15:50", 2, "me", "I'm pulling all our texts together. You still have the photos from December?", None),
    ("2026-06-18 15:52", 2, "priya", "yes, sending", "IMG_2110.HEIC"),
    ("2026-06-18 16:55", 1, "dana", "The ceiling got worse because you didn't report the leak fast enough.", None),
    ("2026-06-18 16:58", 1, "me", "I texted you the same day both times. You said Mike couldn't come until January.", None),
    ("2026-06-18 17:30", 1, "dana", "I'll need to look at my records.", None),
    ("2026-06-19 09:10", 1, "me", "Can you send an itemized list with receipts for the $1,150?", None),
    ("2026-06-25 12:00", 1, "me", "Following up on the itemized list.", None),
    ("2026-06-27 14:44", 1, "dana", "Attached.", "Deductions_itemized.pdf"),
    ("2026-06-27 14:50", 1, "me", "The ceiling invoice is dated January 9. That's the repair from the leak, months before we moved out.", None),
    ("2026-06-27 15:30", 1, "dana", "Let's talk on the phone.", None),
    ("2026-06-27 15:32", 1, "me", "I'd prefer to keep this in writing.", None),
    ("2026-06-27 15:40", 2, "priya", "did she send the itemized list?", None),
    ("2026-06-27 15:42", 2, "me", "yes. the ceiling invoice is from January, before we moved out", None),
    ("2026-06-27 15:43", 2, "priya", "so she's charging us for her own repair?", None),
]

APPLE_EPOCH = datetime(2001, 1, 1, tzinfo=timezone.utc)


def apple_ns(local: str) -> int:
    dt = datetime.strptime(local, "%Y-%m-%d %H:%M").astimezone()  # interpret in this Mac's time zone
    return int((dt - APPLE_EPOCH).total_seconds() * 1_000_000_000)


def write_messages_db(path: Path):
    """A small database with the same tables as a real Messages database."""
    if path.exists():
        path.unlink()
    c = sqlite3.connect(path)
    c.executescript("""
    CREATE TABLE handle(ROWID INTEGER PRIMARY KEY, id TEXT);
    CREATE TABLE chat(ROWID INTEGER PRIMARY KEY, chat_identifier TEXT, display_name TEXT);
    CREATE TABLE chat_handle_join(chat_id INT, handle_id INT);
    CREATE TABLE chat_message_join(chat_id INT, message_id INT);
    CREATE TABLE message(ROWID INTEGER PRIMARY KEY, guid TEXT, date INT, is_from_me INT, service TEXT, text TEXT,
      attributedBody BLOB, cache_has_attachments INT, associated_message_type INT, handle_id INT);
    CREATE TABLE attachment(ROWID INTEGER PRIMARY KEY, filename TEXT, transfer_name TEXT);
    CREATE TABLE message_attachment_join(message_id INT, attachment_id INT);
    """)
    handle_ids = {}
    for n, (key, (_, _, addr)) in enumerate(CONTACTS.items(), 1):
        c.execute("INSERT INTO handle VALUES (?, ?)", (n, addr))
        handle_ids[key] = n
    for chat_id, (name, members) in CHATS.items():
        ident = f"chat{chat_id}" if len(members) > 1 else CONTACTS[members[0]][2]
        c.execute("INSERT INTO chat VALUES (?, ?, ?)", (chat_id, ident, name))
        for m in members:
            c.execute("INSERT INTO chat_handle_join VALUES (?, ?)", (chat_id, handle_ids[m]))
    for n, (when, chat_id, who, text, att) in enumerate(sorted(MESSAGES, key=lambda m: m[0]), 1):
        me = who == "me"
        c.execute("INSERT INTO message VALUES (?,?,?,?,?,?,?,?,?,?)", (
            n, f"DEMO-{n:04d}", apple_ns(when), int(me), "iMessage", text, None,
            int(bool(att)), 0, 0 if me else handle_ids[who]))
        c.execute("INSERT INTO chat_message_join VALUES (?, ?)", (chat_id, n))
        if att:
            c.execute("INSERT INTO attachment (filename, transfer_name) VALUES (?, ?)", (f"~/Library/Messages/Attachments/{att}", att))
            c.execute("INSERT INTO message_attachment_join VALUES (?, ?)", (n, c.execute("SELECT last_insert_rowid()").fetchone()[0]))
    c.commit()
    c.close()


def write_contacts_db(path: Path):
    """iPhone-style AddressBook so senders show as names."""
    if path.exists():
        path.unlink()
    c = sqlite3.connect(path)
    c.executescript("""
    CREATE TABLE ABPerson(ROWID INTEGER PRIMARY KEY, First TEXT, Last TEXT, Organization TEXT);
    CREATE TABLE ABMultiValue(UID INTEGER PRIMARY KEY, record_id INT, property INT, value TEXT);
    """)
    for n, (first, last, addr) in enumerate(CONTACTS.values(), 1):
        c.execute("INSERT INTO ABPerson VALUES (?, ?, ?, NULL)", (n, first, last))
        c.execute("INSERT INTO ABMultiValue (record_id, property, value) VALUES (?, ?, ?)",
                  (n, 4 if "@" in addr else 3, addr))
    c.commit()
    c.close()


def build_demo() -> Path:
    DEMO_DIR.mkdir(parents=True, exist_ok=True)
    db, contacts, index = DEMO_DIR / "chat.db", DEMO_DIR / "contacts.sqlitedb", DEMO_DIR / "index.sqlite"
    write_messages_db(db)
    write_contacts_db(contacts)
    build_index.build(db, index, [], "Sam", use_contacts=False, ios_contacts=contacts,
                      source_desc="Demo data: fictional messages generated by imsg/demo.py. Not a real device.")
    # use_contacts=False skips this Mac's Contacts; load only the demo contacts.
    return index


def main(argv=None):
    index = build_demo()
    server.DEMO = True
    server.main(["--index", str(index), "--port", str(PORT)] + (argv if argv is not None else ["--open"]))


if __name__ == "__main__":
    main(sys.argv[1:] or None)
