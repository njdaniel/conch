package schema

import (
	"encoding/json"
	"testing"
)

func TestMessageV1APIGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "post request", file: "post-message-request-v1.json", new: func() any { return &PostMessageRequestV1{} }},
		{name: "post request with payload", file: "post-message-request-v1-with-payload.json", new: func() any { return &PostMessageRequestV1{} }},
		{name: "post response", file: "post-message-response-v1.json", new: func() any { return &PostMessageResponseV1{} }},
		{name: "list response", file: "list-messages-response-v1.json", new: func() any { return &ListMessagesResponseV1{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

func TestPostMessageRequestV1Validate(t *testing.T) {
	valid := func() PostMessageRequestV1 {
		return PostMessageRequestV1{
			AuthorID: 3,
			Body:     "hello, conch",
		}
	}
	tests := []struct {
		name    string
		mutate  func(*PostMessageRequestV1)
		wantErr bool
	}{
		{name: "valid", mutate: func(*PostMessageRequestV1) {}, wantErr: false},
		{name: "valid with known payload", mutate: func(r *PostMessageRequestV1) {
			r.Payload = &Payload{Schema: LeviathanTradeSignalV1Name, Data: json.RawMessage(`{"any":"json"}`)}
		}, wantErr: false},
		{name: "valid with unknown payload", mutate: func(r *PostMessageRequestV1) {
			r.Payload = &Payload{Schema: "acme.weather_alert.v3", Data: json.RawMessage(`{"x":1}`)}
		}, wantErr: false},
		{name: "zero author", mutate: func(r *PostMessageRequestV1) { r.AuthorID = 0 }, wantErr: true},
		{name: "negative author", mutate: func(r *PostMessageRequestV1) { r.AuthorID = -1 }, wantErr: true},
		{name: "empty body", mutate: func(r *PostMessageRequestV1) { r.Body = "" }, wantErr: true},
		{name: "payload bad name", mutate: func(r *PostMessageRequestV1) {
			r.Payload = &Payload{Schema: "no_version", Data: json.RawMessage(`{}`)}
		}, wantErr: true},
		{name: "payload empty data", mutate: func(r *PostMessageRequestV1) {
			r.Payload = &Payload{Schema: LeviathanTradeSignalV1Name}
		}, wantErr: true},
		{name: "payload invalid json", mutate: func(r *PostMessageRequestV1) {
			r.Payload = &Payload{Schema: LeviathanTradeSignalV1Name, Data: json.RawMessage(`{not json`)}
		}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := valid()
			tt.mutate(&r)
			err := r.Validate()
			if tt.wantErr != (err != nil) {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
