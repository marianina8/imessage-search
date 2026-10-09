// Package backup reads the Messages and Contacts databases out of a local
// (Finder) iPhone backup, encrypted or not.
//
// Encrypted backups follow Apple's documented iOS data-protection scheme, as used by
// open-source tools such as iphone_backup_decrypt and iphone-dataprotection:
//
//  1. Manifest.plist holds a keybag. The backup password, stretched with
//     PBKDF2-SHA256 and then PBKDF2-SHA1, unwraps the keybag's class keys (AES key wrap).
//  2. A class key unwraps the key for Manifest.db, the SQLite list of every file in
//     the backup. Manifest.db is AES-256-CBC encrypted with a zero IV.
//  3. Each file's own key is stored, wrapped with its class key, in Manifest.db.
//
// Nothing here writes to the backup.
package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/marianina8/imessage-search/internal/db"
	"github.com/marianina8/imessage-search/internal/plist"
)

// Where the files we need live inside a backup.
const (
	Domain          = "HomeDomain"
	SMSPath         = "Library/SMS/sms.db"
	AddressBookPath = "Library/AddressBook/AddressBook.sqlitedb"
)

// ErrWrongPassword means the password didn't unlock the backup's keys.
var ErrWrongPassword = errors.New("wrong backup password")

// Encrypted is an unlocked encrypted backup.
type Encrypted struct {
	dir        string
	classKeys  map[uint32][]byte
	manifestDB string // decrypted copy, in a temp folder
	tmpDir     string
}

// Unlock opens the encrypted backup in dir with password.
func Unlock(dir, password string) (*Encrypted, error) {
	manifest := plist.ReadDict(filepath.Join(dir, "Manifest.plist"))
	bag, _ := manifest["BackupKeyBag"].([]byte)
	mkey, _ := manifest["ManifestKey"].([]byte)
	if len(bag) == 0 || len(mkey) < 4 {
		return nil, errors.New("this backup's Manifest.plist has no encryption keys")
	}
	kb, err := parseKeybag(bag)
	if err != nil {
		return nil, err
	}
	keys, err := kb.unlock([]byte(password))
	if err != nil {
		return nil, err
	}
	e := &Encrypted{dir: dir, classKeys: keys}

	class := uint32(int32(binary.LittleEndian.Uint32(mkey[:4])))
	key, err := e.unwrapForClass(class, mkey[4:])
	if err != nil {
		return nil, fmt.Errorf("manifest key: %w", err)
	}
	enc, err := os.ReadFile(filepath.Join(dir, "Manifest.db"))
	if err != nil {
		return nil, err
	}
	plain, err := decryptCBC(key, enc)
	if err != nil {
		return nil, fmt.Errorf("Manifest.db: %w", err)
	}
	e.tmpDir, err = os.MkdirTemp("", "imsg-manifest-*")
	if err != nil {
		return nil, err
	}
	e.manifestDB = filepath.Join(e.tmpDir, "Manifest.db")
	if err := os.WriteFile(e.manifestDB, unpad(plain), 0o600); err != nil {
		e.Close()
		return nil, err
	}
	return e, nil
}

// Close deletes the decrypted file list.
func (e *Encrypted) Close() {
	if e.tmpDir != "" {
		os.RemoveAll(e.tmpDir)
	}
}

// Extract decrypts the backup file at domain/relativePath into out.
func (e *Encrypted) Extract(domain, relativePath, out string) error {
	conn, err := db.OpenReadOnly(e.manifestDB)
	if err != nil {
		return err
	}
	defer conn.Close()
	var fileID string
	var meta []byte
	err = conn.QueryRow("SELECT fileID, file FROM Files WHERE relativePath = ? AND domain = ? AND flags = 1 LIMIT 1",
		relativePath, domain).Scan(&fileID, &meta)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s isn't in this backup", relativePath)
	}
	if err != nil {
		return err
	}
	info, err := fileInfo(meta)
	if err != nil {
		return err
	}
	if len(fileID) < 2 {
		return errors.New("bad file ID in Manifest.db")
	}
	enc, err := os.ReadFile(filepath.Join(e.dir, fileID[:2], fileID))
	if err != nil {
		return err
	}
	var plain []byte
	if info.key != nil {
		key, err := e.unwrapForClass(info.class, info.key)
		if err != nil {
			return fmt.Errorf("%s: %w", relativePath, err)
		}
		if plain, err = decryptCBC(key, enc); err != nil {
			return fmt.Errorf("%s: %w", relativePath, err)
		}
		plain = unpad(plain)
	}
	if info.size > 0 && int64(len(plain)) > info.size {
		plain = plain[:info.size]
	}
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, plain, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, out)
}

