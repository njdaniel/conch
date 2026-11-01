package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"

	"github.com/njdaniel/conch/pkg/schema"
)

// Create an account during a visit (design §3.3, §9.7): what a guest must
// give, what refuses it without leaving a partial account, the races that
// must end in one identity, and what the member it becomes keeps and does
// not gain.

// convertPassword is the password the plaza's converting guests choose.
const convertPassword = "a new passphrase for the account"

// stillTheGuest requires that g's credential still answers as g's visit: a
// refused conversion left it as it was.
func (w *world) stillTheGuest(g *member, after string) error {
	var who schema.WhoAmIResponseV1
	if err := g.api.call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return fmt.Errorf("after %s: %w", after, err)
	}
	if who.IdentityClass != schema.IdentityGuest || who.ID != g.id || who.Name != g.guest.Handle || who.Guest == nil || who.Guest.State != schema.GuestStateActive {
		return fmt.Errorf("after %s %s is %q (%s), want the live guest %q", after, g.guest.Handle, who.Name, who.IdentityClass, g.guest.Handle)
	}
	return nil
}

// convertRefused has g try to create an account, and requires the refusal
// want and that g is still the guest it was.
func (w *world) convertRefused(g *member, want answer, when string) error {
	return w.convertRefusedWith(g, schema.ConvertGuestRequestV1{Name: w.pubName(fmt.Sprintf("member-%d", g.id)), Password: convertPassword}, want, when)
}

func (w *world) convertRefusedWith(g *member, req schema.ConvertGuestRequestV1, want answer, when string) error {
	status, body, err := g.api.do(http.MethodPost, "/v1/guest/convert", req)
	if err != nil {
		return err
	}
	// The refusal may name the password rule's bounds; it names nothing of
	// anyone's, which the marker scan checks.
	if _, err := checkAnswer(result{status: status, body: body}, want); err != nil {
		return fmt.Errorf("a conversion %s: %w", when, err)
	}
	if leaks := w.leaks(body, asked{}); len(leaks) > 0 {
		return fmt.Errorf("a conversion %s: the refusal holds %v", when, leaks)
	}
	return w.stillTheGuest(g, "a conversion "+when)
}

// converted checks a conversion's answer: the same principal under its new
// name, with a member's credential that works, and the guest's that no
// longer does. It returns the member.
func (w *world) converted(g *member, name string, resp schema.ConvertGuestResponseV1) (*member, error) {
	w.h.secret(name+"'s credential", resp.Token)
	if resp.Principal.ID != g.id || resp.Principal.Name != name {
		return nil, fmt.Errorf("the conversion answered principal %d %q, want %d %q", resp.Principal.ID, resp.Principal.Name, g.id, name)
	}
	m := &member{name: name, id: g.id, token: resp.Token, credID: resp.Credential.ID, addr: g.addr, password: true}
	m.api = w.as(m)
	var who schema.WhoAmIResponseV1
	if err := m.api.call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return nil, err
	}
	if who.IdentityClass != schema.IdentityMember || who.ID != g.id || who.Guest != nil || who.MayInvite {
		return nil, fmt.Errorf("the member's whoami is %q (%s), visit %+v, may invite %v", who.Name, who.IdentityClass, who.Guest, who.MayInvite)
	}
	if status, _, err := g.api.do(http.MethodGet, "/v1/whoami", nil); err != nil {
		return nil, err
	} else if status != http.StatusUnauthorized {
		return nil, fmt.Errorf("the guest's credential after the conversion: status %d, want 401", status)
	}
	return m, nil
}

// conversionNeedsPermit: at an address that offers no account a guest
// cannot create one by itself (design §9.2), and can with the operator's
// permit, made for it by name; the permit makes nothing else.
func (w *world) conversionNeedsPermit() error {
	g, err := w.joinGuest("Lee", nil)
	if err != nil {
		return err
	}
	if err := w.convertRefused(g, answer{http.StatusForbidden, schema.ErrorCodeConversionNotAllowed}, "at an address closed to accounts"); err != nil {
		return err
	}
	var permit schema.CreateSignInLinkResponseV1
	if err := w.op.call(http.MethodPost, fmt.Sprintf("/v1/guests/%d/conversion-permit", g.id), schema.CreateConversionPermitRequestV1{}, &permit); err != nil {
		return err
	}
	w.h.secret("a conversion permit", permit.Code)
	if permit.SignInLink.Purpose != schema.SignInLinkPurposeConversion {
		return fmt.Errorf("the permit is a link of purpose %q", permit.SignInLink.Purpose)
	}
	// Another guest cannot use it.
	if err := w.convertRefusedWith(w.p.nick2, schema.ConvertGuestRequestV1{Name: w.pubName("not-hers"), Password: convertPassword, Permit: permit.Code},
		answer{http.StatusNotFound, schema.ErrorCodeConversionPermitInvalid}, "with another guest's permit"); err != nil {
		return err
	}
	name := w.pubName("lee")
	var resp schema.ConvertGuestResponseV1
	if err := g.api.call(http.MethodPost, "/v1/guest/convert", schema.ConvertGuestRequestV1{Name: name, Password: convertPassword, Permit: permit.Code}, &resp); err != nil {
		return err
	}
	_, err = w.converted(g, name, resp)
	return err
}

