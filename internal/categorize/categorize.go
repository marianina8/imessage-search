// Package categorize is the keyword-category review: it finds messages matching
// topic categories, groups them into conversations, scores them, and writes a
// Markdown report plus CSVs.
//
// Categories, weights and bonus combinations live in a JSON config (see
// categories.example.json). Patterns are regular expressions, matched without
// regard to case. They use Go's syntax, which has no lookbehind or backreferences.
package categorize

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/marianina8/imessage-search/internal/db"
	"github.com/marianina8/imessage-search/internal/pystr"
)

// Config is categories.example.json / categories.local.json.
type Config struct {
	Categories       map[string]Category `json:"categories"`
	OverlapBonus     map[string]int      `json:"overlap_bonus"`
	Combos           []Combo             `json:"combos"`
	LowSignalOnly    []string            `json:"low_signal_only"`
	LowSignalPenalty *int                `json:"low_signal_penalty"`

	order    []string                    // category names in file order
	compiled map[string][]*regexp.Regexp // name -> patterns
}

type Category struct {
	Weight   *int     `json:"weight"`
	Patterns []string `json:"patterns"`
}

type Combo struct {
	All   []string `json:"all"`
	Any   []string `json:"any"`
	Bonus int      `json:"bonus"`
}

// LoadConfig reads and compiles a config file.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.order, err = categoryOrder(b); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if cfg.OverlapBonus == nil {
		cfg.OverlapBonus = map[string]int{"2": 3, "3": 5, "5": 8}
	}
	cfg.compiled = map[string][]*regexp.Regexp{}
	for _, name := range cfg.order {
		for i, p := range cfg.Categories[name].Patterns {
			re, err := regexp.Compile("(?i)" + p)
			if err != nil {
				return nil, fmt.Errorf("category %q, pattern %d (%q): %v (Go regular expressions have no lookbehind or backreferences)", name, i+1, p, err)
			}
			cfg.compiled[name] = append(cfg.compiled[name], re)
		}
	}
	return &cfg, nil
}

// categoryOrder lists the keys of "categories" in the order they appear in the file.
func categoryOrder(b []byte) ([]string, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(top["categories"]))
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	var order []string
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return nil, err
		}
		order = append(order, k.(string))
		var skip json.RawMessage
		if err := d.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func (c *Config) weight(name string) int {
	if cat, ok := c.Categories[name]; ok && cat.Weight != nil {
		return *cat.Weight
	}
	return 1
}

// Score adds category weights, then the overlap and combo bonuses, minus the
// low-signal penalty.
func (c *Config) Score(cats map[string]bool) int {
	score := 0
	for name := range cats {
		score += c.weight(name)
	}
	for threshold, bonus := range c.OverlapBonus {
		if n, err := strconv.Atoi(threshold); err == nil && len(cats) >= n {
			score += bonus
		}
	}
	for _, combo := range c.Combos {
		ok := true
		for _, need := range combo.All {
			ok = ok && cats[need]
		}
		anyOK := len(combo.Any) == 0
		for _, a := range combo.Any {
			anyOK = anyOK || cats[a]
		}
		if ok && anyOK {
			score += combo.Bonus
		}
	}
	if len(c.LowSignalOnly) > 0 {
		low := map[string]bool{}
		for _, l := range c.LowSignalOnly {
			low[l] = true
		}
		only := true
		for name := range cats {
			only = only && low[name]
		}
		if only {
			penalty := 8
			if c.LowSignalPenalty != nil {
				penalty = *c.LowSignalPenalty
			}
			score -= penalty
		}
	}
	return score
}

// Options for a run.
type Options struct {
	Index    string
	Out      string
	ChatIDs  []int64
	Context  int
	MinScore int
	Title    string
	Stdout   io.Writer
}

type row struct {
	id     int64
	chatID int64
	ts     sql.NullFloat64
	date   string
	sender string
	text   string
	chat   string
}

type hit struct {
	i          int
	row        *row
	categories []string
	start, end int
}

type group struct {
	chatID     int64
	rows       []*row
	start, end int
	hits       []*hit
	categories map[string]bool
	score      int
}

