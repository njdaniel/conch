package schema

import (
	"errors"
	"fmt"
)

// LeviathanTradeSignalV1Name is the registered versioned name of the Leviathan
// trade-signal payload — the D8 exemplar of a declared, versioned machine
// payload.
const LeviathanTradeSignalV1Name = "leviathan.trade_signal.v1"

// TradeSide is the direction of a trade signal.
type TradeSide string

// Trade sides.
const (
	TradeSideBuy  TradeSide = "buy"
	TradeSideSell TradeSide = "sell"
)

// LeviathanTradeSignalV1 is a draft exemplar machine payload: a trading signal
// emitted by the Leviathan agent. Quantities and prices are decimal strings,
// never floats, so no binary floating-point drift reaches the wire; confidence
// is a plain [0,1] ratio.
type LeviathanTradeSignalV1 struct {
	// Symbol is the instrument, e.g. "BTC-USD".
	Symbol string `json:"symbol"`
	// Side is "buy" or "sell".
	Side TradeSide `json:"side"`
	// Quantity is the order size as a decimal string, e.g. "0.5".
	Quantity string `json:"quantity"`
	// LimitPrice is the optional limit price as a decimal string; empty means a
	// market order.
	LimitPrice string `json:"limit_price,omitempty"`
	// Confidence is the model's confidence in the signal, 0.0 to 1.0.
	Confidence float64 `json:"confidence"`
	// Strategy names the strategy that produced the signal.
	Strategy string `json:"strategy"`
	// IssuedAt is when the signal was generated (UTC, ms precision).
	IssuedAt Timestamp `json:"issued_at"`
}

// Validate reports whether the trade signal is well-formed.
func (s LeviathanTradeSignalV1) Validate() error {
	if s.Symbol == "" {
		return errors.New("schema: trade signal symbol is required")
	}
	if s.Side != TradeSideBuy && s.Side != TradeSideSell {
		return fmt.Errorf("schema: trade signal side must be %q or %q, got %q", TradeSideBuy, TradeSideSell, s.Side)
	}
	if s.Quantity == "" {
		return errors.New("schema: trade signal quantity is required")
	}
	if s.Confidence != s.Confidence || s.Confidence < 0 || s.Confidence > 1 {
		return fmt.Errorf("schema: trade signal confidence must be in [0,1], got %v", s.Confidence)
	}
	if s.Strategy == "" {
		return errors.New("schema: trade signal strategy is required")
	}
	if s.IssuedAt.IsZero() {
		return errors.New("schema: trade signal issued_at is required")
	}
	return nil
}

func init() {
	RegisterPayload(PayloadSchema{
		Name: LeviathanTradeSignalV1Name,
		New:  func() any { return new(LeviathanTradeSignalV1) },
	})
}
