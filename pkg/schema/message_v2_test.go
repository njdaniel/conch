package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMessageV2GoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "channel-wide", file: "message-v2.json", new: func() any { return &MessageV2{} }},
		{name: "net", file: "message-v2-net.json", new: func() any { return &MessageV2{} }},
		{name: "whisper", file: "message-v2-whisper.json", new: func() any { return &MessageV2{} }},
		{name: "post request", file: "post-message-request-v2.json", new: func() any { return &PostMessageRequestV2{} }},
		{name: "post request net", file: "post-message-request-v2-net.json", new: func() any { return &PostMessageRequestV2{} }},
		{name: "post request whisper", file: "post-message-request-v2-whisper.json", new: func() any { return &PostMessageRequestV2{} }},
		{name: "post response", file: "post-message-response-v2.json", new: func() any { return &PostMessageResponseV2{} }},
		{name: "list response", file: "list-messages-response-v2.json", new: func() any { return &ListMessagesResponseV2{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
			assertFixtureValidates(t, tt.file, tt.new)
		})
	}
}

// assertFixtureValidates decodes the fixture again and runs Validate on it, or
// on every envelope it embeds: a published fixture must be a value the schema
// itself accepts.
func assertFixtureValidates(t *testing.T, file string, new func() any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", file)) // #nosec G304 -- fixture name comes from this package's own test table
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	value := new()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	var targets []validator
	switch v := value.(type) {
	case *PostMessageResponseV2:
		targets = append(targets, v.Message)
	case *ListMessagesResponseV2:
		for _, m := range v.Messages {
			targets = append(targets, m)
		}
	case *CreateNetResponseV1:
		targets = append(targets, v.Net)
	case *ListNetsResponseV1:
		for _, n := range v.Nets {
			targets = append(targets, n)
		}
	case validator:
		targets = append(targets, v)
	default:
		t.Fatalf("fixture type %T has no Validate", value)
	}
	for _, target := range targets {
		if err := target.Validate(); err != nil {
			t.Errorf("fixture does not validate: %v", err)
		}
	}
}

func validMessageV2() MessageV2 {
	return MessageV2{
		Schema:    MessageSchemaV2,
		ID:        1,
		ChannelID: 2,
		AuthorID:  3,
		CreatedAt: NewTimestamp(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)),
		Body:      "hi",
	}
}

