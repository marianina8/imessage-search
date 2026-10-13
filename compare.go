package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"time"

	"encoding/json"
	"flag"
	"fmt"
	"github.com/marianina8/imessage-search/internal/macos"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/marianina8/imessage-search/internal/db"
	"github.com/marianina8/imessage-search/internal/index"
)

// compareCmd builds the index twice from the same database, once with this Go code
// and once with the original Python code, and compares them message by message.
// It prints only counts and message ID numbers unless you pass -show, so the output
// is safe to share; -show prints the differing text for your eyes only.
func compareCmd(args []string) error {
	root := appRoot()
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	dbPath := fs.String("db", filepath.Join(root, "data", "chat.db"), "the Messages database copy to index")
	python := fs.String("python", "", "Python with the old app's packages (default: .venv/bin/python, else python3)")
	me := fs.String("me", "", "your name, as in the index (default: from data/setup.json, else Me)")
	show := fs.Bool("show", false, "also print the differing message text (stays on this Mac)")
	keep := fs.Bool("keep", false, "keep both index files for a closer look")
	fs.Parse(args)

	if *python == "" {
		*python = filepath.Join(root, ".venv", "bin", "python")
		if _, err := os.Stat(*python); err != nil {
			*python = "python3"
		}
	}
	if *me == "" {
		*me = "Me"
		if b, err := os.ReadFile(filepath.Join(root, "data", "setup.json")); err == nil {
			var s struct{ Me string }
			if json.Unmarshal(b, &s) == nil && strings.TrimSpace(s.Me) != "" {
				*me = strings.TrimSpace(s.Me)
			}
		}
	}
	tmp, err := os.MkdirTemp("", "imsg-compare-*")
	if err != nil {
		return err
	}
	if !*keep {
		defer os.RemoveAll(tmp)
	}
	goIdx, pyIdx := filepath.Join(tmp, "go-index.sqlite"), filepath.Join(tmp, "py-index.sqlite")

	fmt.Println("1/3 Building the index with Go…")
	if _, err := index.Build(*dbPath, goIdx, index.Options{Me: *me, Contacts: loadContacts(*dbPath, true)}); err != nil {
		return fmt.Errorf("Go indexer: %w", err)
	}
	fmt.Println("2/3 Building the index with Python…")
	py := exec.Command(*python, "-m", "imsg.build_index", "--db", *dbPath, "--out", pyIdx, "--me", *me)
	py.Dir = root
	if out, err := py.CombinedOutput(); err != nil {
		return fmt.Errorf("Python indexer: %v\n%s", err, out)
	} else if strings.Contains(string(out), "pytypedstream not installed") {
		fmt.Println("   note: Python is using its fallback decoder (pytypedstream isn't installed in that Python).")
	}
	fmt.Println("3/3 Comparing…")

	cols := []string{"guid", "chat_id", "ts", "date", "is_from_me", "handle", "sender", "service", "text",
		"text_source", "has_attachment", "is_reaction", "attachments"}
	a, err := readIndex(pyIdx, "messages", "id", cols)
	if err != nil {
		return err
	}
	b, err := readIndex(goIdx, "messages", "id", cols)
	if err != nil {
		return err
	}
	diffCols := map[string]int{}
	var diffIDs []int64
	same, onlyPy, onlyGo := 0, 0, 0
	for id, ra := range a {
		rb, ok := b[id]
		if !ok {
			onlyPy++
			diffIDs = append(diffIDs, id)
			continue
		}
		differs := false
		for i, c := range cols {
			if !equalValue(c, ra[i], rb[i]) {
				diffCols[c]++
				differs = true
			}
		}
		if differs {
			diffIDs = append(diffIDs, id)
		} else {
			same++
		}
	}
	for id := range b {
		if _, ok := a[id]; !ok {
			onlyGo++
			diffIDs = append(diffIDs, id)
		}
	}
	chatCols := []string{"label", "identifier", "message_count"}
	ca, err := readIndex(pyIdx, "chats", "chat_id", chatCols)
	if err != nil {
		return err
	}
	cb, err := readIndex(goIdx, "chats", "chat_id", chatCols)
	if err != nil {
		return err
	}
	chatDiff := 0
	for id, ra := range ca {
		rb, ok := cb[id]
		if !ok {
			chatDiff++
			continue
		}
		for i, c := range chatCols {
			if !equalValue(c, ra[i], rb[i]) {
				chatDiff++
				break
			}
		}
	}
	chatDiff += max(0, len(cb)-len(ca))

	fmt.Printf("\nMessages: %d (Python) vs %d (Go)\n", len(a), len(b))
	fmt.Printf("Identical: %d of %d (%.4f%%)\n", same, len(a), 100*float64(same)/math.Max(1, float64(len(a))))
	if onlyPy+onlyGo > 0 {
		fmt.Printf("Only in Python: %d, only in Go: %d\n", onlyPy, onlyGo)
	}
	if len(diffCols) > 0 {
		fmt.Println("Differences by column:")
		var names []string
		for c := range diffCols {
			names = append(names, c)
		}
		sort.Strings(names)
		for _, c := range names {
			fmt.Printf("  %-15s %d\n", c, diffCols[c])
		}
	}
	fmt.Printf("Conversations: %d (Python) vs %d (Go), %d different\n", len(ca), len(cb), chatDiff)
	sort.Slice(diffIDs, func(i, j int) bool { return diffIDs[i] < diffIDs[j] })
	if len(diffIDs) > 0 {
		first := diffIDs
		if len(first) > 20 {
			first = first[:20]
		}
		fmt.Printf("First differing message IDs (ROWIDs only, no content): %v\n", first)
		if *show {
			fmt.Println("\nDiffering values (this text stays in this window):")
			for _, id := range first {
				for i, c := range cols {
					var va, vb any
					if r, ok := a[id]; ok {
						va = r[i]
					}
					if r, ok := b[id]; ok {
						vb = r[i]
					}
					if !equalValue(c, va, vb) {
						fmt.Printf("  id %d %s\n    python: %q\n    go:     %q\n", id, c, fmt.Sprint(va), fmt.Sprint(vb))
					}
				}
			}
		}
	}
	if *keep {
		fmt.Printf("\nKept: %s\n      %s\n", pyIdx, goIdx)
	}
	if len(diffIDs) == 0 && chatDiff == 0 {
		fmt.Println("\n✅ The Go index matches the Python index exactly.")
		return nil
	}
	fmt.Println("\n❗ Not identical yet. Share the counts and IDs above (they contain no message text).")
	return errDifferent
}

