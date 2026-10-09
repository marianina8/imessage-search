// Package plist reads Apple property lists, binary ("bplist00") and XML.
//
// Values come back as: map[string]any, []any, string, []byte, int64, uint64
// (only for values above the int64 range), float64, bool, time.Time (UTC) and UID
// (an object reference inside NSKeyedArchiver data).
package plist

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// UID is a reference to another object in an NSKeyedArchiver archive.
type UID uint64

var appleEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

// ReadFile parses the plist file at path.
func ReadFile(path string) (any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// ReadDict parses a plist file whose top level is a dictionary. Any error gives an empty map.
func ReadDict(path string) map[string]any {
	v, err := ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Parse reads binary or XML plist data.
func Parse(b []byte) (any, error) {
	if bytes.HasPrefix(b, []byte("bplist00")) {
		return parseBinary(b)
	}
	return parseXML(b)
}

// ---------------------------------------------------------------- binary

type bin struct {
	data    []byte
	offsets []uint64
	refSize int
	depth   int
}

func parseBinary(b []byte) (any, error) {
	if len(b) < 8+32 {
		return nil, errors.New("plist: too short")
	}
	t := b[len(b)-32:]
	offSize := int(t[6])
	refSize := int(t[7])
	numObjects := binary.BigEndian.Uint64(t[8:])
	top := binary.BigEndian.Uint64(t[16:])
	tableOff := binary.BigEndian.Uint64(t[24:])
	if offSize < 1 || offSize > 8 || refSize < 1 || refSize > 8 || numObjects > uint64(len(b)) ||
		tableOff+numObjects*uint64(offSize) > uint64(len(b)) {
		return nil, errors.New("plist: bad trailer")
	}
	p := &bin{data: b, refSize: refSize}
	p.offsets = make([]uint64, numObjects)
	for i := range p.offsets {
		p.offsets[i] = readUint(b[tableOff+uint64(i*offSize):], offSize)
	}
	return p.object(top)
}

func readUint(b []byte, n int) uint64 {
	var v uint64
	for i := 0; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v
}

func (p *bin) object(ref uint64) (any, error) {
	if ref >= uint64(len(p.offsets)) {
		return nil, errors.New("plist: bad object reference")
	}
	p.depth++
	defer func() { p.depth-- }()
	if p.depth > 512 {
		return nil, errors.New("plist: nested too deeply")
	}
	off := p.offsets[ref]
	if off >= uint64(len(p.data)) {
		return nil, errors.New("plist: bad offset")
	}
	marker := p.data[off]
	kind, info := marker>>4, int(marker&0x0f)
	body := off + 1
	need := func(n uint64) error {
		if body+n > uint64(len(p.data)) {
			return errors.New("plist: object runs past the end")
		}
		return nil
	}
	length := func() (uint64, error) {
		if info != 0x0f {
			return uint64(info), nil
		}
		if err := need(1); err != nil {
			return 0, err
		}
		m := p.data[body]
		if m>>4 != 0x1 {
			return 0, errors.New("plist: bad length")
		}
		n := uint64(1) << (m & 0x0f)
		body++
		if err := need(n); err != nil {
			return 0, err
		}
		v := readUint(p.data[body:], int(n))
		body += n
		return v, nil
	}
	switch kind {
	case 0x0:
		switch marker {
		case 0x08:
			return false, nil
		case 0x09:
			return true, nil
		}
		return nil, nil
	case 0x1: // int
		n := uint64(1) << info
		if err := need(n); err != nil {
			return nil, err
		}
		switch n {
		case 1, 2, 4:
			return int64(readUint(p.data[body:], int(n))), nil
		case 8:
			return int64(readUint(p.data[body:], 8)), nil
		case 16:
			hi := readUint(p.data[body:], 8)
			lo := readUint(p.data[body+8:], 8)
			if hi == 0 && lo > math.MaxInt64 {
				return lo, nil
			}
			return int64(lo), nil
		}
		return nil, errors.New("plist: bad int size")
	case 0x2: // real
		n := uint64(1) << info
		if err := need(n); err != nil {
			return nil, err
		}
		if n == 4 {
			return float64(math.Float32frombits(uint32(readUint(p.data[body:], 4)))), nil
		}
		return math.Float64frombits(readUint(p.data[body:], 8)), nil
	case 0x3: // date
		if err := need(8); err != nil {
			return nil, err
		}
		secs := math.Float64frombits(readUint(p.data[body:], 8))
		return appleEpoch.Add(time.Duration(secs * float64(time.Second))), nil
	case 0x4: // data
		n, err := length()
		if err != nil {
			return nil, err
		}
		if err := need(n); err != nil {
			return nil, err
		}
		return append([]byte(nil), p.data[body:body+n]...), nil
	case 0x5: // ASCII string
		n, err := length()
		if err != nil {
			return nil, err
		}
		if err := need(n); err != nil {
			return nil, err
		}
		return string(p.data[body : body+n]), nil
	case 0x6: // UTF-16BE string
		n, err := length()
		if err != nil {
			return nil, err
		}
		if err := need(2 * n); err != nil {
			return nil, err
		}
		u := make([]uint16, n)
		for i := range u {
			u[i] = binary.BigEndian.Uint16(p.data[body+uint64(2*i):])
		}
		return string(utf16.Decode(u)), nil
	case 0x8: // UID
		n := uint64(info) + 1
		if err := need(n); err != nil {
			return nil, err
		}
		return UID(readUint(p.data[body:], int(n))), nil
	case 0xA, 0xC: // array, set
		n, err := length()
		if err != nil {
			return nil, err
		}
		if err := need(n * uint64(p.refSize)); err != nil {
			return nil, err
		}
		out := make([]any, n)
		for i := range out {
			v, err := p.object(readUint(p.data[body+uint64(i*p.refSize):], p.refSize))
			if err != nil {
				return nil, err
			}
			out[i] = v
		}
		return out, nil
	case 0xD: // dict
		n, err := length()
		if err != nil {
			return nil, err
		}
		if err := need(2 * n * uint64(p.refSize)); err != nil {
			return nil, err
		}
		out := make(map[string]any, n)
		for i := uint64(0); i < n; i++ {
			k, err := p.object(readUint(p.data[body+i*uint64(p.refSize):], p.refSize))
			if err != nil {
				return nil, err
			}
			v, err := p.object(readUint(p.data[body+(n+i)*uint64(p.refSize):], p.refSize))
			if err != nil {
				return nil, err
			}
			out[fmt.Sprint(k)] = v
		}
		return out, nil
	}
	return nil, fmt.Errorf("plist: unknown object type 0x%x", marker)
}

// ---------------------------------------------------------------- XML

func parseXML(b []byte) (any, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("plist: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok {
			if se.Name.Local == "plist" {
				continue
			}
			return xmlValue(d, se)
		}
	}
}

func xmlText(d *xml.Decoder) (string, error) {
	var sb strings.Builder
	for {
		tok, err := d.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			sb.Write(t)
		case xml.EndElement:
			return sb.String(), nil
		}
	}
}

