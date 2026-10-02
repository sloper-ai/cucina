// SPDX-License-Identifier: FSL-1.1-ALv2

package sts

import (
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/sloper-ai/cucina/internal/auth"
	"github.com/sloper-ai/cucina/internal/keys"
)

// RFC 8693 / RFC 6749 constants.
const (
	GrantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	maxFormBytes           = 64 << 10
)

// tokenResponse is the RFC 8693 success body (contracts §5.1).
type tokenResponse struct {
	AccessToken     string `json:"access_token"`
	IssuedTokenType string `json:"issued_token_type"`
	TokenType       string `json:"token_type"`
	ExpiresIn       int64  `json:"expires_in"`
}

// errorResponse is the RFC 6749 §5.2 error body. Descriptions never echo token material.
type errorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// Extra error codes beyond auth.Code.
const (
	codeUnsupportedGrantType = "unsupported_grant_type"
	codeSlowDown             = "slow_down" // OAuth extensions error registry (RFC 8628)
)

// exchangeAudit is one audit record. It deliberately has no field that could hold a token.
type exchangeAudit struct {
	// reason is the client-facing description; detail an internal cause (audit only).
	remote, issuer, subject, result, reason, detail, sid, jti string
	policies, grants                                          []string
	scopes                                                    keys.Scopes
	ttl                                                       int64
}

func (s *Server) audit(a exchangeAudit) {
	attrs := []any{
		slog.String("event", "sts.exchange"), slog.String("result", a.result),
		slog.String("issuer", a.issuer), slog.String("remote", a.remote),
	}
	if a.reason != "" {
		attrs = append(attrs, slog.String("reason", a.reason))
	}
	if a.detail != "" {
		attrs = append(attrs, slog.String("detail", a.detail))
	}
	if a.subject != "" {
		attrs = append(attrs, slog.String("subject", a.subject))
	}
	if len(a.policies) > 0 {
		attrs = append(attrs, slog.Any("policies", a.policies), slog.Any("grants", a.grants))
	}
	if a.result == "ok" {
		attrs = append(attrs, slog.Any("scopes", a.scopes), slog.String("sid", a.sid),
			slog.String("jti", a.jti), slog.Int64("ttl_seconds", a.ttl))
	}
	s.d.Audit.Info("token exchange", attrs...)
}

func (s *Server) fail(w http.ResponseWriter, a exchangeAudit, status int, code, desc string) {
	if a.issuer == "" {
		a.issuer = auth.IssuerUnknown
	}
	a.result, a.reason = code, desc
	s.audit(a)
	s.metrics.exchanges.WithLabelValues(a.issuer, code).Inc()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "60")
	}
	writeJSON(w, status, errorResponse{Error: code, ErrorDescription: desc})
}

func statusFor(code auth.Code) int {
	switch code {
	case auth.CodeAccessDenied:
		return http.StatusForbidden
	case auth.CodeServerError:
		return http.StatusInternalServerError
	}
	return http.StatusBadRequest
}

// clientIP returns the peer address, or the first untrusted X-Forwarded-For hop when the
// peer is a trusted proxy.
func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	trusted := func(a netip.Addr) bool {
		for _, p := range s.d.TrustedProxies {
			if p.Contains(a.Unmap()) {
				return true
			}
		}
		return false
	}
	if !trusted(peer) {
		return peer.Unmap().String()
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		if !trusted(a) {
			return a.Unmap().String()
		}
	}
	return peer.Unmap().String()
}

// parseForm reads an application/x-www-form-urlencoded body strictly: parameters only in
// the body, each at most once (RFC 6749 §3.2), bounded size.
func parseForm(w http.ResponseWriter, r *http.Request) (url.Values, string) {
	if r.URL.RawQuery != "" {
		return nil, "parameters must be sent in the request body"
	}
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/x-www-form-urlencoded" {
		return nil, "content type must be application/x-www-form-urlencoded"
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFormBytes))
	if err != nil {
		return nil, "request body too large or unreadable"
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, "malformed request body"
	}
	for k, v := range form {
		if len(v) != 1 {
			return nil, "parameter " + strconv.Quote(k) + " must appear exactly once"
		}
	}
	return form, ""
}

