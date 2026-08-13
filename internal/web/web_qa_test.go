package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"autodeploy/internal/auth"
)

type logoutRepositorySpy struct {
	*repositoryStub
	revoked bool
}

func (s *logoutRepositorySpy) RevokeSession(_ context.Context, _ auth.SessionToken, _ auth.Principal, _ string) error {
	s.revoked = true
	return nil
}

func TestQAValidLogoutFormRevokesSession(t *testing.T) {
	principal := auth.Principal{UserID: "user-1", OwnerID: "owner-1", Role: auth.RoleOwner, AuthRevision: 1, SessionID: "session-1"}
	base := &repositoryStub{principal: principal}
	h := testHandler(t, base)
	spy := &logoutRepositorySpy{repositoryStub: base}
	h.repository = spy
	csrf, err := auth.NewSessionCSRF(h.config.CSRFKeys, principal.SessionID, h.config.PublicOrigin)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"csrf": {csrf}}
	r := httptest.NewRequest(http.MethodPost, "https://admin.example.test/logout", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Origin", "https://admin.example.test")
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "v1.AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"})
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, r)

	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" || !spy.revoked {
		t.Fatalf("valid logout was not completed: status=%d location=%q revoked=%v", rr.Code, rr.Header().Get("Location"), spy.revoked)
	}
}

func TestQAUntrustedPeerIgnoresForwardingHeader(t *testing.T) {
	h := testHandler(t, &repositoryStub{})
	r := &http.Request{RemoteAddr: "192.0.2.10:443", Header: http.Header{"X-Forwarded-For": {"198.51.100.8"}}}

	got, err := h.clientIP(r)

	if err != nil || got != netip.MustParseAddr("192.0.2.10") {
		t.Fatalf("untrusted peer forwarding header was not ignored: ip=%v err=%v", got, err)
	}
}

func TestQATrustedProxySelectsFirstUntrustedFromRight(t *testing.T) {
	h := testHandler(t, &repositoryStub{})
	h.config.TrustedProxyCIDRs = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	r := &http.Request{RemoteAddr: "192.0.2.3:443", Header: http.Header{"X-Forwarded-For": {"203.0.113.9,198.51.100.7,192.0.2.2"}}}

	got, err := h.clientIP(r)

	if err != nil || got != netip.MustParseAddr("198.51.100.7") {
		t.Fatalf("right-to-left proxy selection failed: ip=%v err=%v", got, err)
	}
}

func TestQATrustedMappedIPv4PeerIsCanonicalized(t *testing.T) {
	h := testHandler(t, &repositoryStub{})
	h.config.TrustedProxyCIDRs = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	r := &http.Request{RemoteAddr: "[::ffff:192.0.2.3]:443", Header: http.Header{"X-Forwarded-For": {"198.51.100.7"}}}

	got, err := h.clientIP(r)

	if err != nil || got != netip.MustParseAddr("198.51.100.7") {
		t.Fatalf("mapped IPv4 peer was not trusted after canonicalization: ip=%v err=%v", got, err)
	}
}

func TestQAEmptyOriginDoesNotFallBackToReferer(t *testing.T) {
	h := testHandler(t, &repositoryStub{})
	r := &http.Request{Header: http.Header{"Origin": {""}, "Referer": {"https://admin.example.test/login"}}}

	if err := h.provenance(r); err == nil {
		t.Fatal("present empty Origin was accepted via Referer fallback")
	}
}