func TestMessageV2Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*MessageV2)
		wantErr string
	}{
		{name: "valid channel-wide", mutate: func(*MessageV2) {}},
		{name: "valid with known payload", mutate: func(m *MessageV2) {
			m.Payload = &Payload{Schema: LeviathanTradeSignalV1Name, Data: json.RawMessage(`{"any":"json"}`)}
		}},
		{name: "valid with unknown payload", mutate: func(m *MessageV2) {
			m.Payload = &Payload{Schema: "acme.weather_alert.v3", Data: json.RawMessage(`{"x":1}`)}
		}},
		{name: "valid net audience", mutate: func(m *MessageV2) {
			m.Audience = &Audience{Kind: AudienceKindNet, NetID: 3}
		}},
		{name: "valid principals audience", mutate: func(m *MessageV2) {
			m.Audience = &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3, 4, 7}}
		}},
		{name: "valid principals audience of two", mutate: func(m *MessageV2) {
			m.Audience = &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3, 4}}
		}},

		// Everything MessageV1.Validate enforces.
		{name: "v1 schema", mutate: func(m *MessageV2) { m.Schema = MessageSchemaV1 }, wantErr: `schema must be "conch.message.v2"`},
		{name: "empty schema", mutate: func(m *MessageV2) { m.Schema = "" }, wantErr: `schema must be "conch.message.v2"`},
		{name: "zero id", mutate: func(m *MessageV2) { m.ID = 0 }, wantErr: "id must be positive"},
		{name: "zero channel", mutate: func(m *MessageV2) { m.ChannelID = 0 }, wantErr: "channel_id must be positive"},
		{name: "zero author", mutate: func(m *MessageV2) { m.AuthorID = 0 }, wantErr: "author_id must be positive"},
		{name: "zero created_at", mutate: func(m *MessageV2) { m.CreatedAt = Timestamp{} }, wantErr: "created_at is required"},
		{name: "empty body", mutate: func(m *MessageV2) { m.Body = "" }, wantErr: "body is required"},
		{name: "payload bad name", mutate: func(m *MessageV2) {
			m.Payload = &Payload{Schema: "no_version", Data: json.RawMessage(`{}`)}
		}, wantErr: "not a valid versioned name"},
		{name: "payload empty data", mutate: func(m *MessageV2) {
			m.Payload = &Payload{Schema: LeviathanTradeSignalV1Name}
		}, wantErr: "payload data is required"},
		{name: "payload invalid json", mutate: func(m *MessageV2) {
			m.Payload = &Payload{Schema: LeviathanTradeSignalV1Name, Data: json.RawMessage(`{not json`)}
		}, wantErr: "not valid JSON"},

		// Audience well-formedness (Audience.Validate, through the envelope).
		{name: "unknown audience kind", mutate: func(m *MessageV2) {
			m.Audience = &Audience{Kind: "channel"}
		}, wantErr: `audience kind "channel" is not one of net, principals`},
		{name: "empty audience kind", mutate: func(m *MessageV2) {
			m.Audience = &Audience{}
		}, wantErr: `audience kind "" is not one of net, principals`},
		{name: "net audience without net_id", mutate: func(m *MessageV2) {
			m.Audience = &Audience{Kind: AudienceKindNet}
		}, wantErr: "net_id must be positive"},
		{name: "principals audience without ids", mutate: func(m *MessageV2) {
			m.Audience = &Audience{Kind: AudienceKindPrincipals}
		}, wantErr: "at least one principal_id"},

		// The normalized read-back form.
		{name: "principals audience unsorted", mutate: func(m *MessageV2) {
			m.Audience = &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3, 7, 4}}
		}, wantErr: "must be sorted ascending"},
		{name: "principals audience duplicate", mutate: func(m *MessageV2) {
			m.Audience = &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3, 4, 4}}
		}, wantErr: "more than once"},
		{name: "principals audience without author", mutate: func(m *MessageV2) {
			m.Audience = &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{4, 7}}
		}, wantErr: "must include the author 3"},
		{name: "principals audience of the author alone", mutate: func(m *MessageV2) {
			m.Audience = &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3}}
		}, wantErr: "at least one principal other than the author"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := validMessageV2()
			tt.mutate(&m)
			assertValidation(t, m.Validate(), tt.wantErr)
		})
	}
}

func TestAudienceValidate(t *testing.T) {
	many := make([]int64, MaxAudiencePrincipals)
	for i := range many {
		many[i] = int64(i + 1)
	}
	tooMany := append(append([]int64{}, many...), MaxAudiencePrincipals+1)

	tests := []struct {
		name     string
		audience Audience
		wantErr  string
	}{
		{name: "valid net", audience: Audience{Kind: AudienceKindNet, NetID: 3}},
		{name: "valid principals", audience: Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{4, 7}}},
		{name: "valid principals, one id", audience: Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{4}}},
		{name: "valid principals, any order", audience: Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{7, 4}}},
		{name: "valid principals, maximum", audience: Audience{Kind: AudienceKindPrincipals, PrincipalIDs: many}},

		{name: "zero value", audience: Audience{}, wantErr: `audience kind "" is not one of net, principals`},
		{name: "unknown kind channel", audience: Audience{Kind: "channel"}, wantErr: `audience kind "channel" is not one of net, principals`},
		{name: "unknown kind broadcast", audience: Audience{Kind: "broadcast", NetID: 3}, wantErr: `audience kind "broadcast" is not one of net, principals`},
		{name: "uppercase kind", audience: Audience{Kind: "NET", NetID: 3}, wantErr: `audience kind "NET" is not one of net, principals`},

		{name: "net zero id", audience: Audience{Kind: AudienceKindNet}, wantErr: "net_id must be positive, got 0"},
		{name: "net negative id", audience: Audience{Kind: AudienceKindNet, NetID: -3}, wantErr: "net_id must be positive, got -3"},
		{name: "net with principals", audience: Audience{Kind: AudienceKindNet, NetID: 3, PrincipalIDs: []int64{4}}, wantErr: "kind net must not list principal_ids"},

		{name: "principals with net_id", audience: Audience{Kind: AudienceKindPrincipals, NetID: 3, PrincipalIDs: []int64{4}}, wantErr: "kind principals must not carry net_id"},
		{name: "principals nil ids", audience: Audience{Kind: AudienceKindPrincipals}, wantErr: "at least one principal_id"},
		{name: "principals empty ids", audience: Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{}}, wantErr: "at least one principal_id"},
		{name: "principals too many", audience: Audience{Kind: AudienceKindPrincipals, PrincipalIDs: tooMany}, wantErr: "lists 65 principal_ids, more than the maximum 64"},
		{name: "principals zero id", audience: Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{4, 0}}, wantErr: "principal_id must be positive, got 0"},
		{name: "principals negative id", audience: Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{-1}}, wantErr: "principal_id must be positive, got -1"},
		{name: "principals duplicate", audience: Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{4, 7, 4}}, wantErr: "lists principal_id 4 more than once"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.audience.Validate(), tt.wantErr)
		})
	}
}