// convertSam is the conversion of §9.7: Sam, a guest of the bare address
// with accounts open, posts, is refused for every reason that leaves nothing
// behind, then asks twice at once; one identity results. A lost answer is
// recovered by signing in with the password. The member keeps the visit's
// bound and gains nothing; the post made as a guest keeps the guest's
// attribution, through the member's hiding too. It returns the member.
func (w *world) convertSam() (*member, int64, error) {
	p := w.p
	sam := p.sam
	var asGuest schema.PostMessageResponseV2
	if err := sam.api.call(http.MethodPost, "/v2/channels/"+url.PathEscape(p.channel)+"/messages", schema.PostMessageRequestV2{Body: w.pubName("from-sam")}, &asGuest); err != nil {
		return nil, 0, err
	}
	name := w.pubName("sam")
	w.h.secret("a password", name+" is my password")
	for _, c := range []struct {
		when string
		req  schema.ConvertGuestRequestV1
		want answer
	}{
		{"under a member's name", schema.ConvertGuestRequestV1{Name: p.pia.name, Password: convertPassword}, answer{http.StatusConflict, schema.ErrorCodePrincipalExists}},
		{"under the reserved guest prefix", schema.ConvertGuestRequestV1{Name: "guest-sam", Password: convertPassword}, answer{http.StatusBadRequest, "invalid_request"}},
		{"under a name that only looks like a guest's", schema.ConvertGuestRequestV1{Name: "gue\u0455t-sam", Password: convertPassword}, answer{http.StatusConflict, schema.ErrorCodePrincipalExists}},
		{"with no password", schema.ConvertGuestRequestV1{Name: name}, answer{http.StatusBadRequest, "invalid_request"}},
		{"with a common password", schema.ConvertGuestRequestV1{Name: name, Password: commonPassword}, answer{http.StatusBadRequest, schema.ErrorCodePasswordUnacceptable}},
		{"with a password that holds the name", schema.ConvertGuestRequestV1{Name: name, Password: name + " is my password"}, answer{http.StatusBadRequest, schema.ErrorCodePasswordUnacceptable}},
	} {
		if err := w.convertRefusedWith(sam, c.req, c.want, c.when); err != nil {
			return nil, 0, err
		}
	}

	// Twice at once: exactly one conversion.
	type answered struct {
		status int
		body   []byte
		err    error
	}
	var wg sync.WaitGroup
	got := make([]answered, 2)
	start := make(chan struct{})
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			s, b, err := sam.api.do(http.MethodPost, "/v1/guest/convert", schema.ConvertGuestRequestV1{Name: name, Password: convertPassword})
			got[i] = answered{s, b, err}
		}(i)
	}
	close(start)
	wg.Wait()
	var won []answered
	for _, a := range got {
		if a.err != nil {
			return nil, 0, a.err
		}
		if a.status == http.StatusOK {
			won = append(won, a)
			continue
		}
		if a.status != http.StatusTooManyRequests && a.status != http.StatusUnauthorized && a.status != http.StatusConflict {
			return nil, 0, fmt.Errorf("the second of two conversions at once answered %d %s", a.status, w.h.redact(string(a.body)))
		}
	}
	if len(won) != 1 {
		return nil, 0, fmt.Errorf("two conversions at once: %d succeeded, want exactly one", len(won))
	}
	var resp schema.ConvertGuestResponseV1
	if err := json.Unmarshal(won[0].body, &resp); err != nil {
		return nil, 0, err
	}
	member, err := w.converted(sam, name, resp)
	if err != nil {
		return nil, 0, err
	}

	// The answer could have been lost: the password signs the account in.
	var login schema.LoginResponseV1
	if err := w.newcomer("Sam on another device").call(http.MethodPost, "/v1/login", schema.LoginRequestV1{Name: name, Password: convertPassword}, &login); err != nil {
		return nil, 0, err
	}
	w.h.secret("Sam's sign-in", login.Token)
	if login.Next != schema.LoginNextSignedIn || login.Principal == nil || login.Principal.ID != sam.id {
		return nil, 0, fmt.Errorf("signing in after the conversion answered %s for %+v", login.Next, login.Principal)
	}

	// The post Sam made as a guest is still the guest's; one made now is
	// the member's.
	if err := w.checkAttribution(p.pia.api, asGuest.Message.ID, *sam.guest); err != nil {
		return nil, 0, err
	}
	var asMember schema.PostMessageResponseV2
	if err := member.api.call(http.MethodPost, "/v2/channels/"+url.PathEscape(p.channel)+"/messages", schema.PostMessageRequestV2{Body: w.pubName("from-sam-the-member")}, &asMember); err != nil {
		return nil, 0, err
	}
	if m, ok, err := w.messageIn(p.pia.api, asMember.Message.ID); err != nil {
		return nil, 0, err
	} else if !ok || m.GuestAuthor != nil || m.AuthorID != sam.id {
		return nil, 0, fmt.Errorf("the member's post reads as %+v by %d, want no guest attribution, by %d", m.GuestAuthor, m.AuthorID, sam.id)
	}

	// Nothing gained: no invites, the plaza alone.
	status, body, err := member.api.do(http.MethodPost, "/v1/invites", schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{p.channel}})
	if err != nil {
		return nil, 0, err
	}
	if err := checkRefusal(result{status: status, body: body}, answer{http.StatusForbidden, schema.ErrorCodeInviteNotAllowed}, asked{}); err != nil {
		return nil, 0, fmt.Errorf("the member Sam became invites: %w", err)
	}
	if ids, err := channelIDs(member.api); err != nil {
		return nil, 0, err
	} else if len(ids) != 1 || ids[0] != p.chanID {
		return nil, 0, fmt.Errorf("the member Sam became is in channels %v, want the plaza alone", ids)
	}
	return member, asGuest.Message.ID, nil
}

