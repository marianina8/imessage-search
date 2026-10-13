package macos

import (
	"path/filepath"
	"testing"

	"github.com/marianina8/imessage-search/internal/db"
)

func TestSnapshotIncludesWAL(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "chat.db")
	w, err := db.OpenReadWrite(src)
	if err != nil {
		t.Fatal(err)
	}
	w.SetMaxOpenConns(1)
	w.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0")
	w.Exec("CREATE TABLE message(ROWID INTEGER PRIMARY KEY, text TEXT)")
	for i := 0; i < 50; i++ {
		w.Exec("INSERT INTO message(text) VALUES ('x')")
	}
	// keep w open so the WAL isn't checkpointed, like Messages running
	dest := filepath.Join(dir, "data", "chat.db")
	if err := SnapshotSQLite(src, dest); err != nil {
		t.Fatal(err)
	}
	r, _ := db.OpenReadOnly(dest)
	var n int
	r.QueryRow("SELECT COUNT(*) FROM message").Scan(&n)
	if n != 50 {
		t.Fatalf("got %d messages", n)
	}
	w.Close()
}
