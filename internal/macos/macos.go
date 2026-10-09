// Package macos holds the Mac-specific parts of the setup wizard: permissions,
// finding a plugged-in iPhone, local backups, and getting a copy of the Messages
// database into ./data/.
//
// Two sources are supported:
//   - "backup": a local (Finder) backup of an iPhone connected by cable. Local
//     backups always contain the full Messages database, even when Messages in
//     iCloud is on. Encrypted backups need the backup password.
//   - "mac": the Messages app on this Mac (~/Library/Messages/chat.db), which
//     has whatever Messages in iCloud has synced.
package macos

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/marianina8/imessage-search/internal/backup"
	"github.com/marianina8/imessage-search/internal/db"
	"github.com/marianina8/imessage-search/internal/index"
	"github.com/marianina8/imessage-search/internal/plist"
	"github.com/marianina8/imessage-search/internal/pystr"
)

// IsMac is true on macOS.
var IsMac = runtime.GOOS == "darwin"

var home, _ = os.UserHomeDir()

// Where things live on a Mac.
var (
	MessagesDB = filepath.Join(home, "Library/Messages/chat.db")
	BackupRoot = filepath.Join(home, "Library/Application Support/MobileSync/Backup")
)

// Files inside an unencrypted backup are named sha1("<domain>-<relative path>").
const (
	smsDBHash       = "3d0d7e5fb2ce288813306e4d4636395e047a3d28" // HomeDomain-Library/SMS/sms.db
	addressBookHash = "31bb7ba8914766d4ba40d6dfb6113c8b614be442" // HomeDomain-Library/AddressBook/AddressBook.sqlitedb
)

const appleMobileBackup = "/System/Library/PrivateFrameworks/MobileDevice.framework/Versions/Current/" +
	"AppleMobileDeviceHelper.app/Contents/Resources/AppleMobileBackup"

// ---------------------------------------------------------------- permissions

// FullDiskAccess reports whether this process can read protected folders:
// true or false when it can tell, nil when it can't.
func FullDiskAccess() *bool {
	probes := []string{
		filepath.Join(home, "Library/Application Support/com.apple.TCC"),
		filepath.Join(home, "Library/Messages"),
		BackupRoot,
		filepath.Join(home, "Library/Safari"),
	}
	yes, no := true, false
	for _, p := range probes {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		_, err := os.ReadDir(p)
		switch {
		case err == nil:
			return &yes
		case errors.Is(err, fs.ErrPermission):
			return &no
		}
	}
	return nil
}

// HostApp is the app that needs Full Disk Access: the terminal running us.
func HostApp() string {
	switch os.Getenv("TERM_PROGRAM") {
	case "iTerm.app":
		return "iTerm"
	case "vscode":
		return "Visual Studio Code"
	case "WarpTerminal":
		return "Warp"
	case "ghostty":
		return "Ghostty"
	}
	return "Terminal"
}

var openTargets = map[string][]string{
	"fda":      {"open", "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles"},
	"messages": {"open", "-a", "Messages"},
	"finder":   {"open", "-a", "Finder"},
}

// OpenTarget opens System Settings, Messages or Finder.
func OpenTarget(name string) bool {
	cmd, ok := openTargets[name]
	if !ok || !IsMac {
		return false
	}
	return exec.Command(cmd[0], cmd[1:]...).Start() == nil
}

