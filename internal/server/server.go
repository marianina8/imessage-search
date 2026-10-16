// Package server is the local web app: the setup wizard, the search page, AI
// summaries and report export. It listens on 127.0.0.1 only, so other devices on
// your network can't reach it.
package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marianina8/imessage-search/internal/ai"
	"github.com/marianina8/imessage-search/internal/db"
	"github.com/marianina8/imessage-search/internal/index"
	"github.com/marianina8/imessage-search/internal/macos"
	"github.com/marianina8/imessage-search/internal/pystr"
	"github.com/marianina8/imessage-search/internal/report"
	"github.com/marianina8/imessage-search/web"
)

// Server holds everything the handlers need.
type Server struct {
	Root      string   // the app folder (where .env and the launcher live)
	IndexPath string   // data/index.sqlite
	Demo      bool     // fictional data; the setup wizard is turned off
	Model     ai.Model // what writes summaries

	build   buildJob
	backup  macos.BackupJob
	exports exportStore
}

// New makes a server for the index at indexPath.
func New(root, indexPath string, demo bool) *Server {
	return &Server{Root: root, IndexPath: indexPath, Demo: demo, Model: ai.FromEnv()}
}

// Handler routes every request.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			s.get(w, r)
		case http.MethodPost:
			s.post(w, r)
		default:
			sendJSON(w, map[string]any{"error": "not found"}, http.StatusNotFound)
		}
	})
}

