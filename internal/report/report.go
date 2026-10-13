// Package report builds the printable report (HTML, then Save as PDF in the browser)
// and the CSV of selected messages.
//
// The report is laid out for someone reviewing the messages as a record, such as a
// lawyer: cover page, source and method, an optional AI summary that cites numbered
// messages (M-0001…), an index of matching messages, and the full transcript with
// the surrounding conversation.
package report

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/marianina8/imessage-search/internal/pystr"
)

// MaxRows caps a report's size.
const MaxRows = 5000

// SystemPrompt is the instruction for the report's AI summary.
const SystemPrompt = `You are helping someone prepare their text messages for review by their lawyer.
Write a neutral, precise summary of the messages provided.

Rules:
- Every factual statement must cite the message reference(s) that support it, in square brackets, e.g. [M-0012] or [M-0012, M-0015].
- Quote short phrases exactly (in quotation marks) when the precise wording matters.
- Keep what someone said separate from what actually happened, e.g. "Jordan wrote that she paid the invoice [M-0040]", not "Jordan paid the invoice".
- Do not speculate, characterize motives or emotions, or draw legal conclusions.
- Point out ambiguities, contradictions, and gaps (for example, references to calls or conversations that aren't in the messages).

Use exactly these Markdown sections:
## Overview
(2–4 sentences)
## Key points
(bullets, most important first)
## Timeline
(bullets in date order: "**Mon DD, YYYY** — event [refs]")
## Statements by participant
(a ### heading per person, with bullets)
## Ambiguities and gaps
(bullets)`

// Row is one message in a report.
type Row struct {
	ID            int64
	GUID          sql.NullString
	ChatID        int64
	Chat          sql.NullString
	TS            sql.NullFloat64
	Date          string
	Sender        string
	IsFromMe      bool
	Text          string
	HasAttachment bool
	Attachments   string

	Ref     string // M-0001
	Matched bool   // matched the search (the rest are context)
	Excerpt int    // which excerpt it belongs to
}

func (r *Row) tsOr0() float64 {
	if r.TS.Valid {
		return r.TS.Float64
	}
	return 0
}