func TestAudienceKindValid(t *testing.T) {
	for _, k := range []AudienceKind{AudienceKindNet, AudienceKindPrincipals} {
		if !k.Valid() {
			t.Errorf("audience kind %q should be valid", k)
		}
	}
	for _, k := range []AudienceKind{"", "channel", "NET", "principal", "*"} {
		if k.Valid() {
			t.Errorf("audience kind %q should be invalid", k)
		}
	}
}

func TestAudienceNormalize(t *testing.T) {
	tests := []struct {
		name     string
		audience Audience
		authorID int64
		want     Audience
	}{
		{
			name:     "adds the author and sorts",
			audience: Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{7, 4}},
			authorID: 3,
			want:     Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3, 4, 7}},
		},
		{
			name:     "author already present",
			audience: Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{7, 3, 4}},
			authorID: 3,
			want:     Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3, 4, 7}},
		},
		{
			name:     "removes duplicates",
			audience: Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{4, 4, 7, 7}},
			authorID: 3,
			want:     Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3, 4, 7}},
		},
		{
			name:     "already normalized is unchanged",
			audience: Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3, 4, 7}},
			authorID: 3,
			want:     Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3, 4, 7}},
		},
		{
			name:     "unbound author is not added",
			audience: Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{7, 4}},
			authorID: 0,
			want:     Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{4, 7}},
		},
		{
			name:     "net audience is returned as is",
			audience: Audience{Kind: AudienceKindNet, NetID: 3},
			authorID: 3,
			want:     Audience{Kind: AudienceKindNet, NetID: 3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := append([]int64(nil), tt.audience.PrincipalIDs...)
			got := tt.audience.Normalize(tt.authorID)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Normalize(%d) = %+v, want %+v", tt.authorID, got, tt.want)
			}
			if !reflect.DeepEqual(tt.audience.PrincipalIDs, before) {
				t.Errorf("Normalize modified its receiver: %v, was %v", tt.audience.PrincipalIDs, before)
			}
		})
	}
}

// A request's principals audience, normalized by the server, satisfies the
// message's stricter read-back rule. This is the post -> read contract.
func TestAudienceNormalizedSatisfiesMessageV2(t *testing.T) {
	req := PostMessageRequestV2{
		Body:     "hi",
		Audience: &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{7, 4}},
	}
	if err := req.Validate(); err != nil {
		t.Fatalf("request should validate: %v", err)
	}
	m := validMessageV2()
	normalized := req.Audience.Normalize(m.AuthorID)
	m.Audience = &normalized
	if err := m.Validate(); err != nil {
		t.Fatalf("normalized message should validate: %v", err)
	}
	// Without normalization the same audience is rejected.
	m.Audience = req.Audience
	if err := m.Validate(); err == nil {
		t.Fatal("un-normalized request audience validated on a message; want an error")
	}
}

