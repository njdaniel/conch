package tui

// The breakpoints of the layout (docs/design/tui-polish.md). From fullWidth x
// fullHeight (agents.go) the screen has a title bar and the Agents and Runs
// views their two panes; below it the layout is compact: no title bar, and
// those views are one pane. From wideWidth x wideHeight the chat view adds a
// third pane on the right, of people, agents and voice, in place of the
// members line.
const (
	wideWidth  = 120
	wideHeight = 30
	// navWideW, peopleWideW and inboxWideW are the outer widths of the
	// channel list, of the people pane and of the approval list in the wide
	// layout.
	navWideW    = 24
	peopleWideW = 30
	inboxWideW  = 36
)

// paneWidths is the outer width of the left and right panes below the wide
// layout; together with the column left free on the right they fill the
// terminal's width.
func paneWidths(width int) (left, right int) {
	left = 18
	if width < 60 {
		left = 14
	}
	return left, width - left - 1
}

// layout is where everything is on the screen. View draws from it and mouse
// finds what was clicked with it, so the two cannot disagree. Rows and columns
// are 0-based screen cells.
//
// From the top: the title bar when there is one (row 0), then the panes side
// by side, each in a rounded border, from row top: the border is row top and
// row top+contentH+1, and for the left pane column 0 and leftW-1; a pane's
// content is inside it. Under them come the input line, the members line and
// the voice strip when there are those, and the status line.
type layout struct {
	width, height int
	// top is the row the panes start on: 1 under the title bar, 0 without it.
	top int
	// leftW and rightW are the outer widths of the left pane and of the main
	// one beside it; peopleW is that of the people pane, 0 when it is not
	// shown.
	leftW, rightW, peopleW int
	// contentH is the number of content rows of each pane.
	contentH int
	// voice is the voice strip, "" when there is none.
	voice string
	// members is the members line, "" outside the channels view and in the
	// wide layout, where the people pane says more.
	members string
}

// titled reports whether the screen has a title bar: from fullWidth x
// fullHeight up.
func titled(width, height int) bool { return width >= fullWidth && height >= fullHeight }

func (m Model) layout() layout {
	width, height := m.size()
	l := layout{width: width, height: height}
	if titled(width, height) {
		l.top = 1
	}
	l.leftW, l.rightW = paneWidths(width)
	if width >= wideWidth && height >= wideHeight {
		switch m.mode {
		case modeChannels:
			l.leftW, l.peopleW = navWideW, peopleWideW
		case modeInbox, modeDecision:
			// Room for the titles of the approvals, which is what tells
			// them apart.
			l.leftW = inboxWideW
		default:
			l.leftW = navWideW
		}
		l.rightW = width - l.leftW - l.peopleW - 1
	}
	// The members line and the voice strip, when there are those, take a row
	// each from the panes rather than adding one to the screen, and each is
	// one row however many people there are, so the layout does not move as
	// a roster changes.
	if m.mode == modeChannels {
		l.voice = m.voiceStrip(width)
		if l.peopleW == 0 {
			l.members = m.membersLine(width)
		}
	}
	l.contentH = height - l.top - 4
	if l.voice != "" {
		l.contentH--
	}
	if l.members != "" {
		l.contentH--
	}
	if l.contentH < 4 {
		l.contentH = 4
	}
	return l
}

// chatLayout is the layout of the chat view, whichever view is on screen. The
// history's scroll is counted in the lines of the chat pane, so whatever moves
// or anchors it (a message arriving, a resize) lays the history out in that
// pane's width even while the user is in another view, whose main pane is
// wider in the wide layout.
func (m Model) chatLayout() layout {
	m.mode = modeChannels
	return m.layout()
}

// messageRows is how many message lines the message pane shows at once: all
// of its rows. The way back to the newest is in its bottom border.
func (l layout) messageRows() int { return l.contentH }

// leftContent is the content row of the left pane at (x, y), if it is inside it.
func (l layout) leftContent(x, y int) (row int, ok bool) {
	if x < 1 || x > l.leftW-2 || y < l.top+1 || y > l.top+l.contentH {
		return 0, false
	}
	return y - l.top - 1, true
}

// rightContent is the content row of the main pane at (x, y), if it is inside it.
func (l layout) rightContent(x, y int) (row int, ok bool) {
	if x < l.leftW+1 || x > l.leftW+l.rightW-2 || y < l.top+1 || y > l.top+l.contentH {
		return 0, false
	}
	return y - l.top - 1, true
}

// inLeft and inRight say whether (x, y) is on that pane, its border included.
func (l layout) inLeft(x, y int) bool {
	return x >= 0 && x < l.leftW && y >= l.top && y < l.top+l.contentH+2
}

func (l layout) inRight(x, y int) bool {
	return x >= l.leftW && x < l.leftW+l.rightW && y >= l.top && y < l.top+l.contentH+2
}

// rightFoot reports whether (x, y) is on the bottom border of the main pane,
// where the message pane offers the way back to the newest message.
func (l layout) rightFoot(x, y int) bool {
	return x > l.leftW && x < l.leftW+l.rightW-1 && y == l.top+l.contentH+1
}

// channelStart is the index of the first channel the list shows in rows
// rows: the list scrolls to keep the selected channel on screen.
func (m Model) channelStart(rows int) int {
	start, _ := window(len(m.channels), m.selected, rows)
	return start
}