func xmlValue(d *xml.Decoder, se xml.StartElement) (any, error) {
	switch se.Name.Local {
	case "dict":
		out := map[string]any{}
		key := ""
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				if t.Name.Local == "key" {
					if key, err = xmlText(d); err != nil {
						return nil, err
					}
					continue
				}
				v, err := xmlValue(d, t)
				if err != nil {
					return nil, err
				}
				out[key] = v
			case xml.EndElement:
				return out, nil
			}
		}
	case "array":
		var out []any
		for {
			tok, err := d.Token()
			if err != nil {
				return nil, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				v, err := xmlValue(d, t)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			case xml.EndElement:
				return out, nil
			}
		}
	case "true", "false":
		if err := d.Skip(); err != nil && err != io.EOF {
			return nil, err
		}
		return se.Name.Local == "true", nil
	}
	s, err := xmlText(d)
	if err != nil {
		return nil, err
	}
	switch se.Name.Local {
	case "string":
		return s, nil
	case "integer":
		s = strings.TrimSpace(s)
		if n, err := strconv.ParseInt(s, 0, 64); err == nil {
			return n, nil
		}
		return strconv.ParseUint(s, 0, 64)
	case "real":
		return strconv.ParseFloat(strings.TrimSpace(s), 64)
	case "date":
		return time.Parse(time.RFC3339, strings.TrimSpace(s))
	case "data":
		clean := strings.Map(func(r rune) rune {
			if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
				return -1
			}
			return r
		}, s)
		return base64.StdEncoding.DecodeString(clean)
	}
	return s, nil
}
