package server

import (
	"database/sql"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/marianina8/imessage-search/internal/pystr"
)

var reTerms = regexp.MustCompile(`"[^"]+"|\S+`)

// ftsQuery turns what the user typed into a safe FTS5 MATCH expression.
func ftsQuery(q, mode string) string {
	if mode == "phrase" {
		return `"` + strings.ReplaceAll(q, `"`, " ") + `"`
	}
	var parts []string
	for _, t := range reTerms.FindAllString(q, -1) {
		if strings.HasPrefix(t, `"`) && strings.HasSuffix(t, `"`) && len(t) > 2 {
			parts = append(parts, `"`+strings.ReplaceAll(t[1:len(t)-1], `"`, " ")+`"`)
			continue
		}
		t = strings.ReplaceAll(t, `"`, "")
		if t == "" {
			continue
		}
		prefix := strings.HasSuffix(t, "*")
		t = strings.TrimRight(t, "*")
		if t != "" {
			p := `"` + t + `"`
			if prefix {
				p += "*"
			}
			parts = append(parts, p)
		}
	}
	sep := " AND "
	if mode == "any" {
		sep = " OR "
	}
	return strings.Join(parts, sep)
}

// dateToTS turns a date from the page (YYYY-MM-DD, local time) into unix seconds.
// end=true means the end of that day.
func dateToTS(s string, end bool) (float64, bool) {
	if s == "" {
		return 0, false
	}
	t, err := pystr.ParseISO(s)
	if err != nil {
		return 0, false
	}
	if end && len(s) <= 10 {
		t = time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, t.Location())
	}
	return float64(t.UnixMicro()) / 1e6, true
}

// Message is one search result or context message, as the page expects it.
type Message struct {
	ID            int64   `json:"id"`
	ChatID        *int64  `json:"chat_id,omitempty"`
	Chat          *string `json:"chat,omitempty"`
	Date          string  `json:"date"`
	Sender        string  `json:"sender"`
	IsFromMe      int64   `json:"is_from_me"`
	Text          string  `json:"text"`
	HasAttachment *int64  `json:"has_attachment,omitempty"`
}

func search(conn *sql.DB, q map[string]string, senders []string) map[string]any {
	text := strings.TrimSpace(q["q"])
	mode := q["mode"]
	if mode == "" {
		mode = "all"
	}
	limit := 200
	if q["limit"] != "" {
		v, err := strconv.Atoi(q["limit"])
		if err != nil {
			return map[string]any{"error": "Search syntax problem: bad limit", "total": 0, "results": []Message{}}
		}
		limit = v
	}
	if limit > 2000 {
		limit = 2000
	}
	offset, _ := strconv.Atoi(q["offset"])

	where := []string{"m.text != ''"}
	var args []any
	join := ""
	if text != "" {
		join = "JOIN messages_fts f ON f.rowid = m.id"
		where = append(where, "messages_fts MATCH ?")
		args = append(args, ftsQuery(text, mode))
	}
	var who []any
	for _, s := range senders {
		if s != "" {
			who = append(who, s)
		}
	}
	if len(who) > 0 {
		where = append(where, "m.sender IN ("+strings.TrimSuffix(strings.Repeat("?,", len(who)), ",")+")")
		args = append(args, who...)
	}
	if chat := q["chat"]; chat != "" {
		id, err := strconv.ParseInt(chat, 10, 64)
		if err != nil {
			return map[string]any{"error": "Search syntax problem: bad conversation", "total": 0, "results": []Message{}}
		}
		where = append(where, "m.chat_id = ?")
		args = append(args, id)
	}
	if ts, ok := dateToTS(q["from"], false); ok {
		where = append(where, "m.ts >= ?")
		args = append(args, ts)
	}
	if ts, ok := dateToTS(q["to"], true); ok {
		where = append(where, "m.ts <= ?")
		args = append(args, ts)
	}
	if q["reactions"] != "1" {
		where = append(where, "m.is_reaction = 0")
	}
	order := "m.ts DESC"
	if q["sort"] == "oldest" {
		order = "m.ts ASC"
	}
	base := "FROM messages m " + join + " LEFT JOIN chats c ON c.chat_id = m.chat_id WHERE " + strings.Join(where, " AND ")

	var total int
	if err := conn.QueryRow("SELECT COUNT(*) "+base, args...).Scan(&total); err != nil {
		return map[string]any{"error": "Search syntax problem: " + err.Error(), "total": 0, "results": []Message{}}
	}
	rows, err := conn.Query("SELECT m.id, m.chat_id, c.label AS chat, m.date, m.sender, m.is_from_me, m.text, m.has_attachment "+
		base+" ORDER BY "+order+" LIMIT ? OFFSET ?", append(args, limit, offset)...)
	if err != nil {
		return map[string]any{"error": "Search syntax problem: " + err.Error(), "total": 0, "results": []Message{}}
	}
	defer rows.Close()
	results := []Message{}
	for rows.Next() {
		var m Message
		var chatID, hasAtt sql.NullInt64
		var chat, date, sender, txt sql.NullString
		var fromMe sql.NullInt64
		if err := rows.Scan(&m.ID, &chatID, &chat, &date, &sender, &fromMe, &txt, &hasAtt); err != nil {
			return map[string]any{"error": "Search syntax problem: " + err.Error(), "total": 0, "results": []Message{}}
		}
		m.ChatID, m.Chat, m.HasAttachment = &chatID.Int64, nullable(chat), &hasAtt.Int64
		m.Date, m.Sender, m.Text, m.IsFromMe = date.String, sender.String, txt.String, fromMe.Int64
		results = append(results, m)
	}
	return map[string]any{"total": total, "results": results}
}

