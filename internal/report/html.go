package report

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/marianina8/imessage-search/internal/pystr"
)

// Info is everything about a report other than its rows.
type Info struct {
	Title, PreparedBy, Question string
	ContextN                    int
	Filters                     map[string]any
	Chats                       map[int64]string
	Meta                        map[string]string

	Summary          string // empty when there's no AI summary
	Model            string
	SummaryTruncated bool
	SummaryCount     int
}

// fmtDT shows an ISO date as e.g. "Jun 18, 2026, 3:32:05 PM".
func fmtDT(iso string, seconds bool) string {
	if iso == "" {
		return ""
	}
	t, err := pystr.ParseISO(iso)
	if err != nil {
		return iso
	}
	if seconds {
		return t.Format("Jan 2, 2006, 3:04:05 PM")
	}
	return t.Format("Jan 2, 2006, 3:04 PM")
}

func fmtDay(iso string) string {
	if iso == "" {
		return ""
	}
	t, err := pystr.ParseISO(iso)
	if err != nil {
		return iso
	}
	return t.Format("January 2, 2006")
}

func describeFilters(f map[string]any, chats map[int64]string) [][2]string {
	str := func(k string) string {
		switch v := f[k].(type) {
		case string:
			return v
		case nil:
			return ""
		default:
			return fmt.Sprint(v)
		}
	}
	var out [][2]string
	if q := str("q"); q != "" {
		mode := map[string]string{"all": "all of the words", "any": "any of the words", "phrase": "the exact phrase"}[str("mode")]
		out = append(out, [2]string{"Words", fmt.Sprintf("“%s” (%s)", q, mode)})
	}
	if list, ok := f["sender"].([]any); ok && len(list) > 0 {
		var names []string
		for _, s := range list {
			names = append(names, fmt.Sprint(s))
		}
		out = append(out, [2]string{"Sender", strings.Join(names, ", ")})
	}
	if chat := str("chat"); chat != "" {
		label := chat
		var id int64
		if _, err := fmt.Sscan(chat, &id); err == nil {
			if l, ok := chats[id]; ok {
				label = l
			}
		}
		out = append(out, [2]string{"Conversation", label})
	}
	if from, to := str("from"), str("to"); from != "" || to != "" {
		if from == "" {
			from = "beginning"
		}
		if to == "" {
			to = "latest"
		}
		out = append(out, [2]string{"Dates", from + " to " + to})
	}
	if len(out) == 0 {
		return [][2]string{{"Criteria", "All messages"}}
	}
	return out
}

var (
	reStrong = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reRefs   = regexp.MustCompile(`\[(?:M-\d{4}(?:\s*[,;–-]\s*)?)+\]`)
	reRef    = regexp.MustCompile(`M-\d{4}`)
	reLI     = regexp.MustCompile(`^\s*[-*]\s+(.*)`)
	reH      = regexp.MustCompile(`^(#{1,4})\s+(.*)`)
)

// emphasize turns *word* into <em>word</em> with the same rules as the regex
// (?<![\w*])\*(?!\s)(.+?)(?<!\s)\*(?![\w*]), which Go's regexp can't express.
func emphasize(s string) string {
	r := []rune(s)
	isWordOrStar := func(i int) bool { return i >= 0 && i < len(r) && (r[i] == '*' || pystr.IsWord(r[i])) }
	isSpace := func(i int) bool { return i >= 0 && i < len(r) && pystr.IsSpace(r[i]) }
	var out []rune
	i := 0
	for i < len(r) {
		if r[i] == '*' && !isWordOrStar(i-1) && i+1 < len(r) && !isSpace(i+1) && r[i+1] != '\n' {
			end := -1
			for j := i + 2; j < len(r) && r[j] != '\n'; j++ {
				if r[j] == '*' && !isSpace(j-1) && !isWordOrStar(j+1) {
					end = j
					break
				}
			}
			if end > 0 {
				out = append(out, []rune("<em>")...)
				out = append(out, r[i+1:end]...)
				out = append(out, []rune("</em>")...)
				i = end + 1
				continue
			}
		}
		out = append(out, r[i])
		i++
	}
	return string(out)
}

// MarkdownToHTML is a small renderer for the summary; it turns [M-0001] into links.
func MarkdownToHTML(md string, valid map[string]bool) string {
	inline := func(s string) string {
		s = pystr.HTMLEscape(s)
		s = reStrong.ReplaceAllString(s, "<strong>$1</strong>")
		s = emphasize(s)
		return reRefs.ReplaceAllStringFunc(s, func(m string) string {
			var links []string
			for _, p := range reRef.FindAllString(m, -1) {
				if valid[p] {
					links = append(links, fmt.Sprintf(`<a class="ref" href="#%s">%s</a>`, p, p))
				} else {
					links = append(links, p)
				}
			}
			return "[" + strings.Join(links, ", ") + "]"
		})
	}
	var out []string
	inList := false
	for _, line := range splitLines(md) {
		if m := reLI.FindStringSubmatch(line); m != nil {
			if !inList {
				out = append(out, "<ul>")
				inList = true
			}
			out = append(out, "<li>"+inline(m[1])+"</li>")
			continue
		}
		if inList {
			out = append(out, "</ul>")
			inList = false
		}
		if m := reH.FindStringSubmatch(line); m != nil {
			level := 3
			if len(m[1]) > 2 {
				level = 4
			}
			out = append(out, fmt.Sprintf("<h%d>%s</h%d>", level, inline(m[2]), level))
		} else if pystr.Strip(line) != "" {
			out = append(out, "<p>"+inline(line)+"</p>")
		}
	}
	if inList {
		out = append(out, "</ul>")
	}
	return strings.Join(out, "\n")
}

