package schema

import "testing"

func TestErrorCodeRateLimited(t *testing.T) {
	t.Parallel()
	// The code is a stable wire value: clients match on it.
	if ErrorCodeRateLimited != "rate_limited" {
		t.Fatalf("ErrorCodeRateLimited = %q, want %q", ErrorCodeRateLimited, "rate_limited")
	}
}
