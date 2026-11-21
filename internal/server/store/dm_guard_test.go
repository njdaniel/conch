package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dmTables are the tables of migration 53 that hold what a conversation's
// participants may read, or who they are.
const dmTables = `dm_conversations|dm_viewer_state|direct_messages|direct_message_revisions|direct_message_removals|direct_messages_fts|dm_files|dm_read_marks`

// dmRowRead matches SQL that reads rows of the DM tables, in any case and with
// any spacing, as runRowRead does for runs:
//
//   - the table right after FROM or JOIN;
//   - the table later in a FROM list, after a comma, within one string literal;
//   - a MATCH against the search index, or the function form of it;
//   - an INSERT, REPLACE or UPDATE of the table with RETURNING, which hands
//     back the rows it wrote.
//
// Plain writes and the schema do not match.
var dmRowRead = regexp.MustCompile("(?i)" +
	`\b(from|join)\s+(` + dmTables + `)\b` +
	"|\\bfrom\\b[^;\"`]*?,\\s*(" + dmTables + `)\b` +
	`|\b(direct_messages_fts)\s*(match\b|\()` +
	"|\\b(insert(\\s+or\\s+\\w+)?\\s+into|replace\\s+into|update(\\s+or\\s+\\w+)?)\\s+(" + dmTables + ")\\b[^;\"`]*\\breturning\\b")

// The structural half of the single-visibility-function rule for direct
// messages, as TestOnlyAudienceGoReadsMessageRows is for channel messages:
// only dm_visibility.go reads rows of the DM tables, so the one rule there
// (dmVisibleCond, dmViewTx) is the only way a conversation, a message, a
// revision, a removal, an index hit, a file or a read mark is handed to anyone.
// dm.go writes, and asks dm_visibility.go for what it must know and for the
// row it changed. The migration files hold schema triggers that read the
// tables they guard.
func TestOnlyDMVisibilityGoReadsDMRows(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "dm_visibility.go" ||
			name == "dm_migration.go" || name == "private_call_migration.go" {
			continue
		}
		data, err := os.ReadFile(name) // #nosec G304 -- a source file of this package, from the glob above
		if err != nil {
			t.Fatal(err)
		}
		checked++
		if m := dmRowRead.FindString(string(data)); m != "" {
			t.Errorf("%s contains %q; DM rows are read only in dm_visibility.go (dmViewTx)", name, m)
		}
	}
	if checked < 10 {
		t.Fatalf("only %d source files were checked; the glob is not finding the store", checked)
	}
	// And dm_visibility.go does read them, so the pattern is the one that matters.
	data, err := os.ReadFile("dm_visibility.go")
	if err != nil {
		t.Fatal(err)
	}
	if !dmRowRead.MatchString(string(data)) {
		t.Error("dm_visibility.go reads no DM rows by this pattern; the guard is looking for the wrong thing")
	}
	// Every statement in it that reads a conversation's rows runs after the
	// one condition or embeds it: no public reader skips dmViewTx and
	// dmVisibleCond, which a new reader added here must copy.
	src := string(data)
	for _, reader := range []string{
		"DMConversationFor", "ListDMConversations", "DMMessageFor", "ListDMMessagesPage",
		"ListDMChanges", "SearchDMMessages", "DMReadMarkFor", "DMFileVisibleTo",
	} {
		i := strings.Index(src, "func (s *Store) "+reader+"(")
		if i < 0 {
			t.Fatalf("dm_visibility.go has no %s", reader)
		}
		end := strings.Index(src[i+1:], "\nfunc ")
		body := src[i:]
		if end >= 0 {
			body = src[i : i+1+end]
		}
		if !strings.Contains(body, "dmViewTx(") && !strings.Contains(body, "dmVisibleCond") &&
			!strings.Contains(body, "dmConversationTx(") && !strings.Contains(body, "dmFileTx(") {
			t.Errorf("%s reads without the visibility function", reader)
		}
	}
}

// The guard's pattern catches the spellings a read could take, and leaves the
// writes and the schema alone.
func TestDMRowReadPattern(t *testing.T) {
	for _, c := range []struct {
		sql  string
		read bool
	}{
		{"SELECT * FROM direct_messages WHERE id = ?", true},
		{"select body from direct_messages", true},
		{"SELECT 1 FROM\n\t\tdm_conversations c", true},
		{"SELECT x FROM principals p JOIN dm_files f ON f.uploader_id = p.id", true},
		{"left join direct_message_removals r on r.message_id = m.id", true},
		{"SELECT a FROM principals p, dm_read_marks m WHERE m.principal_id = p.id", true},
		{"select 1 from dm_viewer_state", true},
		{"SELECT body FROM direct_message_revisions", true},
		{"WHERE direct_messages_fts MATCH ?", true},
		{"where direct_messages_fts\n match ?", true},
		{"FROM direct_messages_fts(?) ix", true},
		{"INSERT INTO direct_messages (body) VALUES (?) RETURNING id, body", true},
		{"UPDATE dm_files SET message_id = ? WHERE id = ? RETURNING filename", true},
		{"INSERT INTO direct_messages (body) VALUES (?)", false},
		{"UPDATE dm_files SET message_id = ? WHERE id = ?", false},
		{"INSERT INTO dm_conversations (id, lo, hi) VALUES (?, ?, ?) ON CONFLICT (id) DO NOTHING", false},
		{"CREATE TABLE dm_files (", false},
		{"conversation_id INTEGER NOT NULL REFERENCES dm_conversations (id)", false},
		{"CREATE INDEX direct_messages_by_conversation ON direct_messages (conversation_id, id)", false},
		{"SELECT * FROM direct_messages_archive", false},
		{"SELECT * FROM messages WHERE id = ?", false},
		{"SELECT id FROM principals WHERE id = ?", false},
	} {
		if got := dmRowRead.MatchString(c.sql); got != c.read {
			t.Errorf("%q: matches = %v, want %v", c.sql, got, c.read)
		}
	}
}
