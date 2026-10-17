package main

import (
	"strings"
	"unicode/utf8"
)

// screen is the smallest terminal that can show what the TUI draws: a grid of
// cells that bytes written to a terminal are applied to. A scenario has to ask
// what is on the screen now ("this word, once"), and the raw output is the
// whole history of every frame ever drawn, so searching it can neither count a
// message nor tell that one went away.
//
// It understands what Bubble Tea's renderer writes: printable text, carriage
// return and line feed, cursor movement and positioning, erasing in the line
// and in the screen, and the colour and mode sequences it ignores. Every rune
// takes one cell, which is how the TUI lays out the glyphs it uses.
type screen struct {
	rows, cols int
	cells      [][]rune
	row, col   int

	state   parseState
	params  []byte // the parameter bytes of the control sequence being read
	partial []byte // the start of a multi-byte rune cut by the end of a write
}

type parseState int

const (
	stateText parseState = iota
	stateEsc
	stateCSI
	stateOSC
	stateOSCEsc // an escape inside an operating-system command: "\" ends it
	stateSkip   // the one byte after ESC ( or ESC ) and the like
)

func newScreen(rows, cols int) *screen {
	s := &screen{rows: rows, cols: cols}
	s.clearAll()
	return s
}

func (s *screen) clearAll() {
	s.cells = make([][]rune, s.rows)
	for i := range s.cells {
		s.cells[i] = s.blankRow()
	}
	s.row, s.col = 0, 0
}

func (s *screen) blankRow() []rune {
	row := make([]rune, s.cols)
	for i := range row {
		row[i] = ' '
	}
	return row
}

// write applies p to the screen.
func (s *screen) write(p []byte) {
	if len(s.partial) > 0 {
		p = append(s.partial, p...)
		s.partial = nil
	}
	for len(p) > 0 {
		b := p[0]
		switch s.state {
		case stateText:
			switch {
			case b == 0x1b:
				s.state = stateEsc
				p = p[1:]
			case b == '\r':
				s.col = 0
				p = p[1:]
			case b == '\n':
				// The TUI's line feeds always follow a carriage return, so a
				// line feed here returns to the first column too.
				s.lineFeed()
				s.col = 0
				p = p[1:]
			case b == '\b':
				s.col = max(0, s.col-1)
				p = p[1:]
			case b == '\t':
				s.col = min(s.cols-1, (s.col/8+1)*8)
				p = p[1:]
			case b < 0x20 || b == 0x7f:
				p = p[1:]
			default:
				if !utf8.FullRune(p) {
					s.partial = append([]byte(nil), p...)
					return
				}
				r, size := utf8.DecodeRune(p)
				s.put(r)
				p = p[size:]
			}
		case stateEsc:
			p = p[1:]
			switch b {
			case '[':
				s.state, s.params = stateCSI, s.params[:0]
			case ']':
				s.state = stateOSC
			case '(', ')', '*', '+', '#':
				s.state = stateSkip
			default:
				s.state = stateText
			}
		case stateSkip:
			s.state = stateText
			p = p[1:]
		case stateOSC:
			p = p[1:]
			switch b {
			case 0x07:
				s.state = stateText
			case 0x1b:
				s.state = stateOSCEsc
			}
		case stateOSCEsc:
			s.state = stateText
			p = p[1:]
		case stateCSI:
			p = p[1:]
			if b >= 0x40 && b <= 0x7e {
				s.csi(b)
				s.state = stateText
			} else {
				s.params = append(s.params, b)
			}
		}
	}
}

func (s *screen) put(r rune) {
	if s.col >= s.cols {
		return // the TUI cuts its lines to the width; nothing wraps
	}
	s.cells[s.row][s.col] = r
	s.col++
}

func (s *screen) lineFeed() {
	if s.row < s.rows-1 {
		s.row++
		return
	}
	copy(s.cells, s.cells[1:])
	s.cells[s.rows-1] = s.blankRow()
}

// numbers reads the parameters of a control sequence. A missing one is def.
func (s *screen) numbers(def int) []int {
	text := strings.TrimLeft(string(s.params), "?<=>")
	var out []int
	for _, part := range strings.Split(text, ";") {
		n := 0
		digits := false
		for _, c := range part {
			if c >= '0' && c <= '9' {
				n = n*10 + int(c-'0')
				digits = true
			}
		}
		if !digits {
			n = def
		}
		out = append(out, n)
	}
	return out
}

func (s *screen) csi(final byte) {
	private := len(s.params) > 0 && strings.ContainsRune("?<=>", rune(s.params[0]))
	n := s.numbers(1)[0]
	switch final {
	case 'A':
		s.row = max(0, s.row-max(n, 1))
	case 'B':
		s.row = min(s.rows-1, s.row+max(n, 1))
	case 'C':
		s.col = min(s.cols-1, s.col+max(n, 1))
	case 'D':
		s.col = max(0, s.col-max(n, 1))
	case 'E':
		s.row, s.col = min(s.rows-1, s.row+max(n, 1)), 0
	case 'F':
		s.row, s.col = max(0, s.row-max(n, 1)), 0
	case 'G':
		s.col = min(s.cols-1, max(n, 1)-1)
	case 'd':
		s.row = min(s.rows-1, max(n, 1)-1)
	case 'H', 'f':
		nums := s.numbers(1)
		r, c := nums[0], 1
		if len(nums) > 1 {
			c = nums[1]
		}
		s.row, s.col = min(s.rows-1, max(r, 1)-1), min(s.cols-1, max(c, 1)-1)
	case 'J':
		switch s.numbers(0)[0] {
		case 0:
			s.eraseCols(s.row, s.col, s.cols)
			for r := s.row + 1; r < s.rows; r++ {
				s.cells[r] = s.blankRow()
			}
		case 1:
			s.eraseCols(s.row, 0, s.col+1)
			for r := 0; r < s.row; r++ {
				s.cells[r] = s.blankRow()
			}
		default:
			for r := range s.cells {
				s.cells[r] = s.blankRow()
			}
		}
	case 'K':
		switch s.numbers(0)[0] {
		case 0:
			s.eraseCols(s.row, s.col, s.cols)
		case 1:
			s.eraseCols(s.row, 0, s.col+1)
		default:
			s.eraseCols(s.row, 0, s.cols)
		}
	case 'X':
		s.eraseCols(s.row, s.col, s.col+max(n, 1))
	case 'h', 'l':
		// The alternate screen starts blank.
		if private && final == 'h' {
			for _, mode := range s.numbers(0) {
				if mode == 1049 || mode == 47 || mode == 1047 {
					s.clearAll()
				}
			}
		}
	}
}

func (s *screen) eraseCols(row, from, to int) {
	for c := max(from, 0); c < min(to, s.cols); c++ {
		s.cells[row][c] = ' '
	}
}

// lines is the screen, one string per row, without trailing spaces.
func (s *screen) lines() []string {
	out := make([]string, s.rows)
	for i, row := range s.cells {
		out[i] = strings.TrimRight(string(row), " ")
	}
	return out
}
