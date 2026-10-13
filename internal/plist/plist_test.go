package plist

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Python's plistlib writes the fixtures, so the reader is checked against a real writer.
func TestAgainstPlistlib(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	dir := t.TempDir()
	script := `
import plistlib, datetime, sys
v = {"IsEncrypted": True, "Device Name": "Marian’s iPhone", "Size": 123456789, "Neg": -5, "Big": 2**40,
     "Data": b"\x00\x01\xff" * 30, "Date": datetime.datetime(2026, 10, 1, 12, 30, 5),
     "List": ["a", 1, 2.5, {"k": "v"}], "Empty": "", "Real": 0.25}
for fmt, name in ((plistlib.FMT_BINARY, "b.plist"), (plistlib.FMT_XML, "x.plist")):
    open(sys.argv[1] + "/" + name, "wb").write(plistlib.dumps(v, fmt=fmt))
`
	if out, err := exec.Command(py, "-c", script, dir).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for _, name := range []string{"b.plist", "x.plist"} {
		m := ReadDict(filepath.Join(dir, name))
		if m["IsEncrypted"] != true || m["Device Name"] != "Marian’s iPhone" || m["Size"] != int64(123456789) ||
			m["Neg"] != int64(-5) || m["Big"] != int64(1<<40) || m["Empty"] != "" || m["Real"] != 0.25 {
			t.Errorf("%s: %#v", name, m)
		}
		if !bytes.Equal(m["Data"].([]byte), bytes.Repeat([]byte{0, 1, 0xff}, 30)) {
			t.Errorf("%s: data", name)
		}
		if !m["Date"].(time.Time).Equal(time.Date(2026, 10, 1, 12, 30, 5, 0, time.UTC)) {
			t.Errorf("%s: date %v", name, m["Date"])
		}
		l := m["List"].([]any)
		if l[0] != "a" || l[1] != int64(1) || l[2] != 2.5 || l[3].(map[string]any)["k"] != "v" {
			t.Errorf("%s: list %#v", name, l)
		}
	}
	if _, err := Parse([]byte("bplist00garbage")); err == nil {
		t.Error("expected error")
	}
	os.Remove(dir)
}
