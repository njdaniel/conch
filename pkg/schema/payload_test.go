package schema

import (
	"encoding/json"
	"testing"
)

func TestValidPayloadName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "canonical exemplar", in: "leviathan.trade_signal.v1", want: true},
		{name: "single segment", in: "ping.v1", want: true},
		{name: "high version", in: "acme.weather_alert.v42", want: true},
		{name: "no version", in: "leviathan.trade_signal", want: false},
		{name: "zero version", in: "leviathan.trade_signal.v0", want: false},
		{name: "uppercase", in: "Leviathan.trade_signal.v1", want: false},
		{name: "trailing dot", in: "leviathan.trade_signal.v1.", want: false},
		{name: "empty", in: "", want: false},
		{name: "version only", in: "v1", want: false},
		{name: "segment starts with digit", in: "1eviathan.trade.v1", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidPayloadName(tt.in); got != tt.want {
				t.Errorf("ValidPayloadName(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestRegisteredPayloadsIncludesExemplar(t *testing.T) {
	names := RegisteredPayloads()
	found := false
	for _, n := range names {
		if n == LeviathanTradeSignalV1Name {
			found = true
		}
	}
	if !found {
		t.Errorf("RegisteredPayloads() = %v, missing %q", names, LeviathanTradeSignalV1Name)
	}
	if _, ok := LookupPayload(LeviathanTradeSignalV1Name); !ok {
		t.Errorf("LookupPayload(%q) not found", LeviathanTradeSignalV1Name)
	}
}

func TestPayloadDecodeKnown(t *testing.T) {
	p := Payload{
		Schema: LeviathanTradeSignalV1Name,
		Data:   json.RawMessage(`{"symbol":"BTC-USD","side":"buy","quantity":"0.5","confidence":0.82,"strategy":"momentum","issued_at":"2026-07-13T12:34:56.500Z"}`),
	}
	v, known, err := p.Decode()
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !known {
		t.Fatal("expected known payload")
	}
	sig, ok := v.(*LeviathanTradeSignalV1)
	if !ok {
		t.Fatalf("Decode returned %T, want *LeviathanTradeSignalV1", v)
	}
	if sig.Symbol != "BTC-USD" || sig.Side != TradeSideBuy {
		t.Errorf("decoded signal mismatch: %+v", sig)
	}
}

func TestPayloadDecodeUnknownIsNotError(t *testing.T) {
	p := Payload{
		Schema: "acme.weather_alert.v3",
		Data:   json.RawMessage(`{"region":"pnw"}`),
	}
	if p.Known() {
		t.Fatal("acme.weather_alert.v3 must not be registered")
	}
	v, known, err := p.Decode()
	if err != nil {
		t.Fatalf("Decode of unknown schema must not error, got %v", err)
	}
	if known || v != nil {
		t.Errorf("unknown decode = (%v, %v), want (nil, false)", v, known)
	}
}

func TestPayloadDecodeKnownRejectsUnknownFields(t *testing.T) {
	p := Payload{
		Schema: LeviathanTradeSignalV1Name,
		Data:   json.RawMessage(`{"symbol":"BTC-USD","side":"buy","quantity":"0.5","confidence":0.82,"strategy":"momentum","issued_at":"2026-07-13T12:34:56.500Z","surprise":true}`),
	}
	if _, _, err := p.Decode(); err == nil {
		t.Fatal("expected error decoding a registered payload with an unknown field")
	}
}