func (g *group) sortedCategories() []string {
	var out []string
	for c := range g.categories {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func (g *group) senders() [][2]string {
	var order []string
	n := map[string]int{}
	for _, h := range g.hits {
		if _, ok := n[h.row.sender]; !ok {
			order = append(order, h.row.sender)
		}
		n[h.row.sender]++
	}
	var out [][2]string
	for _, s := range order {
		out = append(out, [2]string{s, strconv.Itoa(n[s])})
	}
	return out
}

// Run does the review and writes review.md, hits.csv and conversations.csv into opt.Out.
func Run(cfg *Config, opt Options) error {
	out := opt.Stdout
	if out == nil {
		out = os.Stdout
	}
	conn, err := db.OpenReadOnly(opt.Index)
	if err != nil {
		return err
	}
	defer conn.Close()
	where, args := "WHERE is_reaction = 0", []any{}
	if len(opt.ChatIDs) > 0 {
		where += " AND chat_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(opt.ChatIDs)), ",") + ")"
		for _, id := range opt.ChatIDs {
			args = append(args, id)
		}
	}
	rs, err := conn.Query("SELECT m.id, m.chat_id, m.ts, m.date, m.sender, m.text, c.label AS chat FROM messages m LEFT JOIN chats c USING(chat_id) "+
		where+" ORDER BY chat_id, ts, id", args...)
	if err != nil {
		return err
	}
	var all []*row
	for rs.Next() {
		r := &row{}
		var date, sender, text, chat sql.NullString
		if err := rs.Scan(&r.id, &r.chatID, &r.ts, &date, &sender, &text, &chat); err != nil {
			rs.Close()
			return err
		}
		r.date, r.sender, r.text, r.chat = date.String, sender.String, text.String, chat.String
		if !chat.Valid {
			r.chat = "None"
		}
		all = append(all, r)
	}
	rs.Close()

	// Group rows per chat so context windows never cross conversations.
	var chatOrder []int64
	byChat := map[int64][]*row{}
	for _, r := range all {
		if _, ok := byChat[r.chatID]; !ok {
			chatOrder = append(chatOrder, r.chatID)
		}
		byChat[r.chatID] = append(byChat[r.chatID], r)
	}

	var hits []*hit
	var groups []*group
	for _, chatID := range chatOrder {
		rows := byChat[chatID]
		var chatHits []*hit
		for i, r := range rows {
			if r.text == "" {
				continue
			}
			var cats []string
			for _, name := range cfg.order {
				for _, re := range cfg.compiled[name] {
					if re.MatchString(r.text) {
						cats = append(cats, name)
						break
					}
				}
			}
			if len(cats) > 0 {
				h := &hit{i: i, row: r, categories: cats, start: max(0, i-opt.Context), end: min(len(rows), i+opt.Context+1)}
				chatHits = append(chatHits, h)
				hits = append(hits, h)
			}
		}
		// Merge overlapping context windows into one conversation.
		for _, h := range chatHits {
			if n := len(groups); n > 0 && groups[n-1].chatID == chatID && h.start <= groups[n-1].end {
				groups[n-1].end = max(groups[n-1].end, h.end)
				groups[n-1].hits = append(groups[n-1].hits, h)
			} else {
				groups = append(groups, &group{chatID: chatID, rows: rows, start: h.start, end: h.end, hits: []*hit{h}})
			}
		}
	}
	for _, g := range groups {
		g.categories = map[string]bool{}
		for _, h := range g.hits {
			for _, c := range h.categories {
				g.categories[c] = true
			}
		}
		g.score = cfg.Score(g.categories)
	}
	var selected []*group
	for _, g := range groups {
		if g.score >= opt.MinScore {
			selected = append(selected, g)
		}
	}
	tsOr0 := func(r *row) float64 {
		if r.ts.Valid {
			return r.ts.Float64
		}
		return 0
	}
	sort.SliceStable(selected, func(i, j int) bool {
		if selected[i].score != selected[j].score {
			return selected[i].score > selected[j].score
		}
		return tsOr0(selected[i].rows[selected[i].start]) < tsOr0(selected[j].rows[selected[j].start])
	})

	if err := os.MkdirAll(opt.Out, 0o755); err != nil {
		return err
	}
	var hitsCSV bytes.Buffer
	writeCSV(&hitsCSV, "message_id", "date", "chat", "sender", "categories", "text")
	for _, h := range hits {
		r := h.row
		writeCSV(&hitsCSV, strconv.FormatInt(r.id, 10), r.date, r.chat, r.sender, strings.Join(h.categories, " | "), r.text)
	}
	if err := os.WriteFile(filepath.Join(opt.Out, "hits.csv"), hitsCSV.Bytes(), 0o644); err != nil {
		return err
	}
	var convCSV bytes.Buffer
	writeCSV(&convCSV, "rank", "score", "chat", "start_date", "end_date", "hit_count", "categories",
		"senders", "first_message_id", "last_message_id")
	for n, g := range selected {
		first, last := g.rows[g.start], g.rows[g.end-1]
		var ss []string
		for _, s := range g.senders() {
			ss = append(ss, s[0]+": "+s[1])
		}
		writeCSV(&convCSV, strconv.Itoa(n+1), strconv.Itoa(g.score), first.chat, first.date, last.date, strconv.Itoa(len(g.hits)),
			strings.Join(g.sortedCategories(), " | "), strings.Join(ss, " | "), strconv.FormatInt(first.id, 10), strconv.FormatInt(last.id, 10))
	}
	if err := os.WriteFile(filepath.Join(opt.Out, "conversations.csv"), convCSV.Bytes(), 0o644); err != nil {
		return err
	}

	var md strings.Builder
	fmt.Fprintf(&md, "# %s\n\n", opt.Title)
	md.WriteString("Conversations identified by keyword categories, ranked by score. " +
		"This is a search index to guide reading, not a conclusion about what the messages mean.\n\n")
	for n, g := range selected {
		first, last := g.rows[g.start], g.rows[g.end-1]
		hitIdx := map[int]bool{}
		for _, h := range g.hits {
			hitIdx[h.i] = true
		}
		var ss []string
		for _, s := range g.senders() {
			ss = append(ss, s[0]+" ("+s[1]+")")
		}
		fmt.Fprintf(&md, "## %d. Score %d · %s · %s\n\n", n+1, g.score, pystr.Prefix(first.date, 10), first.chat)
		fmt.Fprintf(&md, "**Time range:** %s → %s  \n", first.date, last.date)
		fmt.Fprintf(&md, "**Categories:** %s  \n", strings.Join(g.sortedCategories(), " | "))
		fmt.Fprintf(&md, "**Keyword hits by sender:** %s  \n", strings.Join(ss, ", "))
		fmt.Fprintf(&md, "**Message IDs:** %d → %d\n\n", first.id, last.id)
		md.WriteString("### Matched messages\n\n```text\n")
		for _, h := range g.hits {
			r := h.row
			fmt.Fprintf(&md, "[%s] %s [%s] id=%d\n%s\n\n", r.date, r.sender, strings.Join(h.categories, " | "), r.id, r.text)
		}
		md.WriteString("```\n\n### Full conversation\n\n```text\n")
		for j := g.start; j < g.end; j++ {
			r := g.rows[j]
			mark := "    "
			if hitIdx[j] {
				mark = ">>> "
			}
			fmt.Fprintf(&md, "%s[%s] %s: %s\n", mark, r.date, r.sender, r.text)
		}
		md.WriteString("```\n\n---\n\n")
	}
	reportPath := filepath.Join(opt.Out, "review.md")
	if err := os.WriteFile(reportPath, []byte(md.String()), 0o644); err != nil {
		return err
	}

	counts := map[string]int{}
	for _, h := range hits {
		for _, c := range h.categories {
			counts[c]++
		}
	}
	fmt.Fprintf(out, "Messages scanned: %d\n", len(all))
	fmt.Fprintf(out, "Keyword hits:     %d\n", len(hits))
	fmt.Fprintf(out, "Conversations:    %d (%d with score >= %d)\n", len(groups), len(selected), opt.MinScore)
	fmt.Fprintf(out, "Wrote: %s, %s, %s\n\n", reportPath, filepath.Join(opt.Out, "hits.csv"), filepath.Join(opt.Out, "conversations.csv"))
	for _, name := range cfg.order {
		fmt.Fprintf(out, "  %-36s %d\n", name, counts[name])
	}
	fmt.Fprintln(out, "\nTop conversations:")
	for i, g := range selected {
		if i == 15 {
			break
		}
		fmt.Fprintf(out, "  score %3d | %s | %3d hits | %s\n", g.score, pystr.Prefix(g.rows[g.start].date, 10), len(g.hits), strings.Join(g.sortedCategories(), " | "))
	}
	return nil
}

// writeCSV writes one row the way Python's csv module does (quotes only when needed, CRLF).
func writeCSV(b *bytes.Buffer, fields ...string) {
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