func nullable(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

// conversationAround returns n messages before and n after a message, in its conversation.
func conversationAround(conn *sql.DB, id int64, n int) ([]Message, error) {
	var chat int64
	var ts sql.NullFloat64
	if err := conn.QueryRow("SELECT chat_id, ts FROM messages WHERE id = ?", id).Scan(&chat, &ts); err != nil {
		if err == sql.ErrNoRows {
			return []Message{}, nil
		}
		return nil, err
	}
	var tsArg any
	if ts.Valid {
		tsArg = ts.Float64
	}
	read := func(q string, limit int) ([]Message, error) {
		rows, err := conn.Query(q, chat, tsArg, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []Message
		for rows.Next() {
			var m Message
			var date, sender, txt sql.NullString
			var fromMe sql.NullInt64
			if err := rows.Scan(&m.ID, &date, &sender, &fromMe, &txt); err != nil {
				return nil, err
			}
			m.Date, m.Sender, m.Text, m.IsFromMe = date.String, sender.String, txt.String, fromMe.Int64
			out = append(out, m)
		}
		return out, rows.Err()
	}
	before, err := read("SELECT id, date, sender, is_from_me, text FROM messages WHERE chat_id=? AND ts < ? AND is_reaction=0 "+
		"ORDER BY ts DESC LIMIT ?", n)
	if err != nil {
		return nil, err
	}
	after, err := read("SELECT id, date, sender, is_from_me, text FROM messages WHERE chat_id=? AND ts >= ? AND is_reaction=0 "+
		"ORDER BY ts ASC LIMIT ?", n+1)
	if err != nil {
		return nil, err
	}
	out := []Message{}
	for i := len(before) - 1; i >= 0; i-- {
		out = append(out, before[i])
	}
	return append(out, after...), nil
}

type senderCount struct {
	Sender string `json:"sender"`
	N      int64  `json:"n"`
}

func senders(conn *sql.DB) []senderCount {
	out := []senderCount{}
	rows, err := conn.Query("SELECT sender, COUNT(*) AS n FROM messages WHERE is_reaction=0 GROUP BY sender ORDER BY n DESC")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var s sql.NullString
		var c senderCount
		if rows.Scan(&s, &c.N) == nil {
			c.Sender = s.String
			out = append(out, c)
		}
	}
	return out
}

type chatInfo struct {
	ChatID       int64  `json:"chat_id"`
	Label        string `json:"label"`
	MessageCount int64  `json:"message_count"`
}

func chats(conn *sql.DB) []chatInfo {
	out := []chatInfo{}
	rows, err := conn.Query("SELECT chat_id, label, message_count FROM chats ORDER BY message_count DESC")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var c chatInfo
		var label sql.NullString
		var n sql.NullInt64
		if rows.Scan(&c.ChatID, &label, &n) == nil {
			c.Label, c.MessageCount = label.String, n.Int64
			out = append(out, c)
		}
	}
	return out
}