func sendJSON(w http.ResponseWriter, v any, status int) {
	data, err := json.Marshal(v)
	if err != nil {
		status, data = http.StatusInternalServerError, []byte(`{"error":"internal error"}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(data)
}

func sendPage(w http.ResponseWriter, name string) {
	data, err := web.Files.ReadFile(name)
	if err != nil {
		http.Error(w, "missing page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

func (s *Server) indexExists() bool {
	_, err := os.Stat(s.IndexPath)
	return err == nil
}

func (s *Server) openIndex() (*sql.DB, error) { return db.OpenReadOnly(s.IndexPath) }

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query()
	qs := map[string]string{}
	for k, v := range raw {
		qs[k] = v[len(v)-1]
	}
	path := r.URL.Path

	switch {
	case path == "/" || path == "/index.html":
		if !s.indexExists() {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}
		sendPage(w, "index.html")
		return
	case path == "/setup":
		if s.Demo {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		sendPage(w, "setup.html")
		return
	case strings.HasPrefix(path, "/report/"):
		s.serveReport(w, strings.TrimPrefix(path, "/report/"))
		return
	// Setup endpoints work before an index exists.
	case path == "/api/setup/status":
		sendJSON(w, s.setupStatus(), 200)
		return
	case path == "/api/setup/devices":
		sendJSON(w, map[string]any{"devices": macos.ConnectedDevices(), "backups": macos.ListBackups(), "backup_job": s.backup.Status()}, 200)
		return
	case path == "/api/setup/mac":
		sendJSON(w, macos.MacMessagesStats(), 200)
		return
	case path == "/api/setup/build":
		sendJSON(w, s.build.get(), 200)
		return
	}

	if !s.indexExists() {
		sendJSON(w, map[string]any{"error": "No index yet. Open /setup."}, http.StatusNotFound)
		return
	}
	conn, err := s.openIndex()
	if err != nil {
		sendJSON(w, map[string]any{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	switch path {
	case "/api/search":
		sendJSON(w, search(conn, qs, raw["sender"]), 200)
	case "/api/context":
		id, err1 := strconv.ParseInt(qs["id"], 10, 64)
		n := 10
		if v, ok := qs["n"]; ok {
			var err2 error
			if n, err2 = strconv.Atoi(v); err2 != nil {
				err1 = err2
			}
		}
		if err1 != nil {
			sendJSON(w, map[string]any{"error": "bad id"}, http.StatusBadRequest)
			return
		}
		rows, err := conversationAround(conn, id, n)
		if err != nil {
			sendJSON(w, map[string]any{"error": err.Error()}, http.StatusInternalServerError)
			return
		}
		sendJSON(w, rows, 200)
	case "/api/meta":
		status := s.setupStatus()
		sendJSON(w, map[string]any{
			"senders": senders(conn), "chats": chats(conn), "ai": s.Model.Ready(),
			"ai_name": s.Model.Name(), "ai_local": isLocal(s.Model), "ai_hint": s.Model.NotReadyHint(),
			"last_setup": status["last_setup"], "me_name": report.Meta(conn)["me_name"], "demo": s.Demo,
		}, 200)
	default:
		sendJSON(w, map[string]any{"error": "not found"}, http.StatusNotFound)
	}
}

// sameOrigin blocks other websites from driving this local API from your browser.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	return origin == "" || strings.HasSuffix(origin, "//"+r.Host)
}

func (s *Server) post(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		sendJSON(w, map[string]any{"error": "forbidden"}, http.StatusForbidden)
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		sendJSON(w, map[string]any{"error": err.Error()}, http.StatusBadRequest)
		return
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		data = []byte("{}")
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		sendJSON(w, map[string]any{"error": "bad JSON: " + err.Error()}, http.StatusBadRequest)
		return
	}
	switch r.URL.Path {
	case "/api/summarize", "/api/export":
		if !s.indexExists() {
			sendJSON(w, map[string]any{"error": "No index yet. Open /setup."}, http.StatusNotFound)
			return
		}
		conn, err := s.openIndex()
		if err != nil {
			sendJSON(w, map[string]any{"error": err.Error()}, http.StatusInternalServerError)
			return
		}
		defer conn.Close()
		if r.URL.Path == "/api/summarize" {
			sendJSON(w, s.summarize(r.Context(), conn, body), 200)
		} else {
			sendJSON(w, s.createExport(r.Context(), conn, body), 200)
		}
	case "/api/setup/open":
		sendJSON(w, map[string]any{"ok": macos.OpenTarget(str(body, "target"))}, 200)
	case "/api/setup/backup":
		sendJSON(w, s.backup.Start(str(body, "serial")), 200)
	case "/api/setup/build":
		sendJSON(w, s.build.start(s, body), 200)
	case "/api/setup/apikey":
		sendJSON(w, s.saveAPIKey(str(body, "key")), 200)
	case "/api/setup/relaunch":
		ok := macos.RelaunchTerminal(macos.FindLauncher(s.Root))
		if ok {
			dir := filepath.Dir(s.IndexPath)
			os.MkdirAll(dir, 0o755)
			os.WriteFile(filepath.Join(dir, ".resume-setup"), nil, 0o644)
		}
		sendJSON(w, map[string]any{"ok": ok}, 200)
		if ok { // exit so Terminal can quit without asking about a running process
			time.AfterFunc(500*time.Millisecond, func() { os.Exit(0) })
		}
	default:
		sendJSON(w, map[string]any{"error": "not found"}, http.StatusNotFound)
	}
}

// ---------------------------------------------------------------- JSON body helpers

func str(body map[string]any, k string) string {
	switch v := body[k].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func truthy(body map[string]any, k string) bool {
	switch v := body[k].(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

func ids(body map[string]any) ([]int64, error) {
	list, _ := body["ids"].([]any)
	out := make([]int64, 0, len(list))
	for _, v := range list {
		switch x := v.(type) {
		case float64:
			out = append(out, int64(x))
		case string:
			n, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
			if err != nil {
				return nil, err
			}
			out = append(out, n)
		default:
			return nil, fmt.Errorf("bad id %v", v)
		}
	}
	return out, nil
}

func intOr(body map[string]any, k string, def int) int {
	switch v := body[k].(type) {
	case float64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

// ---------------------------------------------------------------- summaries

const summarySystemPrompt = "You summarize excerpts from a person's own text-message history for them. " +
	"Be factual and neutral. Cite specific messages by their [date] and sender when you make a claim. " +
	"Say plainly when the excerpts don't support a conclusion, and don't speculate beyond them. " +
	"Format with short headings and bullet points in Markdown."

func (s *Server) summarize(ctx context.Context, conn *sql.DB, body map[string]any) map[string]any {
	if !s.Model.Ready() {
		return map[string]any{"error": s.Model.NotReadyHint()}
	}
	list, err := ids(body)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	if len(list) > 2000 {
		list = list[:2000]
	}
	if len(list) == 0 {
		return map[string]any{"error": "No messages to summarize."}
	}
	question := pystr.Strip(str(body, "question"))

	var rows []Message
	if truthy(body, "with_context") {
		seen := map[int64]bool{}
		for _, id := range list {
			ctxRows, err := conversationAround(conn, id, 3)
			if err != nil {
				return map[string]any{"error": err.Error()}
			}
			for _, r := range ctxRows {
				if !seen[r.ID] {
					seen[r.ID] = true
					rows = append(rows, r)
				}
			}
		}
	} else {
		args := make([]any, len(list))
		for i, id := range list {
			args[i] = id
		}
		q := "SELECT id, date, sender, text FROM messages WHERE id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(list)), ",") + ")"
		res, err := conn.Query(q, args...)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		for res.Next() {
			var m Message
			var date, sender, text sql.NullString
			if res.Scan(&m.ID, &date, &sender, &text) == nil {
				m.Date, m.Sender, m.Text = date.String, sender.String, text.String
				rows = append(rows, m)
			}
		}
		res.Close()
	}
	sortByDate(rows)

	maxChars := s.Model.MaxInputChars()
	var lines []string
	used, truncated := 0, false
	for _, r := range rows {
		line := fmt.Sprintf("[%s] %s: %s", strings.Replace(pystr.Prefix(r.Date, 16), "T", " ", 1), r.Sender, r.Text)
		n := pystr.Len(line)
		if used+n > maxChars {
			truncated = true
			break
		}
		lines = append(lines, line)
		used += n + 1
	}
	task := question
	if task == "" {
		task = "Summarize the main topics, key facts, decisions, and any notable changes over time."
	}
	filters, _ := json.Marshal(body["filters"])
	if body["filters"] == nil {
		filters = []byte("{}")
	}
	user := "Search filters used: " + string(filters) + "\n\n<messages>\n" + strings.Join(lines, "\n") + "\n</messages>\n\nTask: " + task
	text, model, err := s.Model.Complete(ctx, summarySystemPrompt, user, 2500)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	return map[string]any{"summary": text, "messages_sent": len(lines), "truncated": truncated, "model": model}
}

func sortByDate(rows []Message) {
	// Stable, like Python's sort, so equal dates keep their order.
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].Date < rows[j-1].Date; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

// ---------------------------------------------------------------- export

type export struct {
	html, name string
	csv        []byte
}

// exportStore keeps the 20 most recent reports in memory.
type exportStore struct {
	mu    sync.Mutex
	order []string
	items map[string]export
}

func (e *exportStore) put(token string, x export) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.items == nil {
		e.items = map[string]export{}
	}
	e.items[token] = x
	e.order = append(e.order, token)
	for len(e.order) > 20 {
		delete(e.items, e.order[0])
		e.order = e.order[1:]
	}
}

func (e *exportStore) get(token string) (export, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	x, ok := e.items[token]
	return x, ok
}

func newToken() string {
	b := make([]byte, 12)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

var reNotFileName = regexp.MustCompile(`[^\p{L}\p{N}_\- ]+`)

func (s *Server) createExport(ctx context.Context, conn *sql.DB, body map[string]any) map[string]any {
	list, err := ids(body)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	if len(list) == 0 {
		return map[string]any{"error": "Search first, then export the results."}
	}
	contextN := intOr(body, "context", 5)
	contextN = max(0, min(contextN, 25))
	rows, err := report.Collect(conn, list, contextN)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	if len(rows) == 0 {
		return map[string]any{"error": "Nothing to export."}
	}
	title := pystr.Prefix(pystr.Strip(str(body, "title")), 150)
	if str(body, "title") == "" {
		title = "Text messages"
	}
	filters, _ := body["filters"].(map[string]any)
	if filters == nil {
		filters = map[string]any{}
	}
	chatLabels := map[int64]string{}
	for _, c := range chats(conn) {
		chatLabels[c.ChatID] = c.Label
	}
	info := report.Info{
		Title:      title,
		PreparedBy: pystr.Prefix(pystr.Strip(str(body, "prepared_by")), 100),
		ContextN:   contextN,
		Filters:    filters,
		Chats:      chatLabels,
		Meta:       report.Meta(conn),
		Question:   pystr.Strip(str(body, "question")),
	}
	if truthy(body, "include_summary") {
		transcript, truncated := report.SummaryInput(rows, s.Model.MaxInputChars())
		focus := ""
		if info.Question != "" {
			focus = "\n\nFocus especially on: " + info.Question
		}
		text, model, err := s.Model.Complete(ctx, report.SystemPrompt, "<messages>\n"+transcript+"\n</messages>"+focus, 4000)
		if err != nil {
			return map[string]any{"error": "The report wasn't created because the AI summary failed: " + err.Error()}
		}
		info.Summary, info.Model, info.SummaryTruncated = text, model, truncated
		info.SummaryCount = strings.Count(transcript, "\n") + 1
	}
	token := newToken()
	name := strings.TrimSpace(reNotFileName.ReplaceAllString(info.Title, ""))
	if name == "" {
		name = "messages"
	}
	s.exports.put(token, export{html: report.HTML(rows, info, token, time.Now()), csv: report.CSV(rows), name: name})
	return map[string]any{"token": token, "count": len(rows)}
}

func (s *Server) serveReport(w http.ResponseWriter, token string) {
	asCSV := strings.HasSuffix(token, ".csv")
	x, ok := s.exports.get(strings.TrimSuffix(token, ".csv"))
	if !ok {
		sendJSON(w, map[string]any{"error": "This report has expired. Export again from the search page."}, http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if asCSV {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+x.name+`.csv"`)
		w.Write(x.csv)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(x.html))
}

// ---------------------------------------------------------------- setup

func (s *Server) lastSetup() map[string]any {
	info := map[string]any{}
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(s.IndexPath), "setup.json")); err == nil {
		json.Unmarshal(b, &info)
	}
	return info
}

