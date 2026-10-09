package typedstream

import "testing"

func blob(text string) []byte {
	b := []byte("\x04\x0bstreamtyped\x81\xe8\x03\x84\x01@\x84\x84\x84\x12NSAttributedString\x00\x84\x84\x08NSObject\x00\x85\x92\x84\x84\x84\x08NSString\x01\x94\x84\x01+")
	if len(text) < 0x80 {
		b = append(b, byte(len(text)))
	} else {
		b = append(b, 0x81, byte(len(text)), byte(len(text)>>8))
	}
	b = append(b, text...)
	return append(b, "\x86\x84\x02iI\x01\x05\x92"...)
}

func TestText(t *testing.T) {
	for _, s := range []string{"hi", "café ☕️ 🎉", string(make([]byte, 300))} {
		if got := Text(blob(s)); got != s {
			t.Errorf("got %q want %q", got, s)
		}
	}
	if Text(nil) != "" || Text([]byte("nothing here")) != "" {
		t.Error("expected empty")
	}
	if got := Text(append([]byte("NSString+"), 0x81, 0xff)); got != "" {
		t.Errorf("truncated: %q", got)
	}
}

func TestDecodeReplace(t *testing.T) {
	cases := map[string]string{
		"a\xe2\x82b":   "a�b",
		"a\xffb":       "a�b",
		"\xed\xa0\x80": "���",
		"\xf0\x9f\x98": "�",
		"\xe0\x80\x80": "���",
		"\xc0\xaf":     "��",
	}
	for in, want := range cases {
		if got := decodeReplace([]byte(in)); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
