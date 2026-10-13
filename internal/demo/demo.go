// Package demo builds a small set of fictional messages so anyone can try the app
// without reading their own messages.
//
// The story: Sam and roommate Priya move out of their apartment, and landlord
// Dana withholds part of the security deposit for a ceiling repair and cleaning.
// Every name, number, and message is made up.
package demo

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/marianina8/imessage-search/internal/db"
	"github.com/marianina8/imessage-search/internal/index"
)

// Port is where the demo runs, so it doesn't clash with the real app.
const Port = 8766

type contact struct{ first, last, address string }

var contactKeys = []string{"dana", "priya", "jordan"}

var contacts = map[string]contact{
	"dana":   {"Dana", "Whitfield", "+15550102233"},
	"priya":  {"Priya", "Shah", "+15550104455"},
	"jordan": {"Jordan", "Lee", "jordan.lee@example.com"},
}

type chat struct {
	name    string
	members []string
}

var chats = []struct {
	id int
	chat
}{
	{1, chat{"", []string{"dana"}}},
	{2, chat{"", []string{"priya"}}},
	{3, chat{"Moving crew", []string{"priya", "jordan"}}},
}

// message: local date & time, chat, sender key or "me", text, attachment file name.
type message struct {
	when       string
	chat       int
	who        string
	text       string
	attachment string
}

