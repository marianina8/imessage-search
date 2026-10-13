package index

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/marianina8/imessage-search/internal/db"
)

// NormalizeHandle turns a phone number or email into the key used to look up contacts:
// emails lower-cased, phone numbers reduced to their last 10 digits.
func NormalizeHandle(handle string) string {
	if handle == "" {
		return ""
	}
	if strings.Contains(handle, "@") {
		return strings.ToLower(strings.TrimSpace(handle))
	}
	var digits []rune
	for _, r := range handle {
		if unicode.IsDigit(r) {
			digits = append(digits, r)
		}
	}
	if len(digits) >= 10 {
		digits = digits[len(digits)-10:]
	}
	return string(digits)
}

func personName(first, last, org string) string {
	var parts []string
	for _, p := range []string{first, last} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, " ")
	}
	return org
}

// MacContactsGlob is where the Contacts app keeps its databases.
func MacContactsGlob() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library/Application Support/AddressBook/Sources/*/AddressBook-v22.abcddb")
}

// LoadMacContacts maps normalized phone/email -> name from this Mac's Contacts app.
func LoadMacContacts(pattern string, log io.Writer) map[string]string {
	names := map[string]string{}
	paths, _ := filepath.Glob(pattern)
	for _, path := range paths {
		if err := loadMacContactsFile(path, names); err != nil && log != nil {
			fmt.Fprintf(log, "  (skipping contacts DB %s: %v)\n", path, err)
		}
	}
	return names
}

func loadMacContactsFile(path string, names map[string]string) error {
	conn, err := db.OpenReadOnly(path)
	if err != nil {
		return err
	}
	defer conn.Close()
	people := map[int64]string{}
	rows, err := conn.Query("SELECT Z_PK, ZFIRSTNAME, ZLASTNAME, ZORGANIZATION FROM ZABCDRECORD")
	if err != nil {
		return err
	}
	for rows.Next() {
		var pk int64
		var first, last, org nullStr
		if err := rows.Scan(&pk, &first, &last, &org); err != nil {
			rows.Close()
			return err
		}
		if name := personName(first.s, last.s, org.s); name != "" {
			people[pk] = name
		}
	}
	rows.Close()
	for _, q := range []string{"SELECT ZOWNER, ZFULLNUMBER FROM ZABCDPHONENUMBER", "SELECT ZOWNER, ZADDRESS FROM ZABCDEMAILADDRESS"} {
		rows, err := conn.Query(q)
		if err != nil {
			return err
		}
		for rows.Next() {
			var owner nullInt
			var value nullStr
			if err := rows.Scan(&owner, &value); err != nil {
				rows.Close()
				return err
			}
			if name, ok := people[owner.n]; ok && owner.ok && value.s != "" {
				names[NormalizeHandle(value.s)] = name
			}
		}
		rows.Close()
	}
	return nil
}

// LoadIOSContacts maps normalized phone/email -> name from an iPhone AddressBook.sqlitedb.
func LoadIOSContacts(path string, log io.Writer) map[string]string {
	names := map[string]string{}
	conn, err := db.OpenReadOnly(path)
	if err == nil {
		defer conn.Close()
		var rows interface {
			Next() bool
			Scan(...any) error
			Close() error
		}
		rows, err = conn.Query(`
            SELECT p.First, p.Last, p.Organization, v.property, v.value
            FROM ABMultiValue v JOIN ABPerson p ON p.ROWID = v.record_id
            WHERE v.property IN (3, 4) AND v.value IS NOT NULL`)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var first, last, org, value nullStr
				var prop any
				if err = rows.Scan(&first, &last, &org, &prop, &value); err != nil {
					break
				}
				if name := personName(first.s, last.s, org.s); name != "" {
					names[NormalizeHandle(value.s)] = name
				}
			}
		}
	}
	if err != nil && log != nil {
		fmt.Fprintf(log, "  (couldn't read iPhone contacts: %v)\n", err)
	}
	return names
}

// nullStr scans TEXT, BLOB or NULL into a string.
type nullStr struct{ s string }

func (n *nullStr) Scan(v any) error {
	switch x := v.(type) {
	case nil:
		n.s = ""
	case string:
		n.s = x
	case []byte:
		n.s = string(x)
	default:
		n.s = fmt.Sprint(x)
	}
	return nil
}

type nullInt struct {
	n  int64
	ok bool
}

func (n *nullInt) Scan(v any) error {
	switch x := v.(type) {
	case int64:
		n.n, n.ok = x, true
	case float64:
		n.n, n.ok = int64(x), true
	default:
		n.ok = false
	}
	return nil
}