// RelaunchTerminal quits Terminal and reopens the launcher so a new Full Disk Access
// grant takes effect. It runs in a detached helper so it survives Terminal quitting;
// the caller should exit right after, so Terminal has no running process to ask about.
func RelaunchTerminal(launcher string) bool {
	if !IsMac || HostApp() != "Terminal" {
		return false
	}
	if _, err := os.Stat(launcher); err != nil {
		return false
	}
	script := "sleep 2; osascript -e 'tell application \"Terminal\" to quit'; sleep 2; open " + shellQuote(launcher)
	cmd := exec.Command("/bin/sh", "-c", script)
	detach(cmd)
	return cmd.Start() == nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// FullName is the user's first name, for "Me" in the index.
func FullName() string {
	if IsMac {
		if out, err := exec.Command("id", "-F").Output(); err == nil {
			if f := strings.Fields(string(out)); len(f) > 0 {
				return f[0]
			}
		}
	}
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if name == "" {
		return ""
	}
	r := []rune(strings.ToLower(name))
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// FindLauncher is the double-click file that starts the app.
func FindLauncher(root string) string { return filepath.Join(root, "Start iMessage Search.command") }

// ---------------------------------------------------------------- devices

// Device is an iPhone, iPad or iPod plugged in by USB.
type Device struct {
	Name   string `json:"name"`
	Serial string `json:"serial"`
}

// ConnectedDevices lists plugged-in iPhones (seen even before "Trust" is tapped).
func ConnectedDevices() []Device {
	found := []Device{}
	if !IsMac {
		return found
	}
	for _, dataType := range []string{"SPUSBHostDataType", "SPUSBDataType"} {
		cmd := exec.Command("system_profiler", dataType, "-json")
		out, err := outputWithTimeout(cmd, 20*time.Second)
		if err != nil {
			continue
		}
		root, err := parseOrdered(out)
		if err != nil {
			continue
		}
		walkDevices(root, &found)
		if len(found) > 0 {
			break
		}
	}
	seen := map[Device]bool{}
	unique := []Device{}
	for _, d := range found {
		if !seen[d] {
			seen[d] = true
			unique = append(unique, d)
		}
	}
	return unique
}

func walkDevices(n *node, found *[]Device) {
	switch n.kind {
	case 'o':
		name := ""
		if v := n.get("_name"); v != nil {
			name = v.text()
		}
		if strings.Contains(name, "iPhone") || strings.Contains(name, "iPad") || strings.Contains(name, "iPod") {
			serial := ""
			for _, k := range []string{"serial_num", "USB Serial Number"} {
				if v := n.get(k); v != nil && v.truthy() {
					serial = v.text()
					break
				}
			}
			*found = append(*found, Device{Name: name, Serial: serial})
		}
		for _, v := range n.vals {
			walkDevices(v, found)
		}
	case 'a':
		for _, v := range n.vals {
			walkDevices(v, found)
		}
	}
}

func udidFromSerial(serial string) string {
	s := strings.ReplaceAll(serial, "-", "")
	if len(s) == 24 {
		return s[:8] + "-" + s[8:]
	}
	return s
}

// ---------------------------------------------------------------- backups

// Backup describes one local backup in ~/Library/Application Support/MobileSync/Backup.
type Backup struct {
	ID        string  `json:"id"`
	Device    string  `json:"device"`
	Product   string  `json:"product"`
	UDID      string  `json:"udid"`
	Date      string  `json:"date"`
	TS        float64 `json:"ts"`
	Encrypted bool    `json:"encrypted"`
	Complete  bool    `json:"complete"`
	SizeGB    *int    `json:"size_gb"`
}

// ListBackups returns local backups, newest first.
func ListBackups() []Backup {
	backups := []Backup{}
	entries, err := os.ReadDir(BackupRoot)
	if err != nil {
		return backups
	}
	for _, e := range entries {
		d := filepath.Join(BackupRoot, e.Name())
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(d, "Manifest.db")); err != nil {
			continue
		}
		info := plist.ReadDict(filepath.Join(d, "Info.plist"))
		manifest := plist.ReadDict(filepath.Join(d, "Manifest.plist"))
		status := plist.ReadDict(filepath.Join(d, "Status.plist"))
		b := Backup{
			ID:        e.Name(),
			Device:    firstString(info["Device Name"], info["Display Name"], "iPhone"),
			Product:   firstString(info["Product Type"], ""),
			UDID:      firstString(info["Unique Identifier"], e.Name()),
			Encrypted: manifest["IsEncrypted"] == true,
			Complete:  firstString(status["SnapshotState"], "finished") == "finished",
		}
		last, ok := info["Last Backup Date"].(time.Time)
		if !ok {
			last, ok = status["Date"].(time.Time)
		}
		if ok {
			b.Date = pystr.ISOFormat(last.Local())
			b.TS = float64(last.UnixMicro()) / 1e6
		} else if st, err := os.Stat(d); err == nil {
			b.TS = float64(st.ModTime().UnixNano()) / 1e9
		}
		backups = append(backups, b)
	}
	sort.SliceStable(backups, func(i, j int) bool { return backups[i].TS > backups[j].TS })
	return backups
}

func firstString(vals ...any) string {
	for _, v := range vals {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// BackupJob runs Apple's own command-line backup tool in the background.
type BackupJob struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	running bool
	code    *int
	started time.Time
	log     []string
}

// Available reports whether Apple's backup tool is on this Mac.
func (j *BackupJob) Available() bool {
	if !IsMac {
		return false
	}
	_, err := os.Stat(appleMobileBackup)
	return err == nil
}

// Start begins a backup of the device with this serial number (or the only one plugged in).
func (j *BackupJob) Start(serial string) map[string]any {
	if !j.Available() {
		return map[string]any{"error": "Automatic backup isn't available on this Mac. Use Finder instead."}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.running {
		return map[string]any{"ok": true}
	}
	args := []string{"--backup"}
	if serial != "" {
		args = append(args, "--device", udidFromSerial(serial))
	}
	cmd := exec.Command(appleMobileBackup, args...)
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return map[string]any{"error": err.Error()}
	}
	j.cmd, j.running, j.code, j.started, j.log = cmd, true, nil, time.Now(), nil
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			j.mu.Lock()
			j.log = append(j.log, strings.TrimRightFunc(sc.Text(), pystr.IsSpace))
			if len(j.log) > 30 {
				j.log = j.log[len(j.log)-30:]
			}
			j.mu.Unlock()
		}
	}()
	go func() {
		err := cmd.Wait()
		pw.Close()
		code := 0
		if err != nil {
			code = -1
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			}
		}
		j.mu.Lock()
		j.running, j.code = false, &code
		j.mu.Unlock()
	}()
	return map[string]any{"ok": true}
}

