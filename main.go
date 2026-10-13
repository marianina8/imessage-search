// iMessage Search: search years of iPhone text messages on your Mac by who sent
// them and what they say, get AI summaries that cite every message, and export a
// report. Everything runs on this Mac.
//
//	imessage-search serve --open         start the app (the setup wizard opens if there's no index yet)
//	imessage-search demo --open          try it with fictional messages
//	imessage-search index --me Alex      build data/index.sqlite from data/chat.db
//	imessage-search categorize --config categories.local.json
//	imessage-search compare              check the Go indexer against the Python one, message by message
package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/marianina8/imessage-search/internal/categorize"
	"github.com/marianina8/imessage-search/internal/db"
	"github.com/marianina8/imessage-search/internal/demo"
	"github.com/marianina8/imessage-search/internal/index"
	"github.com/marianina8/imessage-search/internal/server"
)

func main() {
	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serveCmd(args)
	case "demo":
		err = demoCmd(args)
	case "index":
		err = indexCmd(args)
	case "categorize":
		err = categorizeCmd(args)
	case "compare":
		err = compareCmd(args)
	case "compare-backup":
		err = compareBackupCmd(args)
	case "help", "-h", "--help":
		fmt.Println(usage)
	default:
		err = fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

const usage = `Usage: imessage-search [command] [flags]

  serve       start the app in your browser (default)
  demo        start it with fictional demo messages
  index       build the search index from a Messages database
  categorize  keyword-category review report
  compare     check this indexer against the Python one
  compare-backup  check the backup reader against the Python one

Run "imessage-search <command> -h" for a command's flags.`

// appRoot is the app's folder: the parent of bin/ when run from bin/, else the current folder.
func appRoot() string {
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			dir := filepath.Dir(exe)
			if filepath.Base(dir) == "bin" {
				parent := filepath.Dir(dir)
				if _, err := os.Stat(filepath.Join(parent, "go.mod")); err == nil {
					return parent
				}
			}
		}
	}
	wd, _ := os.Getwd()
	return wd
}

func serveCmd(args []string) error {
	root := appRoot()
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	idx := fs.String("index", filepath.Join(root, "data", "index.sqlite"), "search index to serve")
	port := fs.Int("port", 8765, "port on 127.0.0.1")
	open := fs.Bool("open", false, "open the browser once the server is up")
	fs.Parse(args)
	return serve(root, *idx, *port, *open, false)
}

func demoCmd(args []string) error {
	root := appRoot()
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	open := fs.Bool("open", false, "open the browser once the server is up")
	fs.Parse(args)
	idx, err := demo.Build(filepath.Join(root, "data", "demo"))
	if err != nil {
		return err
	}
	return serve(root, idx, demo.Port, *open, true)
}

func serve(root, idx string, port int, open, isDemo bool) error {
	server.LoadDotEnv(root)
	idx, _ = filepath.Abs(idx)
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	if server.PortInUse(port) {
		fmt.Printf("iMessage Search is already running. Opening %s\n", url)
		if open {
			openBrowser(url)
		}
		return nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	s := server.New(root, idx, isDemo)
	label := ""
	if isDemo {
		label = " DEMO (fictional messages)"
	}
	line := strings.Repeat("=", 60)
	fmt.Printf("%s\n  iMessage Search%s is running at %s\n  Keep this window open while you use it.\n  To stop: close this window, or press Ctrl+C.\n%s\n", line, label, url, line)
	_, statErr := os.Stat(idx)
	if statErr != nil {
		fmt.Println("No search index yet, so the setup wizard will open.")
	}
	target := url
	resume := filepath.Join(filepath.Dir(idx), ".resume-setup")
	if _, err := os.Stat(resume); err == nil { // coming back from the Full Disk Access restart
		os.Remove(resume)
		target = url + "setup#resume"
	} else if statErr != nil {
		target = url + "setup"
	}
	if open {
		time.AfterFunc(800*time.Millisecond, func() { openBrowser(target) })
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	return srv.Serve(ln)
}

func openBrowser(url string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("open", url).Start()
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	default:
		exec.Command("xdg-open", url).Start()
	}
}

type intList []int64

func (l *intList) String() string { return fmt.Sprint(*l) }
func (l *intList) Set(s string) error {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*l = append(*l, n)
	return nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// loadContacts reads this Mac's Contacts, plus iPhone contacts saved next to the database.
func loadContacts(dbPath string, use bool) map[string]string {
	if !use {
		return map[string]string{}
	}
	contacts := index.LoadMacContacts(index.MacContactsGlob(), os.Stderr)
	ios := filepath.Join(filepath.Dir(dbPath), "contacts.sqlitedb")
	if _, err := os.Stat(ios); err == nil {
		for k, v := range index.LoadIOSContacts(ios, os.Stderr) {
			contacts[k] = v
		}
	}
	return contacts
}

func indexCmd(args []string) error {
	root := appRoot()
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	dbPath := fs.String("db", filepath.Join(root, "data", "chat.db"), "path to a COPY of chat.db (or an iPhone sms.db)")
	out := fs.String("out", filepath.Join(root, "data", "index.sqlite"), "index file to write")
	var chats intList
	fs.Var(&chats, "chat", "only index this chat_id (repeatable)")
	me := fs.String("me", "Me", "display name for your own messages")
	noContacts := fs.Bool("no-contacts", false, "don't look up names in Contacts")
	list := fs.Bool("list-chats", false, "list chats and exit")
	fs.Parse(args)
	path := expandHome(*dbPath)
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("chat.db not found at %s. See README: 'Copy the database'", path)
	}
	contacts := loadContacts(path, !*noContacts)
	if *list {
		src, err := db.OpenReadOnly(path)
		if err != nil {
			return err
		}
		defer src.Close()
		all, err := index.ListChats(src, contacts)
		if err != nil {
			return err
		}
		fmt.Printf("%7s  %8s  label\n", "chat_id", "messages")
		for _, c := range all {
			fmt.Printf("%7d  %8d  %s\n", c.ID, c.MessageCount, c.Label)
		}
		return nil
	}
	_, err := index.Build(path, expandHome(*out), index.Options{ChatIDs: chats, Me: *me, Contacts: contacts, Log: os.Stdout})
	return err
}

func categorizeCmd(args []string) error {
	root := appRoot()
	fs := flag.NewFlagSet("categorize", flag.ExitOnError)
	config := fs.String("config", filepath.Join(root, "categories.example.json"), "categories config (JSON)")
	idx := fs.String("index", filepath.Join(root, "data", "index.sqlite"), "search index")
	out := fs.String("out", filepath.Join(root, "output"), "folder for the report")
	var chats intList
	fs.Var(&chats, "chat", "limit to chat_id (repeatable)")
	ctx := fs.Int("context", 15, "messages of context before/after each hit")
	minScore := fs.Int("min-score", 0, "only include conversations scoring at least this")
	title := fs.String("title", "Message Review", "report title")
	fs.Parse(args)
	cfg, err := categorize.LoadConfig(*config)
	if err != nil {
		return err
	}
	return categorize.Run(cfg, categorize.Options{Index: *idx, Out: *out, ChatIDs: chats, Context: *ctx,
		MinScore: *minScore, Title: *title, Stdout: os.Stdout})
}

var errDifferent = errors.New("the two indexes differ")