func (s *Server) setupStatus() map[string]any {
	info := s.lastSetup()
	me, _ := info["me"].(string)
	if me == "" {
		me = macos.FullName()
	}
	var fda any
	if v := macos.FullDiskAccess(); v != nil {
		fda = *v
	}
	return map[string]any{
		"platform_mac": macos.IsMac,
		"fda":          fda,
		"host_app":     macos.HostApp(),
		"can_relaunch": macos.IsMac && macos.HostApp() == "Terminal",
		"index_ready":  s.indexExists(),
		"last_setup":   info,
		"me_default":   me,
		"ai":           s.Model.Ready(),
		"backup_tool":  s.backup.Available(),
	}
}

func (s *Server) saveAPIKey(key string) map[string]any {
	key = pystr.Strip(key)
	if !strings.HasPrefix(key, "sk-") {
		return map[string]any{"error": "That doesn't look like an Anthropic API key (it should start with sk-ant-)."}
	}
	env := filepath.Join(s.Root, ".env")
	var lines []string
	if b, err := os.ReadFile(env); err == nil {
		for _, l := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
			if !strings.HasPrefix(l, "ANTHROPIC_API_KEY=") {
				lines = append(lines, l)
			}
		}
		for len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
	}
	lines = append([]string{"ANTHROPIC_API_KEY=" + key}, lines...)
	if err := os.WriteFile(env, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return map[string]any{"error": err.Error()}
	}
	os.Chmod(env, 0o600)
	os.Setenv("ANTHROPIC_API_KEY", key)
	return map[string]any{"ok": true}
}

