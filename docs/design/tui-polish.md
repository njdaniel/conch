# TUI polish: layout, navigation and chat

**Status:** Implemented for issue #538; waiting for visual and interaction acceptance by the maintainer.
**Code:** `internal/cli/tui` (`layout.go`, `view.go`, `style.go`, `history.go`, `help.go`, `mouse.go`).
**References:** [terminal server lobby](mockups/terminal-server-lobby.png), [terminal chat](mockups/terminal-chat.png), [caveats](mockups/README.md), [client experience](client-experience.md) UX-01 to UX-08 and UX-18 to UX-23.

This page records what the terminal client looks like and how it is driven after #538: the
breakpoints, the exact key map, the style tokens, how it compares with the two terminal
references, and terminal captures of the real program before and after the change. Behaviour,
the API and the audio lifecycle did not change; where the references ask for something the API
or the voice track does not have yet, it is listed under [follow-ups](#follow-ups), not built.

## The layout

From the top: a title bar, the panes, the input line, the members line, the voice strip and
the status line. `layout()` decides where each is, and both `View` and the mouse read it, so a
click lands on what is drawn (#83).

| Row (80x24) | What |
| --- | --- |
| 0 | Title bar: `CONCH`, the server, the views with the one on screen in brackets (`[Chat] Inbox Agents Runs`, or `[run #41]`), who is signed in, the text connection (`● text live`), how to open the help |
| 1 to 21 | The panes, each in a rounded border with its title in the top border. The pane the arrow keys move has `▸` before its title and a cyan border |
| 22 | The input line: `general > draft▌`, or `general/alpha > ` on a net. In the decision prompt the reason; in run detail its message or question |
| 23 | The status line: who is signed in, the last status, and the key hints of the view |

The members line and the voice strip each take one row from the panes when they have
something to say, and are one row however many people there are, so the layout does not move as
a roster changes. The chat pane's bottom border carries `↓ 3 newer · end: latest` while the
history is scrolled up.

### Breakpoints

| Terminal | Layout |
| --- | --- |
| 120x30 and more: **wide** | Title bar. Channel list (24 cells), messages, and the people pane (30 cells) with the channel's people, its agents and its voice room. The people pane replaces the members line. The Agents and Runs views' channel list is 24 cells too, and the approval list 36. |
| 80x24 and more: **normal** | Title bar. Channel list (18 cells) and messages; the members line under the input. The Agents and Runs views show the channel list beside their own pane. |
| 40x10 up to 80x24: **compact** | No title bar: who is signed in stays in the status line. Channel list of 18 cells, 14 below 60 columns; message times are dropped below 60 columns, before names are. The Agents and Runs views are one pane. Key hints drop the least needed first (`←/→ focus`, then `F1 help`, then the arrows); `esc quit` and `enter send` stay. |
| Under 40x10 | `terminal too small` with the size needed and `esc: quit`, filling the screen exactly. The mouse does nothing. |

The inbox and the decision prompt keep their own minimum of 40x14, below which they say they
are too small and no decision can be cast (#205). Optional detail goes first as the terminal
shrinks: the people pane collapses to the members line, then the title bar goes, then message
times. The channel, its audience in the prompt, the input and the way out are on every layout
(tested at every breakpoint in `TestLayoutBreakpoints`). A channel list longer than its pane
scrolls with the selection.

A resize keeps the selected channel, the draft, the focus, the view and the open run, which do
not depend on the terminal. A history read scrolled up keeps the same message at the bottom of
the pane, however its lines wrap at the new width.

## The key map

Bindings that existed before #538 are unchanged. New ones are marked *new*.

**Every view.** `F1` opens and closes the help (*new*); `?` does too where nothing is typed
(Inbox, Agents, Runs, run detail without a prompt; *new*). While the help is open it takes every
key: `esc`, `F1`, `?` and `enter` close it, `↑`/`↓`/`pgup`/`pgdn` scroll it when it is
longer than the pane (its bottom border says how much is below), `ctrl+c` closes it and then
does what it does in the view underneath, and nothing else reaches that view, so nothing typed
while reading it is sent, decided or asked, and no channel or view changes. `ctrl+c` quits
everywhere except the decision prompt and run detail's prompts, where it cancels, as before.

| View | Keys |
| --- | --- |
| Chat | Typing goes to the input whatever has the focus. `enter` sends to the audience the prompt names; `backspace` deletes. `/net NAME`, `/net`, `/w IDS TEXT`, `//TEXT` as before; `/help` (*new*) opens the help. `↑`/`↓` move the focused pane: the channel list (the default, as before) or the history, a line at a time (*new*). `←`/`→` move the focus between the channel list and the messages (*new*). `pgup`/`pgdn` scroll the history a page. `home` goes to the oldest message held, `end` back to the newest (*new*). `tab` Inbox, `shift+tab` Agents (*new*). `esc` quits. |
| Inbox | `↑`/`↓` choose; `pgup`/`pgdn` scroll the body; `enter` decides; `tab` Agents; `shift+tab` Chat (*new*); `esc` quits. |
| Decision prompt | `↑`/`↓` choose an option; typing is the reason; `enter` casts; `pgup`/`pgdn` scroll the body; `esc` cancels. `tab` and `shift+tab` do nothing here. |
| Agents | `↑`/`↓` choose; `enter` unfolds or opens a run; `r` Runs; `tab` Chat; `shift+tab` Inbox (*new*); `esc` quits. |
| Runs | `↑`/`↓`/`pgup`/`pgdn` choose; `enter` opens; `f` filter; `a` Agents; `tab` Chat; `shift+tab` Agents (*new*); `esc` quits. |
| Run detail | `↑`/`↓`/`pgup`/`pgdn` the timeline; `enter` data; `g` the message; `m` `x` `p` `r` only as the session supports them, with the cancel question as before; `esc` back. |

**Mouse** (unchanged rule: a click never sends, decides or quits). Chat: a click on a channel
opens it and focuses the list; a click elsewhere in the list focuses it; a click in the message
pane focuses it (*new*); a click on `end: latest` in its bottom border goes to the newest message
(*new*); the wheel over the message pane scrolls it. Inbox: a click selects an approval, the
wheel moves the selection. Decision prompt: a click selects an option. Agents, Runs, run detail
and the help: nothing.

**Collision audit.** `←`/`→`, `home`, `end` and `shift+tab` did nothing before in every view
(the input has no cursor to move), so taking them changes no existing binding. If the input
gets a movable cursor later, the focus keys have to move (to `alt+←`/`alt+→`, say); that is
noted below. `?` is still typed in the chat input and the decision reason, where it is text.
`F1` is kept by some terminals for themselves (GNOME Terminal opens its own help unless that
shortcut is turned off), so the title bar always names the way that works in the view on screen:
`help: F1, /help` in Chat and `help: F1, ?` elsewhere. The mockups' `F2`, `F5` and `F10` are not
used: `F10` opens GNOME Terminal's menu, and the voice and member controls they stood for are
#542's.

## Style tokens

Colours are the terminal's own 16-colour palette (`style.go`), so they follow its theme on a
dark or a light background. None carries meaning alone.

| Token | Default | Used for | Without colour |
| --- | --- | --- | --- |
| `borderStyle` | rounded border, colour 8 | a pane the keys are not on | the border |
| `focusBorderStyle` | rounded border, colour 6 | the pane the arrow keys move | `▸ ` before its title |
| `activeStyle` | bold, colour 6 | the selection, the view on screen, the way back to the newest | `›` before the selection, `[Chat]` in brackets |
| `badgeStyle` | bold, colour 3 | `[agent]`, `[net:alpha]`, `[whisper:3]`, a payload, an unread count | the brackets, and a count after the name |
| `statusStyle` | colour 8 | times, hints, empty states, the continuation mark `┊` | position and wording |
| `titleStyle` | bold | pane titles, `CONCH`, headings in the people pane | upper case |
| `authorStyle` | bold | a message's author, who is signed in | after the time, before `:` |
| `okStyle`, `warnStyle` | colour 2, colour 3 | the text connection up, or not | `● text live`, `◌ text connecting`, `◐ text reconnecting`, `○ text unavailable` |

Presence uses a glyph and a word everywhere: `●` online, `◐` stale, `○` offline, with the word
beside it in the people pane, and `● … talking` in voice.

**Plain and high-contrast.** With `NO_COLOR` (or `CLICOLOR=0`), a pipe or a monochrome
terminal, lipgloss draws no styling at all, not even bold, and the terminal's own foreground on
its own background is the highest contrast it has. Everything above is still said in text.
`TestPlainAndStyledShowTheSameText` renders the chat, inbox and Agents views with colour and
without, and checks that the text is identical once the escape sequences are removed, and that
the plain screen carries `›`, `▸`, `[Chat]`, `[agent]`, the presence glyphs and their words.

## Conversation

A message takes as many lines as it needs. The first is what it was before #538,
`[scope] HH:MM author [agent] [payload]: body`, from the same code (`messageLine`), so every
guarantee against a body or a name posing as a badge still holds. The rest of the body wraps at
spaces (inside a word only when the word is longer than a line: an address, a run of wide
characters) on lines that start with at least two spaces and `┊`, under the author. A head never
starts with a space, so no line of a body can pass for the start of another message or for a
badge. Line breaks in the body are kept; control characters, escape sequences and bidi controls
are removed as before; widths are counted in terminal cells, never bytes or runes. A body that
starts with `[` keeps its backslash.

The history scrolls by line. A reader scrolled up stays where they are when messages arrive,
however many lines those take, and the pane's bottom border says how many newer messages are
below and that `end` (or a click on it) goes back. Going back to the newest moves the read mark
as before. `g` in run detail lands on the message the run answers with the history wrapped.
Empty, loading and failed states say so in the message pane: `loading history…`,
`No messages in general yet. Type below and press enter to send one.`, and the channel-list
failure of #522 with the server and the whole error.

## People, agents and voice

The people pane (wide layout) lists every member of the channel once, people first by liveness
then agents, each with a glyph and a word: `● bob  online ×2` for two live sessions, `○ carol
offline`, `● rex [agent] running` with its task under it. It reads `people: unavailable` while
the roster may be stale, as the members line and the Agents view do. A list too long for the pane
ends in how many lines are not shown. The Agents view and run detail are unchanged, with their
interventions and confirmations.

Voice stays display only until #542. The voice strip, under the input, first says what the
presence of the channel on screen shows of the person here, and only that:
`you: not in #general voice`, or `you: in #general voice from another client` (this terminal does
not join voice), with `● talking` when presence says they are transmitting. That presence covers
only this channel's rooms, so the strip never says "not in voice" outright: the person may be in
another channel's room. While presence is reconnecting, or LiveKit cannot be reached, it says
nothing about the person (`#general voice: reconnecting`). The TUI does not say whether a
microphone is open: it cannot know what `conch-voice` or a browser is doing. Then come who is in
the room, `voice: 2 alice  ●3 bob`, with names from the channel's member list now as well as
whoami. Browsing another channel changes which room is described, never where the person is
speaking. #542 puts the join, leave and talk state in the strip's left part; the strip already has
its row on every layout. (Since #542 it does: see [tui-voice.md](tui-voice.md) §15 and §19. With
no helper running the strip is as described here, except that `you: not in #general voice` reads
`voice off · /voice join` in a TUI that can join.)

## Captures

Captured from the real `conch` against a local `conchd` with the same seeded data, in tmux 3.4
(a private tmux server, `TERM=xterm-256color`, `COLORTERM=truecolor`) on Linux, with
`tmux capture-pane -p` (plain text) and `-p -e` (colour). In the colour files the escape
character is written `␛` so that they pass `scripts/invisible-chars.sh`; to replay one, turn `␛`
back into ESC (`sed 's/␛/\x1b/g' file`). *Before* is `main` at `5c2d943`; *after* is this branch. The *after* chat, scrolled and
help captures and the plain one were taken again, on the same database after a restart of the
server, when the voice strip's wording changed; session counts and times there differ a little
from the other captures.

**Seeded data** (test names only): people `alice` (signed in), `bob` (online from a browser
session), `carol-with-a-rather-long-display-name`, `dave` (a Japanese message), and the agent
`rex` with a live session, one finished run and one running run with two activity entries, and
one approval it raised. Channels `general` (all the messages), `ops` (two unread for alice),
`movie-night`, `design-review`. Messages: short ones, a long one, a long address, a multi-line
list, wide characters, a body with an escape sequence, a bell, a bidi override and a fake
`[net:alpha]` badge, and one that opens with a fake `[whisper:2,3]`. Voice: a stand-in for
LiveKit's room API reports alice (quiet) and bob (talking) in general's room. No microphone,
PipeWire or LiveKit was involved, and no capture can show that audio works.

| Screen | Sizes | Before | After |
| --- | --- | --- | --- |
| Chat (the lobby) | 160x48, 120x36, 80x24, 60x18 | `before/chat-*.txt` | `after/chat-*.txt` |
| Chat, scrolled up a page | same | `before/scrolled-*.txt` | `after/scrolled-*.txt` |
| Inbox | same | `before/inbox-*.txt` | `after/inbox-*.txt` |
| Agents, rex unfolded | same | `before/agents-*.txt` | `after/agents-*.txt` |
| Run detail | same | `before/run-*.txt` | `after/run-*.txt` |
| Help (F1) | same | `before/help-*.txt` (F1 did nothing) | `after/help-*.txt` |
| Failure: no server at the address | same | `before/failure-*.txt` | `after/failure-*.txt` |
| Decision prompt | 80x24 | `before/decision-80x24.txt` | `after/decision-80x24.txt` |
| Chat with `NO_COLOR` | 120x36 | `plain/before/chat-120x36.txt` | `plain/after/chat-120x36.txt` |

Each `.txt` has a colour twin, `.ansi.txt`. All are in [tui-polish/](tui-polish/).

### Compared with the references

| Reference | Here | Difference, and why |
| --- | --- | --- |
| Top bar: `CONCH / The Common Room`, online count, `Server connected` | Title bar: `CONCH`, the server's address, the views, who is signed in, `● text live` | The API has no server display name, so the address stands in. The online count is in the people pane's title. The connection is labelled *text*, so it cannot be read as voice (UX-01). |
| Left: `CHANNELS` with `> # general`, then `VOICE ROOMS` with who is in each | Left: `CHANNELS` with `›` and unread counts | Voice rooms are not listed apart: presence is per channel and the TUI cannot join one. The channel's room is in the people pane and the voice strip. |
| Centre: `# general CHAT`, a "who is in voice" banner, `[F2] Choose voice room` | `# general` in the pane's border, `› net alpha` when sending to a net | No banner and no F2: choosing a room is #542's, and a fake control is not offered (UX-04). |
| Message: time column, author on its own line, body under it | Time, author and the start of the body on one line; the rest under the author, marked `┊` | Keeps 80x24 dense, and keeps the one-line head whose position is what stops a body posing as a badge. |
| Composer box: `Message #general`, `> text`, `Enter send` | One input row under the panes: `general > text▌`; hints on the status line | A box costs two rows on 80x24. The prompt already names the audience, as before. |
| Right: `ONLINE · 6`, each with `Online · English` | `PEOPLE · 2 online` from 120x30: people, agents, voice room | Languages are not in the API. Agents are listed apart with their status (UX-18). |
| Bottom strip: `NOT IN VOICE \| Microphone inactive` and F-key hints | Voice strip `you: not in #general voice · voice: …` and the status line | The microphone is not described: the TUI does not know a helper's capture state (UX-11). |
| Chat mockup: voice roster with PTT/TALK/OPEN/MUTED, "Your microphone Ready", live captions | Not built | Captions are out of scope; `OPEN` is not a policy (mockups README); push-to-talk state is #542's. |

## Follow-ups

Gaps found while doing this, each for its own issue rather than this one:

1. **Voice controls in the strip** (#542): join, leave and talk state in the strip's left part,
   from the integrated helper (#537).
2. **A multi-line input** (UX-06): a newline binding that works in common terminals, and cursor
   movement in the input; the latter would take `←`/`→` from the focus keys.
3. **Older history.** `home` goes to the oldest message held: the newest 100 and what arrived
   since. Paging further back over `GET /v2/channels/{channel}/messages?before=` is a feature of
   its own.
4. **Pending approvals in the tab bar.** The inbox is read when it is opened; a count on `Inbox`
   needs a read at start and on events.
5. **A second help key** for terminals that keep `F1`, if `/help` and `?` prove not enough in
   the acceptance terminal.
6. **A server display name** in the API, for the title bar.
7. **Selecting a person or agent in the people pane** to open it in the Agents view.
8. **A distinct high-contrast palette**, if the plain presentation is not enough at acceptance.

## Checked, and not

Checked: the unit tests named above and in `view_test.go` and `mouse_test.go`, the race and
pseudo-terminal tests of `internal/cli/...` and `cmd/conch`, the end-to-end runs that drive the
TUI on pseudo-terminals (`e2e/p3-local`, `e2e/p2-hardening`), and the captures. Not checked by the
author: a graphical terminal emulator outside tmux (the captures are tmux only), a light
background, and any real microphone. Those are part of the maintainer's acceptance.