func (e *Encrypted) unwrapForClass(class uint32, wrapped []byte) ([]byte, error) {
	ck, ok := e.classKeys[class]
	if !ok {
		return nil, fmt.Errorf("no key for protection class %d", class)
	}
	if len(wrapped) != 0x28 {
		return nil, fmt.Errorf("wrapped key has length %d, want 40", len(wrapped))
	}
	k, err := aesUnwrap(ck, wrapped)
	if err != nil {
		return nil, err
	}
	return k, nil
}

type fileMeta struct {
	class uint32
	key   []byte // wrapped; nil for an empty file
	size  int64
}

// fileInfo reads a Files.file blob: an NSKeyedArchiver plist of an MBFile.
func fileInfo(blob []byte) (fileMeta, error) {
	var fm fileMeta
	v, err := plist.Parse(blob)
	if err != nil {
		return fm, err
	}
	top, _ := v.(map[string]any)
	objects, _ := top["$objects"].([]any)
	t, _ := top["$top"].(map[string]any)
	root, ok := t["root"].(plist.UID)
	if !ok || int(root) >= len(objects) {
		return fm, errors.New("unexpected file record in Manifest.db")
	}
	file, _ := objects[root].(map[string]any)
	if c, ok := file["ProtectionClass"].(int64); ok {
		fm.class = uint32(c)
	}
	if s, ok := file["Size"].(int64); ok {
		fm.size = s
	}
	if ref, ok := file["EncryptionKey"].(plist.UID); ok && int(ref) < len(objects) {
		holder, _ := objects[ref].(map[string]any)
		data, _ := holder["NS.data"].([]byte)
		if len(data) < 4 {
			return fm, errors.New("short file key")
		}
		fm.key = data[4:]
	}
	return fm, nil
}

// ---------------------------------------------------------------- keybag

type keybag struct {
	attrs   map[string]any // ints (4-byte values) or []byte
	classes []map[string]any
}

const wrapPassphrase = 2

func parseKeybag(b []byte) (*keybag, error) {
	kb := &keybag{attrs: map[string]any{}}
	var current map[string]any
	seenUUID, seenWrap := false, false
	for len(b) >= 8 {
		tag := string(b[:4])
		n := binary.BigEndian.Uint32(b[4:8])
		if uint64(n) > uint64(len(b)-8) {
			return nil, errors.New("keybag: truncated")
		}
		raw := b[8 : 8+n]
		b = b[8+n:]
		var val any = raw
		if len(raw) == 4 {
			val = binary.BigEndian.Uint32(raw)
		}
		switch {
		case tag == "UUID" && !seenUUID:
			seenUUID = true
			kb.attrs[tag] = val
		case tag == "WRAP" && !seenWrap:
			seenWrap = true
			kb.attrs[tag] = val
		case tag == "UUID":
			if current != nil {
				kb.classes = append(kb.classes, current)
			}
			current = map[string]any{"UUID": val}
		case current != nil && (tag == "CLAS" || tag == "WRAP" || tag == "WPKY" || tag == "KTYP" || tag == "PBKY"):
			current[tag] = val
		default:
			kb.attrs[tag] = val
		}
	}
	if current != nil {
		kb.classes = append(kb.classes, current)
	}
	return kb, nil
}

func (kb *keybag) bytesAttr(name string) []byte {
	switch v := kb.attrs[name].(type) {
	case []byte:
		return v
	case uint32:
		return binary.BigEndian.AppendUint32(nil, v)
	}
	return nil
}

func (kb *keybag) intAttr(name string) int {
	if v, ok := kb.attrs[name].(uint32); ok {
		return int(v)
	}
	return 0
}

