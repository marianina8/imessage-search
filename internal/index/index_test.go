package index

import (
	"testing"
	"time"
)

func TestAppleTime(t *testing.T) {
	// 2026-06-18 15:32 UTC in nanoseconds and in seconds since 2001.
	want := time.Date(2026, 6, 18, 15, 32, 0, 0, time.UTC)
	secs := int64(want.Sub(appleEpoch) / time.Second)
	if got := AppleTime(secs * 1e9); !got.Equal(want) {
		t.Errorf("ns: %v", got)
	}
	if got := AppleTime(secs); !got.Equal(want) {
		t.Errorf("s: %v", got)
	}
	if !AppleTime(0).IsZero() {
		t.Error("0 should be no date")
	}
	if got := AppleTime(secs*1e9 + 1500); got.Nanosecond() != 2000 { // rounds to whole microseconds, half to even
		t.Errorf("rounding: %d", got.Nanosecond())
	}
}

func TestNormalizeHandle(t *testing.T) {
	for in, want := range map[string]string{
		"+1 (555) 010-2233": "5550102233", " Jordan.Lee@Example.com ": "jordan.lee@example.com", "12345": "12345", "": "",
	} {
		if got := NormalizeHandle(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestClean(t *testing.T) {
	if got := Clean("  \x1c hi ￼\x00 \n"); got != "hi" {
		t.Errorf("%q", got)
	}
}
