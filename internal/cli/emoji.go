package cli

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/njdaniel/conch/pkg/schema"
)

var emojiNames, emojiKeys = buildEmojiVocabulary()

func buildEmojiVocabulary() (byName, byKey map[string]string) {
	byName = make(map[string]string)
	byKey = make(map[string]string)
	for _, entry := range schema.EmojiCatalog() {
		name := EmojiShortcode(entry.Name)
		byName[name] = entry.Key
		byKey[entry.Key] = name
	}
	for _, line := range strings.Split(emojiAliases, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		code, err := strconv.ParseUint(fields[1], 16, 32)
		if err != nil || code > 0x10ffff {
			continue
		}
		key, err := schema.CanonicalReactionKey(string(rune(code)))
		if err != nil {
			continue
		}
		byName[fields[0]] = key
	}
	return byName, byKey
}

// emojiAliases is the pinned Conch alias data from cmd/emoji-gen/aliases.txt.
// The browser catalog is generated from that file and the same Unicode table.
const emojiAliases = `
+1 1F44D
thumbsup 1F44D
-1 1F44E
thumbsdown 1F44E
smile 1F604
smiley 1F603
grin 1F601
laughing 1F606
joy 1F602
lol 1F923
rofl 1F923
wink 1F609
blush 1F60A
innocent 1F607
heart_eyes 1F60D
kissing_heart 1F618
smirk 1F60F
neutral_face 1F610
expressionless 1F611
unamused 1F612
roll_eyes 1F644
thinking 1F914
shushing 1F92B
sweat_smile 1F605
sweat 1F613
disappointed 1F61E
worried 1F61F
angry 1F620
rage 1F621
cry 1F622
sob 1F62D
scream 1F631
fearful 1F628
flushed 1F633
dizzy_face 1F635
sleeping 1F634
mask 1F637
nerd 1F913
hugs 1F917
upside_down 1F643
slightly_smiling_face 1F642
slightly_frowning_face 1F641
confused 1F615
pensive 1F614
relieved 1F60C
yum 1F60B
stuck_out_tongue 1F61B
zany 1F92A
partying 1F973
exploding_head 1F92F
skull 1F480
poop 1F4A9
ghost 1F47B
robot 1F916
see_no_evil 1F648
hear_no_evil 1F649
speak_no_evil 1F64A
heart 2764
orange_heart 1F9E1
yellow_heart 1F49B
green_heart 1F49A
blue_heart 1F499
purple_heart 1F49C
black_heart 1F5A4
white_heart 1F90D
broken_heart 1F494
sparkling_heart 1F496
two_hearts 1F495
100 1F4AF
boom 1F4A5
sparkles 2728
star2 1F31F
zap 26A1
tada 1F389
party_popper 1F389
confetti_ball 1F38A
balloon 1F388
gift 1F381
trophy 1F3C6
medal 1F3C5
rocket 1F680
eyes 1F440
wave 1F44B
clap 1F44F
raised_hands 1F64C
pray 1F64F
ok_hand 1F44C
v 270C
muscle 1F4AA
point_up 261D
point_down 1F447
point_left 1F448
point_right 1F449
fist 270A
punch 1F44A
handshake 1F91D
facepalm 1F926
shrug 1F937
check 2705
white_check_mark 2705
heavy_check_mark 2714
x 274C
warning 26A0
no_entry 26D4
question 2753
exclamation 2757
bulb 1F4A1
link 1F517
paperclip 1F4CE
memo 1F4DD
bug 1F41B
construction 1F6A7
lock 1F512
key 1F511
hourglass 231B
coffee 2615
pizza 1F355
beer 1F37A
cake 1F370
sunny 2600
cloud 2601
snowflake 2744
rainbow 1F308
unicorn 1F984
turtle 1F422
`

// EmojiShortcode derives the browser's CLDR shortcode without a remote service.
func EmojiShortcode(name string) string {
	name = strings.NewReplacer(
		"’", "", "'", "",
		"#", " hash ", "*", " asterisk ", "&", " and ",
		"ñ", "n", "Ñ", "n", "å", "a", "Å", "a", "ç", "c", "Ç", "c",
		"é", "e", "É", "e", "ô", "o", "Ô", "o", "ã", "a", "Ã", "a",
		"ü", "u", "Ü", "u",
	).Replace(name)
	var b strings.Builder
	underscore := false
	for _, r := range strings.ToLower(name) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if underscore && b.Len() > 0 {
				b.WriteByte('_')
			}
			underscore = false
			b.WriteRune(r)
		} else {
			underscore = true
		}
	}
	return b.String()
}

// ResolveReactionKey turns one Unicode emoji or a known :shortcode: into the
// canonical key sent on the wire. Unknown shortcodes never reach the server.
func ResolveReactionKey(input string) (string, error) {
	input = strings.TrimSpace(input)
	if key, err := schema.CanonicalReactionKey(input); err == nil {
		return key, nil
	}
	name := strings.ToLower(input)
	if len(name) >= 2 && strings.HasPrefix(name, ":") && strings.HasSuffix(name, ":") {
		name = name[1 : len(name)-1]
	}
	if key := emojiNames[name]; key != "" {
		return key, nil
	}
	return "", fmt.Errorf("unknown emoji or shortcode %q", safeLine(input))
}

var shortcodeToken = regexp.MustCompile(`(^|[\s(\[\{]):([A-Za-z0-9_+-]+):`)

// ExpandShortcodes replaces known closed :shortcode: tokens with ordinary
// Unicode. Unknown tokens and shortcodes inside words or URLs remain literal.
func ExpandShortcodes(text string) string {
	matches := shortcodeToken.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text
	}
	var out strings.Builder
	last := 0
	for _, match := range matches {
		start, end := match[3], match[1]
		name := strings.ToLower(text[match[4]:match[5]])
		key := emojiNames[name]
		tokenStart := match[3]
		from := tokenStart
		for from > 0 && !unicode.IsSpace(rune(text[from-1])) {
			from--
		}
		if key == "" || strings.Contains(text[from:tokenStart], "://") {
			continue
		}
		out.WriteString(text[last:start])
		out.WriteString(key)
		last = end
	}
	if last == 0 {
		return text
	}
	out.WriteString(text[last:])
	return out.String()
}

// ReactionLabel renders a canonical key as a glyph, or as its pinned shortcode
// in explicit text mode. A key unknown to this catalog uses safe code points.
func ReactionLabel(key string, text bool) string {
	if !text {
		return key
	}
	if name := emojiKeys[key]; name != "" {
		for alias, aliasKey := range emojiNames {
			if aliasKey == key && alias != "+1" && alias != "-1" && (len(alias) < len(name) || len(alias) == len(name) && alias < name) {
				name = alias
			}
		}
		return ":" + name + ":"
	}
	parts := make([]string, 0, 4)
	for _, r := range key {
		parts = append(parts, fmt.Sprintf("U+%04X", r))
	}
	if len(parts) == 0 {
		return "[empty emoji]"
	}
	return "[" + strings.Join(parts, " ") + "]"
}