// Status is shown in the wizard while the backup runs.
func (j *BackupJob) Status() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.cmd == nil {
		return map[string]any{"state": "idle", "available": j.Available()}
	}
	state := "running"
	var code any
	if !j.running {
		code = *j.code
		state = "failed"
		if *j.code == 0 {
			state = "done"
		}
	}
	tail := j.log
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	return map[string]any{
		"state": state, "code": code, "elapsed": int(time.Since(j.started).Seconds()),
		"log": append([]string{}, tail...), "available": j.Available(),
	}
}

// ---------------------------------------------------------------- getting a copy into ./data

// MacMessagesStats counts the messages in the Messages app's database.
func MacMessagesStats() map[string]any {
	if _, err := os.Stat(MessagesDB); err != nil {
		return map[string]any{"exists": false}
	}
	conn, err := db.OpenReadOnly(MessagesDB)
	if err != nil {
		return map[string]any{"exists": true, "readable": false, "error": err.Error()}
	}
	defer conn.Close()
	var count int64
	var lo, hi *int64
	if err := conn.QueryRow("SELECT COUNT(*), MIN(date), MAX(date) FROM message WHERE date > 0").Scan(&count, &lo, &hi); err != nil {
		return map[string]any{"exists": true, "readable": false, "error": err.Error()}
	}
	iso := func(v *int64) string {
		if v == nil || *v == 0 {
			return ""
		}
		return pystr.ISOFormat(index.AppleTime(*v))
	}
	return map[string]any{"exists": true, "readable": true, "count": count, "oldest": iso(lo), "newest": iso(hi)}
}