func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPost) {
		return
	}
	a := exchangeAudit{remote: s.clientIP(r)}
	if !s.ipLimit.allow(a.remote) {
		s.fail(w, a, http.StatusTooManyRequests, codeSlowDown, "rate limit exceeded; retry later")
		return
	}
	form, problem := parseForm(w, r)
	if problem != "" {
		s.fail(w, a, http.StatusBadRequest, string(auth.CodeInvalidRequest), problem)
		return
	}
	if gt := form.Get("grant_type"); gt != GrantTypeTokenExchange {
		s.fail(w, a, http.StatusBadRequest, codeUnsupportedGrantType, "grant_type must be "+GrantTypeTokenExchange)
		return
	}
	subjectToken, tokenType := form.Get("subject_token"), form.Get("subject_token_type")
	switch {
	case subjectToken == "" || tokenType == "":
		s.fail(w, a, http.StatusBadRequest, string(auth.CodeInvalidRequest), "subject_token and subject_token_type are required")
		return
	case form.Has("actor_token") || form.Has("actor_token_type"):
		s.fail(w, a, http.StatusBadRequest, string(auth.CodeInvalidRequest), "actor tokens are not supported")
		return
	}
	if rt := form.Get("requested_token_type"); rt != "" && rt != auth.TokenTypeAccessToken && rt != auth.TokenTypeJWT {
		s.fail(w, a, http.StatusBadRequest, string(auth.CodeInvalidRequest), "requested_token_type must be an access token or JWT")
		return
	}
	audience := form.Get("audience")
	if audience != "" && !s.instances[audience] {
		s.fail(w, a, http.StatusBadRequest, string(auth.CodeInvalidRequest), "audience must be a configured instance name")
		return
	}

	var p *auth.Principal
	var err error
	if keys.LooksLikeServiceKey(subjectToken) {
		a.issuer = auth.IssuerServiceAccount
		if tokenType != auth.TokenTypeAccessToken && tokenType != auth.TokenTypeServiceKey {
			s.fail(w, a, http.StatusBadRequest, string(auth.CodeInvalidRequest), "service keys use subject_token_type "+auth.TokenTypeServiceKey)
			return
		}
		rec, kerr := s.d.Keys.ServiceKeys.Authenticate(r.Context(), subjectToken)
		var ke *keys.KeyError
		switch {
		case errors.As(kerr, &ke):
			a.detail = ke.Reason
			s.fail(w, a, http.StatusBadRequest, string(auth.CodeInvalidGrant), "invalid service key")
			return
		case kerr != nil:
			s.d.Log.Error("service key lookup failed", "error", kerr)
			s.fail(w, a, http.StatusInternalServerError, string(auth.CodeServerError), "internal error")
			return
		}
		p, err = s.d.Engine.ExchangeServiceAccount(r.Context(), auth.ServiceAccountKey{ID: rec.ID, Account: rec.Account, BreakGlass: rec.BreakGlass})
	} else {
		p, err = s.d.Engine.ExchangeToken(r.Context(), tokenType, subjectToken)
	}
	if err != nil {
		e := auth.AsError(err)
		if e.Code == auth.CodeServerError && errors.Unwrap(e) != nil {
			s.d.Log.Error("token exchange failed", "error", errors.Unwrap(e))
		}
		a.issuer = e.Issuer
		if e.Policy != "" {
			a.policies = []string{e.Policy}
		}
		s.fail(w, a, statusFor(e.Code), string(e.Code), e.Reason)
		return
	}
	a.issuer, a.subject, a.policies, a.grants = p.Issuer, p.Subject, p.Policies, p.GrantNames

	// Renewal is refused for a deny-listed principal (R-AUTH-9, normal path).
	if s.d.Keys.Revocations.IsDenied(p.Session, p.Subject) {
		s.fail(w, a, http.StatusForbidden, string(auth.CodeAccessDenied), "principal is revoked")
		return
	}
	if !s.subLimit.allow(p.Subject) {
		s.fail(w, a, http.StatusTooManyRequests, codeSlowDown, "rate limit exceeded for this principal; retry later")
		return
	}
	grants := p.Grants
	if audience != "" {
		if grants = grants.Restrict(audience); grants.Empty() {
			s.fail(w, a, http.StatusForbidden, string(auth.CodeAccessDenied), "no grant on the requested audience")
			return
		}
	}
	raw, claims, err := s.d.Keys.Minter.Mint(keys.MintRequest{Subject: p.Subject, Session: p.Session, Name: p.DisplayName, Scopes: grants.Scopes(), TTL: p.TTL})
	if err != nil {
		s.d.Log.Error("minting token failed", "error", err)
		s.fail(w, a, http.StatusInternalServerError, string(auth.CodeServerError), "internal error")
		return
	}
	ttl := claims.Expiry - claims.IssuedAt
	a.result, a.scopes, a.sid, a.jti, a.ttl = "ok", claims.Cucina, claims.Session, claims.ID, ttl
	s.audit(a)
	s.metrics.exchanges.WithLabelValues(a.issuer, "ok").Inc()
	s.metrics.ttl.Observe(float64(ttl))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: raw, IssuedTokenType: auth.TokenTypeAccessToken, TokenType: "Bearer", ExpiresIn: ttl,
	})
}