func TestPostMessageRequestV2Validate(t *testing.T) {
	valid := func() PostMessageRequestV2 {
		return PostMessageRequestV2{AuthorID: 3, Body: "hello, conch"}
	}
	tests := []struct {
		name    string
		mutate  func(*PostMessageRequestV2)
		wantErr string
	}{
		{name: "valid", mutate: func(*PostMessageRequestV2) {}},
		{name: "valid without author: server binds it", mutate: func(r *PostMessageRequestV2) { r.AuthorID = 0 }},
		{name: "valid with known payload", mutate: func(r *PostMessageRequestV2) {
			r.Payload = &Payload{Schema: LeviathanTradeSignalV1Name, Data: json.RawMessage(`{"any":"json"}`)}
		}},
		{name: "valid with unknown payload", mutate: func(r *PostMessageRequestV2) {
			r.Payload = &Payload{Schema: "acme.weather_alert.v3", Data: json.RawMessage(`{"x":1}`)}
		}},
		{name: "valid net audience", mutate: func(r *PostMessageRequestV2) {
			r.Audience = &Audience{Kind: AudienceKindNet, NetID: 3}
		}},
		{name: "valid principals audience, unsorted, author omitted", mutate: func(r *PostMessageRequestV2) {
			r.Audience = &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{7, 4}}
		}},
		{name: "valid principals audience including the author", mutate: func(r *PostMessageRequestV2) {
			r.Audience = &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3, 4, 7}}
		}},

		{name: "valid whisper to one id with the author omitted: only the server can tell whether it is the author", mutate: func(r *PostMessageRequestV2) {
			r.AuthorID = 0
			r.Audience = &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3}}
		}},

		{name: "whisper to the named author alone", mutate: func(r *PostMessageRequestV2) {
			r.Audience = &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3}}
		}, wantErr: "at least one principal other than the author"},
		{name: "negative author", mutate: func(r *PostMessageRequestV2) { r.AuthorID = -1 }, wantErr: "author_id must not be negative, got -1"},
		{name: "empty body", mutate: func(r *PostMessageRequestV2) { r.Body = "" }, wantErr: "body is required"},
		{name: "payload bad name", mutate: func(r *PostMessageRequestV2) {
			r.Payload = &Payload{Schema: "no_version", Data: json.RawMessage(`{}`)}
		}, wantErr: "not a valid versioned name"},
		{name: "payload empty data", mutate: func(r *PostMessageRequestV2) {
			r.Payload = &Payload{Schema: LeviathanTradeSignalV1Name}
		}, wantErr: "payload data is required"},
		{name: "payload invalid json", mutate: func(r *PostMessageRequestV2) {
			r.Payload = &Payload{Schema: LeviathanTradeSignalV1Name, Data: json.RawMessage(`{not json`)}
		}, wantErr: "not valid JSON"},
		{name: "unknown audience kind", mutate: func(r *PostMessageRequestV2) {
			r.Audience = &Audience{Kind: "channel"}
		}, wantErr: `audience kind "channel" is not one of net, principals`},
		{name: "net audience without net_id", mutate: func(r *PostMessageRequestV2) {
			r.Audience = &Audience{Kind: AudienceKindNet}
		}, wantErr: "net_id must be positive"},
		{name: "net audience with principals", mutate: func(r *PostMessageRequestV2) {
			r.Audience = &Audience{Kind: AudienceKindNet, NetID: 3, PrincipalIDs: []int64{4}}
		}, wantErr: "must not list principal_ids"},
		{name: "principals audience without ids", mutate: func(r *PostMessageRequestV2) {
			r.Audience = &Audience{Kind: AudienceKindPrincipals}
		}, wantErr: "at least one principal_id"},
		{name: "principals audience with net_id", mutate: func(r *PostMessageRequestV2) {
			r.Audience = &Audience{Kind: AudienceKindPrincipals, NetID: 3, PrincipalIDs: []int64{4}}
		}, wantErr: "must not carry net_id"},
		{name: "principals audience duplicate", mutate: func(r *PostMessageRequestV2) {
			r.Audience = &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{4, 4}}
		}, wantErr: "lists principal_id 4 more than once"},
		{name: "principals audience too many", mutate: func(r *PostMessageRequestV2) {
			ids := make([]int64, MaxAudiencePrincipals+1)
			for i := range ids {
				ids[i] = int64(i + 1)
			}
			r.Audience = &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: ids}
		}, wantErr: "more than the maximum 64"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := valid()
			tt.mutate(&r)
			assertValidation(t, r.Validate(), tt.wantErr)
		})
	}
}

// TestMessageV2UnknownAudienceKindFailsClosed is the fail-closed rule at the
// API level: a v2 document whose audience kind this build does not know must
// fail validation, never be read as channel-wide. An audience a reader does
// not understand must never widen delivery.
func TestMessageV2UnknownAudienceKindFailsClosed(t *testing.T) {
	docs := []struct {
		name string
		doc  string
	}{
		{"future kind", `{"schema":"conch.message.v2","id":1,"channel_id":2,"author_id":3,"created_at":"2026-10-08T12:00:00.000Z","body":"hi","audience":{"kind":"squad","squad_id":9}}`},
		{"channel kind", `{"schema":"conch.message.v2","id":1,"channel_id":2,"author_id":3,"created_at":"2026-10-08T12:00:00.000Z","body":"hi","audience":{"kind":"channel"}}`},
		{"empty audience object", `{"schema":"conch.message.v2","id":1,"channel_id":2,"author_id":3,"created_at":"2026-10-08T12:00:00.000Z","body":"hi","audience":{}}`},
	}
	for _, tt := range docs {
		t.Run(tt.name, func(t *testing.T) {
			var m MessageV2
			if err := json.Unmarshal([]byte(tt.doc), &m); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if m.Audience == nil {
				t.Fatal("audience was dropped on decode; a scoped message must never decode as channel-wide")
			}
			err := m.Validate()
			if err == nil {
				t.Fatal("Validate() = nil; want an error for an unknown audience kind")
			}
			if !strings.Contains(err.Error(), "audience kind") {
				t.Errorf("Validate() = %q; want the audience-kind error", err)
			}
		})
	}
}

