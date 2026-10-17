package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestScreen feeds the screen what Bubble Tea's renderer writes and checks the
// grid it ends with. The scenarios count words on this screen, so a sequence
// it misreads would make them pass or fail for the wrong reason.
func TestScreen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		writes []string
		want   []string // the first rows; the rest are blank
	}{
		{"plain text and a line break", []string{"one\r\ntwo"}, []string{"one", "two"}},
		{"colour is not text", []string{"\x1b[1;36m› \x1b[0mgeneral\r\n"}, []string{"› general"}},
		{"home and overwrite", []string{"abcdef\r\nxyz", "\x1b[Hfoo"}, []string{"foodef", "xyz"}},
		{"erase to the end of the line", []string{"hello world", "\x1b[1G\x1b[5C\x1b[K"}, []string{"hello"}},
		{"erase the whole line", []string{"hello", "\x1b[2K\r", "ok"}, []string{"ok"}},
		{"erase the screen below", []string{"a\r\nb\r\nc", "\x1b[2;1H\x1b[J"}, []string{"a"}},
		{"cursor position", []string{"\x1b[3;4Hx"}, []string{"", "", "   x"}},
		{"skipped lines keep the row", []string{"a\r\n\nc\r\n"}, []string{"a", "", "c"}},
		{"a rune cut across two writes", []string{"\xe2\x80", "\xba x"}, []string{"› x"}},
		{"operating-system commands are skipped", []string{"\x1b]11;rgb:0000/0000/0000\x1b\\ok", "\x1b]0;title\x07!"}, []string{"ok!"}},
		{"private modes are not text", []string{"\x1b[?25l\x1b[?2004hhi"}, []string{"hi"}},
		{"entering the alternate screen clears it", []string{"old", "\x1b[?1049h", "new"}, []string{"new"}},
		{"character set selection", []string{"\x1b(Bab"}, []string{"ab"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScreen(6, 20)
			for _, w := range tc.writes {
				s.write([]byte(w))
			}
			got := s.lines()[:len(tc.want)]
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("rows = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestScreenScrollsAtTheBottom(t *testing.T) {
	s := newScreen(3, 10)
	s.write([]byte("1\r\n2\r\n3\r\n4"))
	if got, want := strings.Join(s.lines(), "|"), "2|3|4"; got != want {
		t.Errorf("rows = %q, want %q", got, want)
	}
}

func TestScreenClipsAtTheWidth(t *testing.T) {
	s := newScreen(2, 5)
	s.write([]byte("abcdefgh\r\nij"))
	if got, want := strings.Join(s.lines(), "|"), "abcde|ij"; got != want {
		t.Errorf("rows = %q, want %q", got, want)
	}
}