func readIndex(path, table, key string, cols []string) (map[int64][]any, error) {
	conn, err := db.OpenReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rows, err := conn.Query(fmt.Sprintf("SELECT %s, %s FROM %s", key, strings.Join(cols, ", "), table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]any{}
	for rows.Next() {
		var id int64
		vals := make([]any, len(cols))
		ptrs := []any{&id}
		for i := range vals {
			ptrs = append(ptrs, &vals[i])
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, v := range vals {
			if bs, ok := v.([]byte); ok {
				vals[i] = string(bs)
			}
		}
		out[id] = vals
	}
	return out, rows.Err()
}

func equalValue(col string, a, b any) bool {
	if col == "ts" {
		fa, oka := toFloat(a)
		fb, okb := toFloat(b)
		if oka != okb {
			return false
		}
		return !oka || math.Abs(fa-fb) < 2e-6
	}
	return fmt.Sprint(a) == fmt.Sprint(b) && (a == nil) == (b == nil)
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// compareBackupCmd checks the Go backup reader: it extracts the Messages database
// from the same iPhone backup the Python version used (data/setup.json) and checks
// the result is byte-for-byte the same as data/chat.db. The password comes from
// IMSG_BACKUP_PASSWORD (the double-click script asks for it without showing it).
func compareBackupCmd(args []string) error {
	root := appRoot()
	fs := flag.NewFlagSet("compare-backup", flag.ExitOnError)
	fs.Parse(args)
	var setup struct {
		Source   string `json:"source"`
		BackupID string `json:"backup_id"`
		Built    string `json:"built"`
	}
	b, err := os.ReadFile(filepath.Join(root, "data", "setup.json"))
	if err != nil {
		return fmt.Errorf("no data/setup.json: set up the app once first (%v)", err)
	}
	json.Unmarshal(b, &setup)
	if setup.Source != "backup" || setup.BackupID == "" {
		fmt.Println("Your messages came from the Messages app on this Mac, not an iPhone backup, so there's no backup to check.")
		return nil
	}
	var info macos.Backup
	for _, bk := range macos.ListBackups() {
		if bk.ID == setup.BackupID {
			info = bk
		}
	}
	if info.ID == "" {
		return fmt.Errorf("the backup used on %s isn't on this Mac any more", setup.Built)
	}
	fmt.Printf("Backup: %s, made %s, %s\n", info.Device, info.Date, map[bool]string{true: "encrypted", false: "not encrypted"}[info.Encrypted])
	fmt.Printf("data/chat.db was extracted by the Python version on %s\n", setup.Built)

	tmp, err := os.MkdirTemp("", "imsg-backup-check-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	start := time.Now()
	got, err := macos.ExtractFromBackup(setup.BackupID, tmp, os.Getenv("IMSG_BACKUP_PASSWORD"))
	if err != nil {
		return err
	}
	fmt.Printf("Go extracted the Messages database in %.1fs\n", time.Since(start).Seconds())
	ha, sa, err := fileHash(filepath.Join(root, "data", "chat.db"))
	if err != nil {
		return err
	}
	hb, sb, err := fileHash(got.DB)
	if err != nil {
		return err
	}
	fmt.Printf("Python's copy: %d bytes, SHA-256 %s…\nGo's copy:     %d bytes, SHA-256 %s…\n", sa, ha[:16], sb, hb[:16])
	if ha == hb {
		fmt.Println("\n✅ Byte-for-byte identical. The Go backup reader matches the Python one.")
		return nil
	}
	if t, err := time.Parse(time.RFC3339Nano, setup.Built); err == nil && info.TS > float64(t.Unix()) {
		fmt.Println("\nThey differ, but the backup is newer than data/chat.db, so that's expected.")
		fmt.Println("To compare properly, run the setup again with the Python version, then run this check.")
		return nil
	}
	fmt.Println("\n❗ They differ. Share the sizes above (no message content).")
	return errDifferent
}

func fileHash(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}
