package schema

import (
	"testing"
	"time"
)

func TestLeviathanTradeSignalV1GoldenFixture(t *testing.T) {
	assertGoldenFixture(t, "leviathan-trade-signal-v1.json", func() any { return &LeviathanTradeSignalV1{} })
}

func TestLeviathanTradeSignalV1Validate(t *testing.T) {
	valid := func() LeviathanTradeSignalV1 {
		return LeviathanTradeSignalV1{
			Symbol:     "BTC-USD",
			Side:       TradeSideBuy,
			Quantity:   "0.5",
			LimitPrice: "65000.00",
			Confidence: 0.82,
			Strategy:   "momentum",
			IssuedAt:   NewTimestamp(time.Date(2026, 7, 13, 12, 34, 56, 0, time.UTC)),
		}
	}
	tests := []struct {
		name    string
		mutate  func(*LeviathanTradeSignalV1)
		wantErr bool
	}{
		{name: "valid", mutate: func(*LeviathanTradeSignalV1) {}, wantErr: false},
		{name: "market order (no limit)", mutate: func(s *LeviathanTradeSignalV1) { s.LimitPrice = "" }, wantErr: false},
		{name: "sell side", mutate: func(s *LeviathanTradeSignalV1) { s.Side = TradeSideSell }, wantErr: false},
		{name: "empty symbol", mutate: func(s *LeviathanTradeSignalV1) { s.Symbol = "" }, wantErr: true},
		{name: "bad side", mutate: func(s *LeviathanTradeSignalV1) { s.Side = "hold" }, wantErr: true},
		{name: "empty quantity", mutate: func(s *LeviathanTradeSignalV1) { s.Quantity = "" }, wantErr: true},
		{name: "confidence below range", mutate: func(s *LeviathanTradeSignalV1) { s.Confidence = -0.1 }, wantErr: true},
		{name: "confidence above range", mutate: func(s *LeviathanTradeSignalV1) { s.Confidence = 1.1 }, wantErr: true},
		{name: "empty strategy", mutate: func(s *LeviathanTradeSignalV1) { s.Strategy = "" }, wantErr: true},
		{name: "zero issued_at", mutate: func(s *LeviathanTradeSignalV1) { s.IssuedAt = Timestamp{} }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := valid()
			tt.mutate(&s)
			err := s.Validate()
			if tt.wantErr != (err != nil) {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
