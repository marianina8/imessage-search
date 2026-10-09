package backup

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/marianina8/imessage-search/internal/db"
)

func TestRFC3394(t *testing.T) {
	kek, _ := hex.DecodeString("000102030405060708090A0B0C0D0E0F")
	key, _ := hex.DecodeString("00112233445566778899AABBCCDDEEFF")
	want, _ := hex.DecodeString("1FA68B0A8112B447AEF34BD8FB5A7B829D3E862371D2CFE5")
	got, err := aesWrap(kek, key)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("wrap: %x %v", got, err)
	}
	back, err := aesUnwrap(kek, want)
	if err != nil || !bytes.Equal(back, key) {
		t.Fatalf("unwrap: %x %v", back, err)
	}
	want[3] ^= 1
	if _, err := aesUnwrap(kek, want); err == nil {
		t.Fatal("tampered key unwrapped")
	}
}

func tlv(tag string, v []byte) []byte {
	b := append([]byte(tag), 0, 0, 0, 0)
	binary.BigEndian.PutUint32(b[4:], uint32(len(v)))
	return append(b, v...)
}

func u32(n uint32) []byte { return binary.BigEndian.AppendUint32(nil, n) }

func encryptCBC(key, plain []byte) []byte {
	p := aes.BlockSize - len(plain)%aes.BlockSize
	plain = append(plain, bytes.Repeat([]byte{byte(p)}, p)...)
	block, _ := aes.NewCipher(key)
	out := make([]byte, len(plain))
	cipher.NewCBCEncrypter(block, make([]byte, 16)).CryptBlocks(out, plain)
	return out
}

// TestEncryptedBackup builds a small encrypted backup the way iOS lays one out,
// then reads sms.db back out of it.
func TestEncryptedBackup(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 needed to write binary plists")
	}
	dir := t.TempDir()
	const password = "correct horse"
	dpsl, salt := bytes.Repeat([]byte{7}, 20), bytes.Repeat([]byte{9}, 20)
	r1, _ := pbkdf2.Key(sha256.New, password, dpsl, 100, 32)
	pk, _ := pbkdf2.Key(sha1.New, string(r1), salt, 10, 32)

	class3 := bytes.Repeat([]byte{3}, 32)
	class4 := bytes.Repeat([]byte{4}, 32)
	w3, _ := aesWrap(pk, class3)
	w4, _ := aesWrap(pk, class4)
	var bag []byte
	for _, part := range [][]byte{
		tlv("VERS", u32(4)), tlv("TYPE", u32(1)), tlv("UUID", bytes.Repeat([]byte{1}, 16)), tlv("HMCK", make([]byte, 40)),
		tlv("WRAP", u32(0)), tlv("SALT", salt), tlv("ITER", u32(10)), tlv("DPWT", u32(1)), tlv("DPIC", u32(100)), tlv("DPSL", dpsl),
		tlv("UUID", bytes.Repeat([]byte{2}, 16)), tlv("CLAS", u32(3)), tlv("WRAP", u32(2)), tlv("KTYP", u32(0)), tlv("WPKY", w3),
		tlv("UUID", bytes.Repeat([]byte{3}, 16)), tlv("CLAS", u32(4)), tlv("WRAP", u32(2)), tlv("KTYP", u32(0)), tlv("WPKY", w4),
	} {
		bag = append(bag, part...)
	}
	manifestKey := bytes.Repeat([]byte{0x42}, 32)
	wm, _ := aesWrap(class4, manifestKey)
	mk := append(binary.LittleEndian.AppendUint32(nil, 4), wm...)

	// The file to recover: a real SQLite database.
	smsPlain := filepath.Join(dir, "plain-sms.db")
	conn, err := db.OpenReadWrite(smsPlain)
	if err != nil {
		t.Fatal(err)
	}
	conn.Exec("CREATE TABLE message(ROWID INTEGER PRIMARY KEY, text TEXT); INSERT INTO message(text) VALUES ('hello from the backup')")
	conn.Close()
	smsBytes, _ := os.ReadFile(smsPlain)
	fileKey := bytes.Repeat([]byte{0x77}, 32)
	wf, _ := aesWrap(class3, fileKey)
	const fileID = "3d0d7e5fb2ce288813306e4d4636395e047a3d28"
	os.MkdirAll(filepath.Join(dir, "bk", "3d"), 0o755)
	os.WriteFile(filepath.Join(dir, "bk", "3d", fileID), encryptCBC(fileKey, smsBytes), 0o600)

	script := `
import plistlib, sys
d, bag, mk, wf, size = sys.argv[1], bytes.fromhex(sys.argv[2]), bytes.fromhex(sys.argv[3]), bytes.fromhex(sys.argv[4]), int(sys.argv[5])
open(d + "/bk/Manifest.plist", "wb").write(plistlib.dumps({"BackupKeyBag": bag, "ManifestKey": mk, "IsEncrypted": True}, fmt=plistlib.FMT_BINARY))
mb = {"$version": 100000, "$archiver": "NSKeyedArchiver", "$top": {"root": plistlib.UID(1)},
      "$objects": ["$null", {"ProtectionClass": 3, "Size": size, "EncryptionKey": plistlib.UID(2), "RelativePath": plistlib.UID(3)},
                   {"NS.data": b"\x03\x00\x00\x00" + wf}, "Library/SMS/sms.db"]}
open(d + "/file.bplist", "wb").write(plistlib.dumps(mb, fmt=plistlib.FMT_BINARY))
`
	if out, err := exec.Command(py, "-c", script, dir, hex.EncodeToString(bag), hex.EncodeToString(mk), hex.EncodeToString(wf),
		strconv.Itoa(len(smsBytes))).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	fileBlob, _ := os.ReadFile(filepath.Join(dir, "file.bplist"))
	mdb := filepath.Join(dir, "plain-manifest.db")
	conn, _ = db.OpenReadWrite(mdb)
	conn.Exec("CREATE TABLE Files (fileID TEXT PRIMARY KEY, domain TEXT, relativePath TEXT, flags INTEGER, file BLOB)")
	conn.Exec("INSERT INTO Files VALUES (?, 'HomeDomain', 'Library/SMS/sms.db', 1, ?)", fileID, fileBlob)
	conn.Close()
	mbytes, _ := os.ReadFile(mdb)
	os.WriteFile(filepath.Join(dir, "bk", "Manifest.db"), encryptCBC(manifestKey, mbytes), 0o600)

	if _, err := Unlock(filepath.Join(dir, "bk"), "wrong"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("wrong password: %v", err)
	}
	e, err := Unlock(filepath.Join(dir, "bk"), password)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	out := filepath.Join(dir, "chat.db")
	if err := e.Extract(Domain, SMSPath, out); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, smsBytes) {
		t.Fatal("decrypted sms.db differs")
	}
	if err := e.Extract(Domain, AddressBookPath, filepath.Join(dir, "ab")); err == nil {
		t.Fatal("expected missing-file error")
	}
}
