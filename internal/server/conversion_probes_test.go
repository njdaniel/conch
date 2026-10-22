package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The guests behind one address share one conversion name-probe allowance
// as well as each having their own (design §8: five an hour an address), so
// cycling guests gains nothing; it is not the one POST /v1/join/account
// spends, which neither touches nor is touched by it.
func TestConversionNameProbesAreLimitedPerAddress(t *testing.T) {
	f := newLoginFixture(t, Config{})
	f.mustPutJoin(t, openBoth("red"))
	l := looseLoginLimits()
	l.nameProbesPerAddress = inviteLimit{"name_probes_per_address", 1, time.Hour}
	l.conversionNameProbesPerAddress = inviteLimit{"conversion_name_probes_per_address", 2, time.Hour}
	f.srv.setLoginLimits(l)
	const remote = "198.51.100.96:1"
	convertFrom := func(token, name, from string) *httptest.ResponseRecorder {
		return f.send(t, "POST", "/v1/guest/convert", token, inviteJSON(t, schema.ConvertGuestRequestV1{Name: name, Password: testJoinPassword}), from)
	}
	// An anonymous caller spends the address's account probes; the
	// conversions' are untouched.
	assertErrorBody(t, f.joinAccount(t, "alice", remote), http.StatusConflict, schema.ErrorCodePrincipalExists)
	assertErrorBody(t, f.joinAccount(t, "a-free-name", remote), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	a := mustJoined(t, f.joinGuest(t, "", remote))
	b := mustJoined(t, f.joinGuest(t, "", remote))
	c := mustJoined(t, f.joinGuest(t, "", remote))
	assertErrorBody(t, convertFrom(a.Token, "alice", remote), http.StatusConflict, schema.ErrorCodePrincipalExists)
	assertErrorBody(t, convertFrom(b.Token, "alice", remote), http.StatusConflict, schema.ErrorCodePrincipalExists)
	// The address's two are spent between them: a third guest, with probes
	// of its own left, is refused before any name is looked at.
	assertErrorBody(t, convertFrom(c.Token, "member", remote), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	// Another address's guests are not affected, and the conversions spent
	// nothing of that address's account allowance.
	const other = "198.51.100.97:1"
	d := mustJoined(t, f.joinGuest(t, "", other))
	mustConverted(t, convertFrom(d.Token, "member", other))
	assertErrorBody(t, f.joinAccount(t, "alice", other), http.StatusConflict, schema.ErrorCodePrincipalExists)
}