// Collect returns the matched messages plus contextN messages either side, grouped
// into excerpts, in report order.
func Collect(conn *sql.DB, ids []int64, contextN int) ([]*Row, error) {
	att := "'' AS attachments"
	if hasColumn(conn, "messages", "attachments") {
		att = "m.attachments"
	}
	var unique []int64
	matched := map[int64]bool{}
	for _, id := range ids {
		if !matched[id] {
			matched[id] = true
			unique = append(unique, id)
		}
	}

	// Locate each match inside its conversation.
	var order []int64
	windows := map[int64][]any{}
	for _, id := range unique {
		var chat int64
		var ts sql.NullFloat64
		if err := conn.QueryRow("SELECT chat_id, ts FROM messages WHERE id=?", id).Scan(&chat, &ts); err != nil {
			continue
		}
		if _, ok := windows[chat]; !ok {
			order = append(order, chat)
		}
		var v any
		if ts.Valid {
			v = ts.Float64
		}
		windows[chat] = append(windows[chat], v)
	}

	q := "SELECT m.id, m.guid, m.chat_id, c.label AS chat, m.ts, m.date, m.sender, m.is_from_me, " +
		"m.text, m.has_attachment, " + att + " FROM messages m LEFT JOIN chats c USING(chat_id) " +
		"WHERE m.chat_id=? AND m.is_reaction=0 AND m.ts %s ? ORDER BY m.ts %s, m.id %s LIMIT ?"
	before := fmt.Sprintf(q, "<", "DESC", "DESC")
	after := fmt.Sprintf(q, ">=", "ASC", "ASC")

	picked := map[int64]*Row{}
	for _, chat := range order {
		for _, ts := range windows[chat] {
			for _, part := range []struct {
				sql   string
				limit int
			}{{before, contextN}, {after, contextN + 1}} {
				rows, err := queryRows(conn, part.sql, chat, ts, part.limit)
				if err != nil {
					return nil, err
				}
				for _, r := range rows {
					if _, ok := picked[r.ID]; !ok {
						picked[r.ID] = r
					}
				}
			}
			if len(picked) > MaxRows {
				break
			}
		}
	}

	rows := make([]*Row, 0, len(picked))
	for _, r := range picked {
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.ChatID != b.ChatID {
			return a.ChatID < b.ChatID
		}
		if a.tsOr0() != b.tsOr0() {
			return a.tsOr0() < b.tsOr0()
		}
		return a.ID < b.ID
	})

	// Split into excerpts: a new one starts when the conversation changes or when
	// there are unselected messages in between.
	seq := map[int64]int{}
	doneChat := map[int64]bool{}
	for _, r := range rows {
		if doneChat[r.ChatID] {
			continue
		}
		doneChat[r.ChatID] = true
		ordered, err := conn.Query("SELECT id FROM messages WHERE chat_id=? AND is_reaction=0 ORDER BY ts, id", r.ChatID)
		if err != nil {
			return nil, err
		}
		n := 0
		for ordered.Next() {
			var id int64
			ordered.Scan(&id)
			seq[id] = n
			n++
		}
		ordered.Close()
	}
	var excerpts [][]*Row
	for _, r := range rows {
		if len(excerpts) > 0 {
			last := excerpts[len(excerpts)-1]
			prev := last[len(last)-1]
			if prev.ChatID == r.ChatID && seq[r.ID]-seq[prev.ID] <= 1 {
				excerpts[len(excerpts)-1] = append(last, r)
				continue
			}
		}
		excerpts = append(excerpts, []*Row{r})
	}
	sort.SliceStable(excerpts, func(i, j int) bool { return excerpts[i][0].tsOr0() < excerpts[j][0].tsOr0() })

	var out []*Row
	n := 0
	for e, ex := range excerpts {
		for _, r := range ex {
			n++
			r.Ref = fmt.Sprintf("M-%04d", n)
			r.Matched = matched[r.ID]
			r.Excerpt = e + 1
			out = append(out, r)
		}
	}
	if len(out) > MaxRows {
		out = out[:MaxRows]
	}
	return out, nil
}

func queryRows(conn *sql.DB, q string, args ...any) ([]*Row, error) {
	rows, err := conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Row
	for rows.Next() {
		r := &Row{}
		var fromMe, hasAtt sql.NullInt64
		var date, sender, text, atts sql.NullString
		if err := rows.Scan(&r.ID, &r.GUID, &r.ChatID, &r.Chat, &r.TS, &date, &sender, &fromMe, &text, &hasAtt, &atts); err != nil {
			return nil, err
		}
		r.Date, r.Sender, r.Text, r.Attachments = date.String, sender.String, text.String, atts.String
		r.IsFromMe, r.HasAttachment = fromMe.Int64 != 0, hasAtt.Int64 != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

func hasColumn(conn *sql.DB, table, col string) bool {
	rows, err := conn.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil && name == col {
			return true
		}
	}
	return false
}

// Meta reads the index's meta table (where the data came from, when it was built…).
func Meta(conn *sql.DB) map[string]string {
	out := map[string]string{}
	rows, err := conn.Query("SELECT key, value FROM meta")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v sql.NullString
		if rows.Scan(&k, &v) == nil {
			out[k] = v.String
		}
	}
	return out
}

// SummaryInput is the transcript sent to the AI, cut off at maxChars characters.
func SummaryInput(rows []*Row, maxChars int) (string, bool) {
	var lines []string
	used := 0
	for _, r := range rows {
		text := r.Text
		if r.Attachments != "" {
			text += " [attachment: " + r.Attachments + "]"
		}
		chat := "None"
		if r.Chat.Valid {
			chat = r.Chat.String
		}
		line := fmt.Sprintf("[%s] %s | %s | %s: %s", r.Ref, strings.Replace(pystr.Prefix(r.Date, 16), "T", " ", 1), chat, r.Sender, text)
		n := pystr.Len(line)
		if used+n > maxChars {
			return strings.Join(lines, "\n"), true
		}
		lines = append(lines, line)
		used += n + 1
	}
	return strings.Join(lines, "\n"), false
}