// SnapshotSQLite makes a consistent copy of a live SQLite database, including
// anything still in its -wal file, without writing to the original.
func SnapshotSQLite(src, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := strings.TrimSuffix(dest, filepath.Ext(dest)) + ".tmp"
	os.Remove(tmp)
	conn, err := db.OpenReadOnly(src)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Exec("VACUUM INTO " + db.QuoteString(tmp)); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// Copied says where the database (and maybe iPhone contacts) ended up.
type Copied struct {
	DB       string
	Contacts string // "" when there are none
}

// CopyFromMac snapshots this Mac's Messages database into dataDir.
func CopyFromMac(dataDir string) (Copied, error) {
	dest := filepath.Join(dataDir, "chat.db")
	if err := SnapshotSQLite(MessagesDB, dest); err != nil {
		return Copied{}, err
	}
	os.Remove(filepath.Join(dataDir, "contacts.sqlitedb")) // Mac Contacts are read directly by the indexer
	return Copied{DB: dest}, nil
}

// WrongPasswordError asks the wizard to show the password box again.
type WrongPasswordError struct{ Msg string }

func (e *WrongPasswordError) Error() string { return e.Msg }

// ExtractFromBackup copies the Messages database (and Contacts, if present) out of a backup.
func ExtractFromBackup(backupID, dataDir, password string) (Copied, error) {
	bdir := filepath.Join(BackupRoot, backupID)
	manifest := plist.ReadDict(filepath.Join(bdir, "Manifest.plist"))
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return Copied{}, err
	}
	dbOut, abOut := filepath.Join(dataDir, "chat.db"), filepath.Join(dataDir, "contacts.sqlitedb")
	os.Remove(dbOut)
	os.Remove(abOut)

	if manifest["IsEncrypted"] == true {
		if password == "" {
			return Copied{}, &WrongPasswordError{"This backup is encrypted. Enter the backup password."}
		}
		bk, err := backup.Unlock(bdir, password)
		if err == nil {
			defer bk.Close()
			err = bk.Extract(backup.Domain, backup.SMSPath, dbOut)
		}
		if errors.Is(err, backup.ErrWrongPassword) {
			return Copied{}, &WrongPasswordError{"That password didn't unlock the backup. Try again."}
		}
		if err != nil {
			return Copied{}, &WrongPasswordError{fmt.Sprintf("Couldn't unlock this backup. Check the password, or make a new backup. (Details: %v)", err)}
		}
		_ = bk.Extract(backup.Domain, backup.AddressBookPath, abOut) // contacts are optional
	} else {
		sms := filepath.Join(bdir, smsDBHash[:2], smsDBHash)
		if _, err := os.Stat(sms); err != nil {
			return Copied{}, errors.New("Couldn't find Messages in this backup. Try making a new backup.")
		}
		if err := copyFile(sms, dbOut); err != nil {
			return Copied{}, err
		}
		ab := filepath.Join(bdir, addressBookHash[:2], addressBookHash)
		if _, err := os.Stat(ab); err == nil {
			if err := copyFile(ab, abOut); err != nil {
				return Copied{}, err
			}
		}
	}
	c := Copied{DB: dbOut}
	if _, err := os.Stat(abOut); err == nil {
		c.Contacts = abOut
	}
	return c, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func outputWithTimeout(cmd *exec.Cmd, d time.Duration) ([]byte, error) {
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return buf.Bytes(), err
	case <-time.After(d):
		cmd.Process.Kill()
		return nil, errors.New("timed out")
	}
}

// ---------------------------------------------------------------- ordered JSON

// node is parsed JSON that keeps object keys in order (system_profiler output is
// walked in order, so the first iPhone listed is the one we pick).
type node struct {
	kind byte // 'o' object, 'a' array, 's' string, 'v' other value
	keys []string
	vals []*node
	str  string
	val  any
}

func (n *node) get(k string) *node {
	for i, key := range n.keys {
		if key == k {
			return n.vals[i]
		}
	}
	return nil
}

func (n *node) text() string {
	if n.kind == 's' {
		return n.str
	}
	if n.val == nil {
		return "None"
	}
	return fmt.Sprint(n.val)
}

func (n *node) truthy() bool {
	switch n.kind {
	case 's':
		return n.str != ""
	case 'o', 'a':
		return len(n.vals) > 0
	}
	switch v := n.val.(type) {
	case nil:
		return false
	case bool:
		return v
	case json.Number:
		return v.String() != "0"
	}
	return true
}

func parseOrdered(b []byte) (*node, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return readNode(d)
}

func readNode(d *json.Decoder) (*node, error) {
	tok, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		n := &node{kind: 'a'}
		if t == '{' {
			n.kind = 'o'
		}
		for d.More() {
			if n.kind == 'o' {
				k, err := d.Token()
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, fmt.Sprint(k))
			}
			v, err := readNode(d)
			if err != nil {
				return nil, err
			}
			n.vals = append(n.vals, v)
		}
		if _, err := d.Token(); err != nil { // closing delimiter
			return nil, err
		}
		return n, nil
	case string:
		return &node{kind: 's', str: t}, nil
	default:
		return &node{kind: 'v', val: t}, nil
	}
}