var messages = []message{
	{"2025-11-03 09:12", 1, "me", "Hi Dana, water is dripping from the bathroom ceiling again. Photos attached.", "IMG_2041.HEIC"},
	{"2025-11-03 09:15", 2, "me", "Leak is back in the bathroom, I texted Dana photos", ""},
	{"2025-11-03 09:20", 2, "priya", "omg again. ok I'll be home Thursday for the plumber", ""},
	{"2025-11-03 09:40", 1, "dana", "Ugh, sorry. I'll have Mike the plumber come by Thursday.", ""},
	{"2025-11-03 09:41", 1, "me", "Thursday works. Priya will be home to let him in.", ""},
	{"2025-11-06 17:05", 1, "dana", "Mike says it's the seal on the upstairs tub. Fixed for now.", ""},
	{"2025-11-06 17:07", 1, "me", "Thanks! The ceiling paint is bubbling though. Will that get repaired?", ""},
	{"2025-11-06 17:30", 1, "dana", "Yes, I'll get it patched and painted. Don't worry about it.", ""},
	{"2025-12-14 10:22", 1, "me", "Hi Dana, the leak is back and the stain on the bathroom ceiling is bigger.", "IMG_2107.HEIC"},
	{"2025-12-14 12:48", 1, "dana", "I'm traveling until the 28th. Can it wait?", ""},
	{"2025-12-14 12:50", 1, "me", "It drips every time someone upstairs showers. We've put a bucket under it.", ""},
	{"2025-12-14 12:52", 1, "dana", "OK, I'll call Mike.", ""},
	{"2025-12-14 12:55", 2, "priya", "bucket is in place lol", ""},
	{"2025-12-19 08:15", 1, "dana", "Mike can't come until January. Keep using the bucket, sorry.", ""},
	{"2025-12-19 08:30", 2, "me", "Dana says the plumber can't come till January", ""},
	{"2025-12-19 08:33", 2, "priya", "that's ridiculous, it's been dripping for a week", ""},
	{"2026-01-09 14:30", 1, "dana", "Mike replaced the seal and the drywall in the bathroom ceiling. Painting next week.", ""},
	{"2026-01-09 14:31", 1, "me", "Great, thank you.", ""},
	{"2026-02-02 19:10", 1, "me", "Hi Dana, a heads up that we won't renew. Our lease ends May 31 and we'll be out by then.", ""},
	{"2026-02-02 19:15", 2, "me", "Told Dana we're not renewing", ""},
	{"2026-02-02 19:16", 2, "priya", "🙌", ""},
	{"2026-02-02 20:02", 1, "dana", "Thanks for letting me know. I'll start showing it in April.", ""},
	{"2026-02-02 20:03", 1, "dana", "Please have it cleaned professionally when you leave. The deposit is $2,400.", ""},
	{"2026-02-02 20:05", 1, "me", "Will do.", ""},
	{"2026-03-15 11:00", 1, "me", "By the way, the bathroom ceiling was never painted. It still has the patch.", ""},
	{"2026-03-15 13:24", 1, "dana", "I'll take care of it before the new tenants move in.", ""},
	{"2026-04-10 09:05", 1, "dana", "Can I show the apartment Saturday at 11?", ""},
	{"2026-04-10 09:20", 1, "me", "Saturday at 11 is fine.", ""},
	{"2026-05-24 18:00", 3, "jordan", "I can bring the truck Thursday the 28th after 5", ""},
	{"2026-05-24 18:05", 3, "priya", "you're a lifesaver", ""},
	{"2026-05-24 18:06", 3, "me", "pizza is on us", ""},
	{"2026-05-28 17:10", 3, "jordan", "here, parked out front", ""},
	{"2026-05-28 21:00", 2, "priya", "cleaners booked for tomorrow 10am, $320. I'll venmo you half", ""},
	{"2026-05-28 21:02", 2, "me", "perfect thanks", ""},
	{"2026-05-28 22:30", 3, "me", "thank you both, every box is at the new place", ""},
	{"2026-05-29 13:00", 2, "priya", "sent you $160 for the cleaners", ""},
	{"2026-05-29 16:40", 1, "me", "We're all moved out! Professional cleaners came today. Receipt attached.", "Cleaning_receipt.pdf"},
	{"2026-05-29 16:41", 1, "me", "Keys are in the lockbox. Can we do the walkthrough tomorrow?", ""},
	{"2026-05-29 18:02", 1, "dana", "I'll do it Monday. You don't need to be there.", ""},
	{"2026-05-29 18:05", 1, "me", "I'd prefer to be there. I took a video of every room today.", "Moveout_walkthrough.MOV"},
	{"2026-05-29 18:30", 1, "dana", "Fine, Monday 10am.", ""},
	{"2026-06-01 10:48", 1, "dana", "Walkthrough done. Looks mostly good.", ""},
	{"2026-06-01 10:50", 1, "me", "Great. When should we expect the deposit?", ""},
	{"2026-06-01 11:15", 1, "dana", "Within 21 days, per the law.", ""},
	{"2026-06-18 15:32", 1, "dana", "I'm keeping $1,150 of the deposit: $900 for the bathroom ceiling repair and $250 for cleaning. The $1,250 refund was mailed today.", ""},
	{"2026-06-18 15:40", 1, "me", "The ceiling damage came from the leak you were fixing. We reported it in November and again in December.", ""},
	{"2026-06-18 15:41", 1, "me", "And we had it professionally cleaned. I sent you the receipt on May 29.", ""},
	{"2026-06-18 15:45", 2, "me", "Dana is keeping $1,150 of the deposit, including $250 for cleaning!", ""},
	{"2026-06-18 15:47", 2, "priya", "WHAT. we paid $320 for professional cleaners", ""},
	{"2026-06-18 15:48", 2, "priya", "and the ceiling was her leak", ""},
	{"2026-06-18 15:50", 2, "me", "I'm pulling all our texts together. You still have the photos from December?", ""},
	{"2026-06-18 15:52", 2, "priya", "yes, sending", "IMG_2110.HEIC"},
	{"2026-06-18 16:55", 1, "dana", "The ceiling got worse because you didn't report the leak fast enough.", ""},
	{"2026-06-18 16:58", 1, "me", "I texted you the same day both times. You said Mike couldn't come until January.", ""},
	{"2026-06-18 17:30", 1, "dana", "I'll need to look at my records.", ""},
	{"2026-06-19 09:10", 1, "me", "Can you send an itemized list with receipts for the $1,150?", ""},
	{"2026-06-25 12:00", 1, "me", "Following up on the itemized list.", ""},
	{"2026-06-27 14:44", 1, "dana", "Attached.", "Deductions_itemized.pdf"},
	{"2026-06-27 14:50", 1, "me", "The ceiling invoice is dated January 9. That's the repair from the leak, months before we moved out.", ""},
	{"2026-06-27 15:30", 1, "dana", "Let's talk on the phone.", ""},
	{"2026-06-27 15:32", 1, "me", "I'd prefer to keep this in writing.", ""},
	{"2026-06-27 15:40", 2, "priya", "did she send the itemized list?", ""},
	{"2026-06-27 15:42", 2, "me", "yes. the ceiling invoice is from January, before we moved out", ""},
	{"2026-06-27 15:43", 2, "priya", "so she's charging us for her own repair?", ""},
}

var appleEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// appleNS is a local date and time as nanoseconds since 2001, like Messages stores it.
func appleNS(local string) int64 {
	t, err := time.ParseInLocation("2006-01-02 15:04", local, time.Local)
	if err != nil {
		panic(err)
	}
	return t.Sub(appleEpoch).Nanoseconds()
}