// hideKeepsAttribution: hiding the member a guest became renames the member,
// and the post made during the visit keeps the guest's attribution
// (design §4.4).
func (w *world) hideKeepsAttribution(member *member, guestPost int64, g schema.GuestV1) error {
	if err := w.op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/hide", member.id), nil, nil); err != nil {
		return err
	}
	if err := w.voicePermissionHiddenIsGhost(member.id); err != nil {
		return err
	}
	return w.checkAttribution(w.p.pia.api, guestPost, g)
}

// logoutRace: a guest creates an account and ends its visit at once. One
// wins: either the account exists, with its password, or the visit ended
// and no account was made.
func (w *world) logoutRace() (string, error) {
	g, err := w.joinGuest("Rae", nil)
	if err != nil {
		return "", err
	}
	name := w.pubName("rae")
	var wg sync.WaitGroup
	var convStatus, endStatus int
	var convBody []byte
	var convErr, endErr error
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		convStatus, convBody, convErr = g.api.do(http.MethodPost, "/v1/guest/convert", schema.ConvertGuestRequestV1{Name: name, Password: convertPassword})
	}()
	go func() {
		defer wg.Done()
		<-start
		endStatus, _, endErr = g.api.do(http.MethodPost, "/v1/guest/end", nil)
	}()
	close(start)
	wg.Wait()
	if convErr != nil {
		return "", convErr
	}
	if endErr != nil {
		return "", endErr
	}
	if tok := tokenOf(convBody); tok != "" {
		w.h.secret("Rae's credential", tok)
	}
	if (convStatus == http.StatusOK) == (endStatus == http.StatusOK) {
		return "", fmt.Errorf("a conversion and an End visit at once answered %d and %d: exactly one must win", convStatus, endStatus)
	}
	status, body, err := w.newcomer("Rae signing in").do(http.MethodPost, "/v1/login", schema.LoginRequestV1{Name: name, Password: convertPassword})
	if err != nil {
		return "", err
	}
	if tok := tokenOf(body); tok != "" {
		w.h.secret("Rae's sign-in", tok)
	}
	if convStatus == http.StatusOK {
		if status != http.StatusOK {
			return "", fmt.Errorf("the conversion won the race, and signing in answers %d", status)
		}
		return "the conversion won; the account signs in", nil
	}
	if status != http.StatusUnauthorized {
		return "", fmt.Errorf("when End visit won the race, signing in as the name the conversion asked for answers %d, want 401: a partial account", status)
	}
	return "End visit won; no account was made", nil
}