// unlock derives the key from the password and unwraps every class key it protects.
func (kb *keybag) unlock(password []byte) (map[uint32][]byte, error) {
	salt, iter := kb.bytesAttr("SALT"), kb.intAttr("ITER")
	if salt == nil || iter == 0 {
		return nil, errors.New("keybag has no salt")
	}
	secret := password
	if dpsl := kb.bytesAttr("DPSL"); dpsl != nil { // iOS 10.2 and later add a SHA-256 round
		k, err := pbkdf2.Key(sha256.New, string(password), dpsl, kb.intAttr("DPIC"), 32)
		if err != nil {
			return nil, err
		}
		secret = k
	}
	key, err := pbkdf2.Key(sha1.New, string(secret), salt, iter, 32)
	if err != nil {
		return nil, err
	}
	out := map[uint32][]byte{}
	for _, c := range kb.classes {
		wpky, ok := c["WPKY"].([]byte)
		if !ok {
			continue
		}
		clas, _ := c["CLAS"].(uint32)
		if wrap, _ := c["WRAP"].(uint32); wrap == wrapPassphrase {
			k, err := aesUnwrap(key, wpky)
			if err != nil {
				return nil, ErrWrongPassword
			}
			out[clas] = k
		}
	}
	return out, nil
}

// ---------------------------------------------------------------- crypto helpers

var defaultIV = []byte{0xA6, 0xA6, 0xA6, 0xA6, 0xA6, 0xA6, 0xA6, 0xA6}

// aesUnwrap is the AES key unwrap algorithm (RFC 3394).
func aesUnwrap(kek, wrapped []byte) ([]byte, error) {
	if len(wrapped)%8 != 0 || len(wrapped) < 24 {
		return nil, errors.New("bad wrapped key length")
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	n := len(wrapped)/8 - 1
	a := append([]byte(nil), wrapped[:8]...)
	r := make([][]byte, n)
	for i := range r {
		r[i] = append([]byte(nil), wrapped[8*(i+1):8*(i+2)]...)
	}
	buf := make([]byte, 16)
	for j := 5; j >= 0; j-- {
		for i := n; i >= 1; i-- {
			t := uint64(n*j + i)
			copy(buf, a)
			binary.BigEndian.PutUint64(buf[:8], binary.BigEndian.Uint64(buf[:8])^t)
			copy(buf[8:], r[i-1])
			block.Decrypt(buf, buf)
			copy(a, buf[:8])
			copy(r[i-1], buf[8:])
		}
	}
	if subtle.ConstantTimeCompare(a, defaultIV) != 1 {
		return nil, errors.New("key unwrap failed")
	}
	out := make([]byte, 0, 8*n)
	for _, x := range r {
		out = append(out, x...)
	}
	return out, nil
}

// aesWrap is RFC 3394 key wrap (used by the tests to build a backup).
func aesWrap(kek, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	n := len(key) / 8
	a := append([]byte(nil), defaultIV...)
	r := make([][]byte, n)
	for i := range r {
		r[i] = append([]byte(nil), key[8*i:8*(i+1)]...)
	}
	buf := make([]byte, 16)
	for j := 0; j <= 5; j++ {
		for i := 1; i <= n; i++ {
			copy(buf, a)
			copy(buf[8:], r[i-1])
			block.Encrypt(buf, buf)
			t := uint64(n*j + i)
			binary.BigEndian.PutUint64(a, binary.BigEndian.Uint64(buf[:8])^t)
			copy(r[i-1], buf[8:])
		}
	}
	out := append([]byte(nil), a...)
	for _, x := range r {
		out = append(out, x...)
	}
	return out, nil
}

func decryptCBC(key, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data)%aes.BlockSize != 0 {
		return nil, errors.New("encrypted data isn't a whole number of blocks")
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, make([]byte, aes.BlockSize)).CryptBlocks(out, data)
	return out, nil
}

// unpad removes PKCS#7 padding when it's present and valid.
func unpad(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	p := int(b[len(b)-1])
	if p == 0 || p > aes.BlockSize || p > len(b) {
		return b
	}
	for _, c := range b[len(b)-p:] {
		if int(c) != p {
			return b
		}
	}
	return b[:len(b)-p]
}
