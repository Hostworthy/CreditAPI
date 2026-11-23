package openai

import (
	"strings"
	"testing"
)

func FuzzJPEGOrientation(f *testing.F) {
	f.Add([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x10, 'E', 'x', 'i', 'f', 0, 0, 'I', 'I', 0x2a, 0, 8, 0, 0, 0})
	f.Add([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x10, 'E', 'x', 'i', 'f', 0, 0, 'M', 'M', 0, 0x2a, 0, 0, 0, 8, 0, 1, 1, 0x12})
	f.Fuzz(func(t *testing.T, b []byte) {
		if o := jpegOrientation(b); o < 0 || o > 8 {
			t.Fatalf("orientation %d", o)
		}
	})
}

// FuzzStopFilter checks that, however the text is split into deltas, the
// output is exactly the text up to the first stop sequence.
func FuzzStopFilter(f *testing.F) {
	f.Add("one two STOP three", "STOP", 3)
	f.Add("héllo wörld END", "END", 1)
	f.Fuzz(func(t *testing.T, text, stop string, step int) {
		if stop == "" || step <= 0 {
			return
		}
		fl := newStopFilter([]string{stop})
		var out strings.Builder
		hit := false
		for i := 0; i < len(text) && !hit; i += step {
			var s string
			s, hit = fl.push(text[i:min(i+step, len(text))])
			out.WriteString(s)
		}
		if !hit {
			out.WriteString(fl.flush())
		}
		want := text
		if i := strings.Index(text, stop); i >= 0 {
			want = text[:i]
		}
		if out.String() != want {
			t.Fatalf("got %q want %q", out.String(), want)
		}
	})
}
