// Package index builds the searchable index (data/index.sqlite) from a copy of the
// Messages database (chat.db from a Mac, or sms.db from an iPhone backup).
//
// It reads the source strictly read-only, decodes text that only lives in
// attributedBody, turns phone numbers and emails into contact names, and writes
// everything to a new SQLite file with a full-text (FTS5) index.
package index

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/marianina8/imessage-search/internal/db"
	"github.com/marianina8/imessage-search/internal/pystr"
	"github.com/marianina8/imessage-search/internal/typedstream"
)

var appleEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// AppleTime converts a Messages date: seconds (older) or nanoseconds (newer) since 2001.
// It returns the zero time for 0. Nanoseconds are rounded to microseconds exactly the
// way the Python version did (true division, then timedelta's round-half-even), so
// both versions write the same dates.
func AppleTime(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	var micros int64
	if value > 1e11 {
		secs, _ := new(big.Rat).SetFrac64(value, 1_000_000_000).Float64() // Python's value / 1e9
		whole, frac := math.Modf(secs)
		micros = int64(whole)*1_000_000 + int64(math.RoundToEven(frac*1e6))
	} else {
		micros = value * 1_000_000
	}
	return appleEpoch.Add(time.Duration(micros) * time.Microsecond).Local()
}

// Unix is a time as Python's datetime.timestamp() would give it.
func Unix(t time.Time) float64 { return float64(t.UnixMicro()) / 1e6 }

// Clean strips NULs and the U+FFFC placeholder that stands in for attachments.
func Clean(text string) string {
	text = strings.ReplaceAll(text, "\x00", "")
	text = strings.ReplaceAll(text, "\uFFFC", "")
	return pystr.Strip(text)
}

const chatSQL = `
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
ORDER BY message_count DESC`

const messageSQL = `
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
%s
ORDER BY cmj.chat_id, m.date, m.ROWID`

// Schema is the layout of index.sqlite. The search page and reports read it.
const Schema = `
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
);`

// Options control a build.
type Options struct {
	ChatIDs    []int64           // only these chats (empty = all)
	Me         string            // name shown for your own messages
	Contacts   map[string]string // normalized phone/email -> name
	SourceDesc string            // where the database came from, for reports
	Progress   func(done, total int)
	Log        io.Writer
}

// Result counts what the build did. Keys match the Python version's output.
type Result struct {
	Messages       int `json:"messages"`
	Contacts       int `json:"contacts"`
	Text           int `json:"text"`
	AttributedBody int `json:"attributedBody"`
	Unresolved     int `json:"unresolved"`
	Attachment     int `json:"attachment"`
}