// TestMessageV1RejectsV2Document: a v1 decoder handed a v2 document fails on
// the schema name, whether or not it dropped the audience on decode. A v1
// reader must not silently render a v2 message.
func TestMessageV1RejectsV2Document(t *testing.T) {
	for _, file := range []string{"message-v2.json", "message-v2-net.json", "message-v2-whisper.json"} {
		t.Run(file, func(t *testing.T) {
			golden, err := os.ReadFile(filepath.Join("testdata", file)) // #nosec G304 -- fixture name comes from this test's own list
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			var m MessageV1
			if err := json.Unmarshal(golden, &m); err != nil {
				t.Fatalf("decode: %v", err)
			}
			err = m.Validate()
			if err == nil {
				t.Fatal("MessageV1.Validate() = nil on a conch.message.v2 document; want a schema-name error")
			}
			if !strings.Contains(err.Error(), `message schema must be "conch.message.v1", got "conch.message.v2"`) {
				t.Errorf("Validate() = %q; want the schema-name error", err)
			}
		})
	}
}

func TestMessageV1V2ConversionIsLossless(t *testing.T) {
	v1s := []struct {
		name string
		file string
	}{
		{"plain", "message-v1.json"},
		{"with payload", "message-v1-with-payload.json"},
		{"unknown payload", "message-v1-unknown-payload.json"},
	}
	for _, tt := range v1s {
		t.Run(tt.name, func(t *testing.T) {
			golden, err := os.ReadFile(filepath.Join("testdata", tt.file)) // #nosec G304 -- fixture name comes from this test's own table
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			var v1 MessageV1
			if err := json.Unmarshal(golden, &v1); err != nil {
				t.Fatalf("decode: %v", err)
			}

			v2 := MessageV2FromV1(v1)
			if err := v2.Validate(); err != nil {
				t.Fatalf("converted v2 should validate: %v", err)
			}
			if v2.Schema != MessageSchemaV2 {
				t.Errorf("schema = %q, want %q", v2.Schema, MessageSchemaV2)
			}
			if v2.Audience != nil {
				t.Errorf("audience = %+v, want nil (channel-wide)", v2.Audience)
			}
			// The v2 wire form is the v1 wire form with only the schema name changed.
			encoded, err := json.MarshalIndent(v2, "", "  ")
			if err != nil {
				t.Fatalf("encode v2: %v", err)
			}
			encoded = append(encoded, '\n')
			want := bytes.Replace(golden, []byte(MessageSchemaV1), []byte(MessageSchemaV2), 1)
			if !bytes.Equal(encoded, want) {
				t.Errorf("v2 wire form differs from v1 beyond the schema name\ngot:\n%s\nwant:\n%s", encoded, want)
			}

			back, err := MessageV1FromV2(v2)
			if err != nil {
				t.Fatalf("convert back: %v", err)
			}
			if !reflect.DeepEqual(back, v1) {
				t.Errorf("round trip changed the message\ngot:  %+v\nwant: %+v", back, v1)
			}
		})
	}
}

// A scoped message has no v1 form: downgrading would present a whisper as an
// open message.
func TestMessageV1FromV2RejectsScopedMessages(t *testing.T) {
	for _, a := range []Audience{
		{Kind: AudienceKindNet, NetID: 3},
		{Kind: AudienceKindPrincipals, PrincipalIDs: []int64{3, 4}},
		{Kind: "unknown"},
	} {
		m := validMessageV2()
		m.Audience = &a
		if _, err := MessageV1FromV2(m); err == nil {
			t.Errorf("MessageV1FromV2 with audience %+v = nil error; want a rejection", a)
		}
	}
}

func TestListMessagesResponseV2EmptyEncodesArray(t *testing.T) {
	got, err := json.Marshal(ListMessagesResponseV2{Messages: []MessageV2{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"messages":[]}` {
		t.Fatalf("got %s", got)
	}
}
