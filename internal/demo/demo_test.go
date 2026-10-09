package demo

import (
	"path/filepath"
	"testing"

	"github.com/marianina8/imessage-search/internal/db"
)

func TestBuild(t *testing.T) {
	idx, err := Build(filepath.Join(t.TempDir(), "demo"))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.OpenReadOnly(idx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var n, dana, hits int
	conn.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n)
	conn.QueryRow("SELECT COUNT(*) FROM messages WHERE sender = 'Dana Whitfield'").Scan(&dana)
	conn.QueryRow("SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'deposit'").Scan(&hits)
	if n != len(messages) || dana == 0 || hits != 4 {
		t.Fatalf("messages %d (want %d), from Dana %d, 'deposit' hits %d (want 4)", n, len(messages), dana, hits)
	}
	var label string
	conn.QueryRow("SELECT label FROM chats WHERE chat_id = 2").Scan(&label)
	if label != "Priya Shah" {
		t.Fatalf("chat 2 label %q", label)
	}
}