// buildJob runs the wizard's "get my messages" step in the background.
type buildJob struct {
	mu      sync.Mutex
	state   map[string]any
	running bool
}

func (j *buildJob) get() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.state == nil {
		return map[string]any{"state": "idle"}
	}
	out := make(map[string]any, len(j.state))
	for k, v := range j.state {
		out[k] = v
	}
	return out
}

func (j *buildJob) set(kv ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for i := 0; i+1 < len(kv); i += 2 {
		j.state[kv[i].(string)] = kv[i+1]
	}
}

func (j *buildJob) start(s *Server, body map[string]any) map[string]any {
	j.mu.Lock()
	if j.running {
		j.mu.Unlock()
		return map[string]any{"error": "Already building."}
	}
	j.running = true
	j.state = map[string]any{"state": "running", "stage": "Getting your messages…", "done": 0, "total": 0}
	j.mu.Unlock()
	go func() {
		defer func() {
			j.mu.Lock()
			j.running = false
			j.mu.Unlock()
		}()
		j.run(s, body)
	}()
	return map[string]any{"ok": true}
}

func (j *buildJob) run(s *Server, body map[string]any) {
	dataDir := filepath.Dir(s.IndexPath)
	now := time.Now().Format("January 2, 2006 at 3:04 PM MST")
	var got macos.Copied
	var desc string
	var err error
	if str(body, "source") == "backup" {
		j.set("stage", "Unlocking and copying messages from the iPhone backup…")
		id := str(body, "backup_id")
		got, err = macos.ExtractFromBackup(id, dataDir, str(body, "password"))
		if err == nil {
			info := macos.Backup{Device: "iPhone"}
			for _, b := range macos.ListBackups() {
				if b.ID == id {
					info = b
				}
			}
			made := "unknown date"
			if t, perr := pystr.ParseISO(info.Date); info.Date != "" && perr == nil {
				made = t.Format("January 2, 2006 at 3:04 PM")
			}
			enc := ""
			if info.Encrypted {
				enc = " (encrypted)"
			}
			desc = fmt.Sprintf("Local (Finder) backup of the iPhone \"%s\" made %s%s. Messages database extracted %s.", info.Device, made, enc, now)
		}
	} else {
		j.set("stage", "Copying messages from the Messages app…")
		got, err = macos.CopyFromMac(dataDir)
		desc = "Messages app database on this Mac (~/Library/Messages/chat.db), copied " + now + "."
	}
	if err != nil {
		var wp *macos.WrongPasswordError
		if errors.As(err, &wp) {
			j.set("state", "failed", "error", wp.Msg, "needs_password", true)
		} else {
			j.set("state", "failed", "error", err.Error())
		}
		return
	}
	j.set("stage", "Reading and indexing messages…")
	me := pystr.Strip(str(body, "me"))
	if me == "" {
		me = "Me"
	}
	contacts := index.LoadMacContacts(index.MacContactsGlob(), os.Stderr)
	if got.Contacts != "" {
		for k, v := range index.LoadIOSContacts(got.Contacts, os.Stderr) {
			contacts[k] = v
		}
	}
	res, err := index.Build(got.DB, s.IndexPath, index.Options{
		Me: me, Contacts: contacts, SourceDesc: desc, Log: os.Stdout,
		Progress: func(done, total int) { j.set("done", done, "total", total) },
	})
	if err != nil {
		j.set("state", "failed", "error", err.Error())
		return
	}
	setup, _ := json.Marshal(map[string]any{
		"source": body["source"], "backup_id": body["backup_id"], "me": body["me"],
		"built": pystr.ISOFormat(time.Now()),
	})
	os.WriteFile(filepath.Join(dataDir, "setup.json"), setup, 0o644)
	j.set("state", "done", "stage", "Done", "result", res)
}

// ---------------------------------------------------------------- running

// PortInUse reports whether something already listens on 127.0.0.1:port.
func PortInUse(port int) bool {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// LoadDotEnv reads KEY=value lines from root/.env without overriding real environment variables.
func LoadDotEnv(root string) {
	b, err := os.ReadFile(filepath.Join(root, ".env"))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.Trim(strings.TrimSpace(v), `"`), "'")
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
}

// isLocal reports whether summaries run on this Mac (Ollama) rather than in the cloud.
func isLocal(m ai.Model) bool {
	_, ok := m.(*ai.Ollama)
	return ok
}