// Build reads dbPath and writes the index to outPath, swapping it in atomically
// so a running server never sees half an index.
func Build(dbPath, outPath string, opt Options) (Result, error) {
	var res Result
	logf := func(format string, a ...any) {
		if opt.Log != nil {
			fmt.Fprintf(opt.Log, format+"\n", a...)
		}
	}
	if opt.Me == "" {
		opt.Me = "Me"
	}
	hash, err := sha256File(dbPath)
	if err != nil {
		return res, err
	}
	src, err := db.OpenReadOnly(dbPath)
	if err != nil {
		return res, err
	}
	defer src.Close()
	contacts := opt.Contacts
	if contacts == nil {
		contacts = map[string]string{}
	}
	if len(contacts) > 0 {
		logf("Contacts resolved: %d phone numbers/emails", len(contacts))
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return res, err
	}
	tmp := outPath + ".building"
	os.Remove(tmp)
	dst, err := db.OpenReadWrite(tmp)
	if err != nil {
		return res, err
	}
	defer func() {
		dst.Close()
		os.Remove(tmp) // gone already after a successful rename
	}()
	if _, err := dst.Exec(Schema); err != nil {
		return res, fmt.Errorf("create index: %w", err)
	}

	want := map[int64]bool{}
	for _, id := range opt.ChatIDs {
		want[id] = true
	}
	tx, err := dst.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	chats, err := ListChats(src, contacts)
	if err != nil {
		return res, err
	}
	for _, c := range chats {
		if len(want) > 0 && !want[c.ID] {
			continue
		}
		if _, err := tx.Exec("INSERT INTO chats VALUES (?,?,?,?)", c.ID, c.Label, c.Identifier, c.MessageCount); err != nil {
			return res, err
		}
	}

	where, args := "", []any{}
	if len(opt.ChatIDs) > 0 {
		where = "WHERE cmj.chat_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(opt.ChatIDs)), ",") + ")"
		for _, id := range opt.ChatIDs {
			args = append(args, id)
		}
	}
	var total int
	if err := src.QueryRow("SELECT COUNT(*) FROM message m JOIN chat_message_join cmj ON cmj.message_id = m.ROWID "+where, args...).Scan(&total); err != nil {
		return res, err
	}
	attachments := loadAttachments(src)

	insert, err := tx.Prepare("INSERT OR IGNORE INTO messages VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)")
	if err != nil {
		return res, err
	}
	defer insert.Close()

	rows, err := src.Query(fmt.Sprintf(messageSQL, where), args...)
	if err != nil {
		return res, err
	}
	defer rows.Close()
	done := 0
	for rows.Next() {
		var (
			id, chatID                        int64
			guid, service, text, handle       sql.NullString
			date, isFromMe, hasAtt, assocType sql.NullInt64
			body                              []byte
		)
		if err := rows.Scan(&id, &guid, &date, &isFromMe, &service, &text, &body, &hasAtt, &assocType, &handle, &chatID); err != nil {
			return res, err
		}
		done++
		if opt.Progress != nil && done%2000 == 0 {
			opt.Progress(done, total)
		}
		msg := Clean(text.String)
		source := "text"
		if msg == "" && len(body) > 0 {
			msg = Clean(typedstream.Text(body))
			if msg != "" {
				source = "attributedBody"
			} else {
				source = "unresolved"
			}
		}
		if msg == "" && hasAtt.Int64 != 0 {
			source = "attachment"
		}
		switch source {
		case "text":
			res.Text++
		case "attributedBody":
			res.AttributedBody++
		case "unresolved":
			res.Unresolved++
		case "attachment":
			res.Attachment++
		}

		var ts any
		dateStr := ""
		if t := AppleTime(date.Int64); !t.IsZero() {
			ts = Unix(t)
			dateStr = pystr.ISOFormat(t)
		}
		sender := opt.Me
		if isFromMe.Int64 == 0 {
			sender = handle.String
			if sender == "" {
				sender = "(unknown)"
			}
			if name, ok := contacts[NormalizeHandle(handle.String)]; ok {
				sender = name
			}
		}
		reaction := assocType.Int64 >= 2000 && assocType.Int64 < 4000 // tapbacks
		if _, err := insert.Exec(id, guid, chatID, ts, dateStr, b2i(isFromMe.Int64 != 0), handle.String, sender,
			service.String, msg, source, b2i(hasAtt.Int64 != 0), b2i(reaction), attachments[id]); err != nil {
			return res, err
		}
	}
	if err := rows.Err(); err != nil {
		return res, err
	}
	if opt.Progress != nil {
		opt.Progress(total, total)
	}
	if _, err := tx.Exec("INSERT INTO messages_fts(messages_fts) VALUES ('rebuild')"); err != nil {
		return res, err
	}
	desc := opt.SourceDesc
	if desc == "" {
		desc = "Messages database file " + filepath.Base(dbPath)
	}
	abs, _ := filepath.Abs(dbPath)
	meta := [][2]string{
		{"built_at", pystr.ISOSeconds(time.Now())},
		{"source_desc", desc},
		{"source_file", abs},
		{"source_sha256", hash},
		{"me_name", opt.Me},
		{"timezone", LocalTimezoneName()},
		{"tool", "iMessage Search (github.com/marianina8/imessage-search)"},
	}
	for _, kv := range meta {
		if _, err := tx.Exec("INSERT INTO meta VALUES (?, ?)", kv[0], kv[1]); err != nil {
			return res, err
		}
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	if err := dst.QueryRow("SELECT COUNT(*) FROM messages").Scan(&res.Messages); err != nil {
		return res, err
	}
	res.Contacts = len(contacts)
	dst.Close()
	src.Close()
	if err := os.Rename(tmp, outPath); err != nil {
		return res, err
	}
	logf("Indexed %d messages -> %s", res.Messages, outPath)
	logf("  %-15s %d", "text", res.Text)
	logf("  %-15s %d", "attributedBody", res.AttributedBody)
	logf("  %-15s %d", "unresolved", res.Unresolved)
	logf("  %-15s %d", "attachment", res.Attachment)
	return res, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Chat is one conversation in the source database.
type Chat struct {
	ID           int64
	Identifier   sql.NullString // NULL stays NULL, as in the source
	Label        string
	MessageCount int64
}

// ListChats returns every conversation, biggest first, with a readable label.
func ListChats(src *sql.DB, contacts map[string]string) ([]Chat, error) {
	rows, err := src.Query(chatSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		var c Chat
		var display, participants sql.NullString
		if err := rows.Scan(&c.ID, &c.Identifier, &display, &c.MessageCount, &participants); err != nil {
			return nil, err
		}
		c.Label = chatLabel(display.String, participants.String, c.Identifier.String, contacts)
		out = append(out, c)
	}
	return out, rows.Err()
}

func chatLabel(display, participants, identifier string, contacts map[string]string) string {
	if display != "" {
		return display
	}
	list := participants
	if list == "" {
		list = identifier
	}
	var names []string
	for _, p := range strings.Split(list, ",") {
		if p == "" {
			continue
		}
		if name, ok := contacts[NormalizeHandle(p)]; ok {
			p = name
		}
		names = append(names, p)
	}
	if len(names) == 0 {
		return "(unknown)"
	}
	return strings.Join(names, ", ")
}

func loadAttachments(src *sql.DB) map[int64]string {
	rows, err := src.Query(`
            SELECT maj.message_id, COALESCE(a.transfer_name, a.filename, 'attachment')
            FROM message_attachment_join maj JOIN attachment a ON a.ROWID = maj.attachment_id`)
	if err != nil {
		return map[int64]string{}
	}
	defer rows.Close()
	lists := map[int64][]string{}
	for rows.Next() {
		var id int64
		var name any
		if rows.Scan(&id, &name) != nil {
			continue
		}
		lists[id] = append(lists[id], baseName(fmt.Sprint(asString(name))))
	}
	out := make(map[int64]string, len(lists))
	for id, names := range lists {
		out[id] = strings.Join(names, "; ")
	}
	return out
}

func asString(v any) any {
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

// baseName is Python's Path(name).name.
func baseName(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	if p == "." {
		return ""
	}
	return p
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// LocalTimezoneName is the IANA name of this machine's time zone, e.g. America/Los_Angeles.
func LocalTimezoneName() string {
	tz := strings.TrimPrefix(os.Getenv("TZ"), ":")
	if strings.Contains(tz, "/") && !strings.HasPrefix(tz, "/") {
		return tz
	}
	if link, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(link, "zoneinfo/"); i >= 0 {
			return link[i+len("zoneinfo/"):]
		}
	}
	name, _ := time.Now().Zone()
	return name
}