// writeMessagesDB makes a small database with the same tables as a real Messages database.
func writeMessagesDB(path string) error {
	os.Remove(path)
	c, err := db.OpenReadWrite(path)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Exec(`
    CREATE TABLE handle(ROWID INTEGER PRIMARY KEY, id TEXT);
    CREATE TABLE chat(ROWID INTEGER PRIMARY KEY, chat_identifier TEXT, display_name TEXT);
    CREATE TABLE chat_handle_join(chat_id INT, handle_id INT);
    CREATE TABLE chat_message_join(chat_id INT, message_id INT);
    CREATE TABLE message(ROWID INTEGER PRIMARY KEY, guid TEXT, date INT, is_from_me INT, service TEXT, text TEXT,
      attributedBody BLOB, cache_has_attachments INT, associated_message_type INT, handle_id INT);
    CREATE TABLE attachment(ROWID INTEGER PRIMARY KEY, filename TEXT, transfer_name TEXT);
    CREATE TABLE message_attachment_join(message_id INT, attachment_id INT);`); err != nil {
		return err
	}
	handles := map[string]int{}
	for i, key := range contactKeys {
		if _, err := c.Exec("INSERT INTO handle VALUES (?, ?)", i+1, contacts[key].address); err != nil {
			return err
		}
		handles[key] = i + 1
	}
	for _, ch := range chats {
		ident := fmt.Sprintf("chat%d", ch.id)
		if len(ch.members) == 1 {
			ident = contacts[ch.members[0]].address
		}
		if _, err := c.Exec("INSERT INTO chat VALUES (?, ?, ?)", ch.id, ident, ch.name); err != nil {
			return err
		}
		for _, m := range ch.members {
			if _, err := c.Exec("INSERT INTO chat_handle_join VALUES (?, ?)", ch.id, handles[m]); err != nil {
				return err
			}
		}
	}
	sorted := append([]message(nil), messages...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].when < sorted[j].when })
	for i, m := range sorted {
		n := i + 1
		me := m.who == "me"
		handle := 0
		if !me {
			handle = handles[m.who]
		}
		if _, err := c.Exec("INSERT INTO message VALUES (?,?,?,?,?,?,?,?,?,?)",
			n, fmt.Sprintf("DEMO-%04d", n), appleNS(m.when), b2i(me), "iMessage", m.text, nil, b2i(m.attachment != ""), 0, handle); err != nil {
			return err
		}
		if _, err := c.Exec("INSERT INTO chat_message_join VALUES (?, ?)", m.chat, n); err != nil {
			return err
		}
		if m.attachment != "" {
			res, err := c.Exec("INSERT INTO attachment (filename, transfer_name) VALUES (?, ?)", "~/Library/Messages/Attachments/"+m.attachment, m.attachment)
			if err != nil {
				return err
			}
			id, _ := res.LastInsertId()
			if _, err := c.Exec("INSERT INTO message_attachment_join VALUES (?, ?)", n, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeContactsDB makes an iPhone-style AddressBook so senders show as names.
func writeContactsDB(path string) error {
	os.Remove(path)
	c, err := db.OpenReadWrite(path)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.Exec(`
    CREATE TABLE ABPerson(ROWID INTEGER PRIMARY KEY, First TEXT, Last TEXT, Organization TEXT);
    CREATE TABLE ABMultiValue(UID INTEGER PRIMARY KEY, record_id INT, property INT, value TEXT);`); err != nil {
		return err
	}
	for i, key := range contactKeys {
		ct := contacts[key]
		if _, err := c.Exec("INSERT INTO ABPerson VALUES (?, ?, ?, NULL)", i+1, ct.first, ct.last); err != nil {
			return err
		}
		prop := 3
		if containsAt(ct.address) {
			prop = 4
		}
		if _, err := c.Exec("INSERT INTO ABMultiValue (record_id, property, value) VALUES (?, ?, ?)", i+1, prop, ct.address); err != nil {
			return err
		}
	}
	return nil
}

func containsAt(s string) bool {
	for _, r := range s {
		if r == '@' {
			return true
		}
	}
	return false
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Build writes the demo databases into dir and indexes them. It returns the index path.
// This Mac's own Contacts are never read.
func Build(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	chatDB, contactsDB, idx := filepath.Join(dir, "chat.db"), filepath.Join(dir, "contacts.sqlitedb"), filepath.Join(dir, "index.sqlite")
	if err := writeMessagesDB(chatDB); err != nil {
		return "", err
	}
	if err := writeContactsDB(contactsDB); err != nil {
		return "", err
	}
	_, err := index.Build(chatDB, idx, index.Options{
		Me:         "Sam",
		Contacts:   index.LoadIOSContacts(contactsDB, os.Stderr),
		SourceDesc: "Demo data: fictional messages generated by the demo command. Not a real device.",
		Log:        os.Stdout,
	})
	return idx, err
}