// splitLines is Python's str.splitlines() for the line breaks that matter here.
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// CSV writes the rows as a spreadsheet Excel and Numbers open as UTF-8.
func CSV(rows []*Row) []byte {
	var b bytes.Buffer
	b.WriteString("\uFEFF")
	write := func(fields ...string) {
		for i, f := range fields {
			if i > 0 {
				b.WriteByte(',')
			}
			if strings.ContainsAny(f, ",\"\r\n") {
				f = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
			}
			b.WriteString(f)
		}
		b.WriteString("\r\n")
	}
	yes := func(v bool, y, n string) string {
		if v {
			return y
		}
		return n
	}
	write("Ref", "Matched search", "Excerpt", "Date and time", "Sender", "Sent by me",
		"Conversation", "Message", "Attachments", "Message ROWID", "Message GUID")
	for _, r := range rows {
		write(r.Ref, yes(r.Matched, "yes", "context"), fmt.Sprint(r.Excerpt), r.Date, r.Sender, yes(r.IsFromMe, "yes", "no"),
			r.Chat.String, r.Text, r.Attachments, fmt.Sprint(r.ID), r.GUID.String)
	}
	return b.Bytes()
}

// HTML renders the report page. token is used for the CSV download link.
func HTML(rows []*Row, info Info, token string, now time.Time) string {
	e := pystr.HTMLEscape
	title := info.Title
	m := info.Meta
	var matched []*Row
	senderCount := map[string]int{}
	var senderOrder []string
	var dates []string
	for _, r := range rows {
		if r.Matched {
			matched = append(matched, r)
		}
		if _, ok := senderCount[r.Sender]; !ok {
			senderOrder = append(senderOrder, r.Sender)
		}
		senderCount[r.Sender]++
		if r.Date != "" {
			dates = append(dates, r.Date)
		}
	}
	sort.SliceStable(senderOrder, func(i, j int) bool { return senderCount[senderOrder[i]] > senderCount[senderOrder[j]] })
	dateRange := "—"
	if len(dates) > 0 {
		lo, hi := dates[0], dates[0]
		for _, d := range dates {
			if d < lo {
				lo = d
			}
			if d > hi {
				hi = d
			}
		}
		dateRange = fmtDay(lo) + " – " + fmtDay(hi)
	}
	var participants []string
	for _, s := range senderOrder {
		participants = append(participants, fmt.Sprintf("%s (%s)", s, pystr.Thousands(senderCount[s])))
	}
	preparedBy := info.PreparedBy
	if preparedBy == "" {
		preparedBy = "—"
	}
	count := fmt.Sprintf("%s total: %s matching the search, %s shown for context", pystr.Thousands(len(rows)),
		pystr.Thousands(len(matched)), pystr.Thousands(len(rows)-len(matched)))
	if info.ContextN != 0 {
		count += fmt.Sprintf(" (%d before and after each match)", info.ContextN)
	}
	facts := [][2]string{
		{"Prepared by", e(preparedBy)},
		{"Prepared on", e(now.Format("January 2, 2006 at 3:04 PM MST"))},
		{"Messages in this report", count},
		{"Date range", e(dateRange)},
		{"Participants", e(strings.Join(participants, ", "))},
	}
	for _, kv := range describeFilters(info.Filters, info.Chats) {
		facts = append(facts, [2]string{kv[0], e(kv[1])})
	}
	var factRows strings.Builder
	for _, kv := range facts {
		fmt.Fprintf(&factRows, "<tr><td>%s</td><td>%s</td></tr>", kv[0], kv[1])
	}
	get := func(k, def string) string {
		if v, ok := m[k]; ok {
			return v
		}
		return def
	}
	tz := m["timezone"]
	tzNote := ""
	if tz != "" {
		tzNote = " (" + e(tz) + ")"
	}
	built := fmtDT(m["built_at"], false)
	if built == "" {
		built = "—"
	}
	css := strings.Replace(reportCSS, "REPORT_TITLE", strings.ReplaceAll(strings.ReplaceAll(title, `"`, "'"), `\`, ""), 1)

	var out []string
	out = append(out, fmt.Sprintf(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s – %s</title>
<style>%s</style></head><body>
<div class="toolbar"><button class="primary" onclick="window.print()">Save as PDF</button>
<a href="/report/%s.csv">Download spreadsheet (CSV)</a>
<span>In the print window, choose <b>Save as PDF</b> and turn off “Headers and footers”. Page numbers are added automatically in Chrome.</span></div>
<div class="page">
<section class="cover">
  <h1>%s</h1>
  <p class="sub">Text message report</p>
  <table class="facts">%s</table>
  <p class="end">Messages are reproduced as stored on the device. Reference numbers (M-0001…) identify each message in this
  report and match the accompanying spreadsheet.</p>
</section>

<section class="pb">
  <h2>1. Source and method</h2>
  <table class="facts">
    <tr><td>Source</td><td>%s</td></tr>
    <tr><td>Database fingerprint</td><td><span class="mono">SHA-256 %s</span><br>
      <span class="note">A fingerprint of the exact copy of the Messages database this report was made from. Any change to that file changes the fingerprint.</span></td></tr>
    <tr><td>Search index built</td><td>%s</td></tr>
    <tr><td>Times</td><td>Shown in the Mac's local time zone%s, adjusted for daylight saving time.</td></tr>
    <tr><td>What's included</td><td>Message text exactly as stored. Tapback reactions are left out. Attachments are listed
      by file name; their contents aren't included. Messages marked ★ matched the search; the others are the surrounding
      conversation, included for context.</td></tr>
    <tr><td>Prepared with</td><td>%s</td></tr>
  </table>
</section>
`, e(title), now.Format("2006-01-02"), css, token, e(title), factRows.String(),
		e(get("source_desc", "Not recorded")), e(get("source_sha256", "not recorded")), e(built), tzNote,
		e(get("tool", "iMessage Search"))))

	sec := 2
	if info.Summary != "" {
		author := info.Model
		if strings.HasPrefix(info.Model, "claude") {
			author = "Claude (" + info.Model + ")"
		}
		trunc := ""
		if info.SummaryTruncated {
			trunc = " (the earliest ones, because the full set was too long)"
		}
		focus := ""
		if info.Question != "" {
			focus = ", focusing on: “" + e(info.Question) + "”"
		}
		valid := map[string]bool{}
		for _, r := range rows {
			valid[r.Ref] = true
		}
		out = append(out, fmt.Sprintf(`<section class="pb"><h2>%d. Summary</h2>
<div class="ai"><div class="ai-label">AI-generated summary
<span>Written by %s on %s from the %s messages
in this report%s%s.
It's a reading aid, not evidence or legal advice. Check each point against the cited messages.</span></div>
%s
</div></section>`, sec, e(author), e(now.Format("January 2, 2006")), e(pystr.Thousands(info.SummaryCount)), trunc, focus,
			MarkdownToHTML(info.Summary, valid)))
		sec++
	}

	out = append(out, fmt.Sprintf(`<section class="pb"><h2>%d. Index of matching messages</h2>
<table class="index"><thead><tr><th>Ref</th><th>Date</th><th>Sender</th><th>Message</th></tr></thead><tbody>`, sec))
	for _, r := range matched {
		text := strings.ReplaceAll(r.Text, "\n", " ")
		if utf8.RuneCountInString(text) > 160 {
			text = pystr.Prefix(text, 160) + "…"
		}
		out = append(out, fmt.Sprintf(`<tr><td><a class="ref" href="#%s">%s</a></td><td>%s</td><td>%s</td><td>%s</td></tr>`,
			r.Ref, r.Ref, e(fmtDT(r.Date, false)), e(r.Sender), e(text)))
	}
	out = append(out, "</tbody></table></section>")
	sec++

	out = append(out, fmt.Sprintf(`<section class="pb"><h2>%d. Messages</h2>
<p class="legend"><b>★ Highlighted</b> messages matched the search. The others are the conversation around them.</p>`, sec))
	current := 0
	for i, r := range rows {
		if r.Excerpt != current {
			if current != 0 {
				out = append(out, "</div>")
			}
			current = r.Excerpt
			first, last := r, r
			for _, x := range rows[i:] {
				if x.Excerpt == current {
					last = x
				}
			}
			out = append(out, fmt.Sprintf(`<div class="excerpt"><div class="excerpt-head">Excerpt %d · %s · %s – %s</div>`,
				current, e(r.Chat.String), e(fmtDT(first.Date, false)), e(fmtDT(last.Date, false))))
		}
		att := ""
		if r.Attachments != "" {
			att = `<div class="att">Attachment: ` + e(r.Attachments) + `</div>`
		} else if r.HasAttachment && r.Text == "" {
			att = `<div class="att">[Attachment]</div>`
		}
		match := ""
		if r.Matched {
			match = " match"
		}
		out = append(out, fmt.Sprintf(`<div class="msg%s" id="%s"><div class="r">%s</div><div class="t">%s</div><div><span class="who">%s:</span> <span class="body">%s</span>%s</div></div>`,
			match, r.Ref, r.Ref, e(fmtDT(r.Date, true)), e(r.Sender), e(r.Text), att))
	}
	if current != 0 {
		out = append(out, "</div>")
	}
	out = append(out, "</section></div></body></html>")
	return strings.Join(out, "\n")
}
