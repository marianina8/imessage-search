// Package typedstream pulls the message text out of an attributedBody blob.
//
// Recent macOS and iOS often leave message.text empty and store the text inside
// attributedBody: an NSAttributedString archived with NSArchiver's "typedstream"
// format. The text is the first NSString in the archive, written as a '+' type
// code followed by a length and UTF-8 bytes. The length is one byte, or 0x81
// then 2 bytes, or 0x82 then 4 bytes (little-endian).
package typedstream

import (
	"bytes"
	"encoding/binary"
	"unicode/utf8"
)

// Text returns the message text in blob, or "" if it can't find one.
func Text(blob []byte) string {
	i := bytes.Index(blob, []byte("NSString"))
	if i < 0 {
		return ""
	}
	j := bytes.IndexByte(blob[i:], '+')
	if j < 0 {
		return ""
	}
	p := i + j + 1
	if p >= len(blob) {
		return ""
	}
	n := int(blob[p])
	p++
	switch n {
	case 0x81:
		if p+2 > len(blob) {
			return ""
		}
		n = int(binary.LittleEndian.Uint16(blob[p:]))
		p += 2
	case 0x82:
		if p+4 > len(blob) {
			return ""
		}
		n = int(binary.LittleEndian.Uint32(blob[p:]))
		p += 4
	}
	end := p + n
	if end > len(blob) {
		end = len(blob)
	}
	if p > end {
		return ""
	}
	return decodeReplace(blob[p:end])
}

// decodeReplace decodes UTF-8 the way Python's bytes.decode("utf-8", errors="replace")
// does: each maximal invalid sequence becomes one U+FFFD.
func decodeReplace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out []rune
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r != utf8.RuneError || size != 1 {
			out = append(out, r)
			b = b[size:]
			continue
		}
		// Invalid: consume the longest prefix of a would-be sequence (a lead byte
		// plus valid continuation bytes), as Python does.
		skip := 1
		if need := seqLen(b[0]); need > 1 {
			for skip < need && skip < len(b) && b[skip]&0xC0 == 0x80 && validSecond(b[0], b[1], skip) {
				skip++
			}
		}
		out = append(out, utf8.RuneError)
		b = b[skip:]
	}
	return string(out)
}

func seqLen(c byte) int {
	switch {
	case c >= 0xC2 && c <= 0xDF:
		return 2
	case c >= 0xE0 && c <= 0xEF:
		return 3
	case c >= 0xF0 && c <= 0xF4:
		return 4
	}
	return 1
}

// validSecond applies the stricter ranges UTF-8 has for the byte after E0, ED, F0 and F4.
func validSecond(lead, second byte, idx int) bool {
	if idx != 1 {
		return true
	}
	switch lead {
	case 0xE0:
		return second >= 0xA0
	case 0xED:
		return second <= 0x9F
	case 0xF0:
		return second >= 0x90
	case 0xF4:
		return second <= 0x8F
	}
	return true
}
