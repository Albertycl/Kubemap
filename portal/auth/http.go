package auth

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"kubemap/portal/httpjson"
	"kubemap/portal/store"
)

type ctxKey struct{}

// UserFromContext returns the logged-in user set by Require.
func UserFromContext(ctx context.Context) (string, bool) {
	u, ok := ctx.Value(ctxKey{}).(string)
	return u, ok
}

// ContextWithUser returns ctx as Require leaves it for a logged-in user,
// for tests of the APIs behind Require.
func ContextWithUser(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, ctxKey{}, user)
}

// Routes registers the login API on mux.
func (s *Service) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.Handle("POST /api/logout", s.require(http.HandlerFunc(s.handleLogout), true))
	mux.Handle("GET /api/me", s.require(http.HandlerFunc(s.handleMe), true))
	mux.Handle("POST /api/password", s.require(http.HandlerFunc(s.handlePassword), true))
	mux.Handle("GET /api/totp", s.require(http.HandlerFunc(s.handleTOTPStatus), false))
	mux.Handle("POST /api/totp/setup", s.require(http.HandlerFunc(s.handleTOTPSetup), false))
	mux.Handle("POST /api/totp/enable", s.require(http.HandlerFunc(s.handleTOTPEnable), false))
	mux.Handle("POST /api/totp/disable", s.require(http.HandlerFunc(s.handleTOTPDisable), false))
}

// Require lets a request through only with a valid session whose password
// does not have to change first. It also rejects cross-site writes.
func (s *Service) Require(next http.Handler) http.Handler { return s.require(next, false) }

func (s *Service) require(next http.Handler, allowMustChange bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			httpjson.Error(w, http.StatusForbidden, "cross_origin", "cross-origin request refused")
			return
		}
		c, err := r.Cookie(CookieName)
		if err != nil || c.Value == "" {
			httpjson.Error(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		sess, err := s.store.GetSession(r.Context(), c.Value)
		if errors.Is(err, store.ErrNotFound) {
			clearCookie(w, r)
			httpjson.Error(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		if err != nil {
			s.internal(w, "get session", err)
			return
		}
		u, err := s.store.GetUser(r.Context(), sess.Username)
		if err != nil {
			clearCookie(w, r)
			httpjson.Error(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		if u.MustChangePassword && !allowMustChange {
			httpjson.Error(w, http.StatusForbidden, "password_change_required", "change the initial password first")
			return
		}
		// Sliding expiry, written at most every few minutes.
		if exp := s.sessionExpiry(sess.CreatedAt); exp.Sub(sess.ExpiresAt) > 5*time.Minute {
			if err := s.store.ExtendSession(r.Context(), c.Value, exp); err == nil {
				setCookie(w, r, c.Value, exp)
			} else {
				// The session still ends at its old time; worth knowing when
				// someone asks why they were logged out (review km-16 N7).
				s.log.Debug("could not extend a session", "user", u.Username, "err", err)
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, u.Username)))
	})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	// Code is the TOTP code or a recovery code; needed only once two-factor
	// login is on for the user.
	Code string `json:"code"`
}

type meResponse struct {
	Username           string `json:"username"`
	MustChangePassword bool   `json:"mustChangePassword"`
	TOTPEnabled        bool   `json:"totpEnabled"`
}

// me builds the answer for a user; a failed TOTP lookup counts as "off" in
// the answer but is logged.
func (s *Service) me(ctx context.Context, u store.User) meResponse {
	t, err := s.store.GetTOTP(ctx, u.Username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Error("auth: totp lookup", "err", err)
	}
	return meResponse{Username: u.Username, MustChangePassword: u.MustChangePassword, TOTPEnabled: err == nil && t.Enabled}
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		httpjson.Error(w, http.StatusForbidden, "cross_origin", "cross-origin request refused")
		return
	}
	var req loginRequest
	if !httpjson.Read(w, r, &req) {
		return
	}
	ip, key, max := s.clientLimit(r)
	now := s.now()
	name := strings.ToLower(strings.TrimSpace(req.Username))
	// Count the attempt before checking the password (B1): parallel
	// requests cannot slip past the limit while bcrypt runs.
	if _, ok, until := s.limiter.reserveN(key, now, max); !ok {
		s.log.Warn("login blocked", "client", ip, "username", s.loginName(r.Context(), req.Username))
		w.Header().Set("Retry-After", strconv.Itoa(int(until.Sub(now).Seconds())+1))
		httpjson.Error(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed logins; try again later")
		return
	}
	// Many failures for this username, from any address: wait first.
	if d := s.backoff.delay(name, now); d > 0 {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			s.limiter.refund(key)
			return
		}
	}
	var u store.User
	var ok bool
	err := s.withHashSlot(r.Context(), func() (err error) {
		u, ok, err = s.authenticate(r.Context(), req.Username, req.Password)
		return err
	})
	if errors.Is(err, errBusy) {
		s.limiter.refund(key)
		w.Header().Set("Retry-After", "5")
		httpjson.Error(w, http.StatusServiceUnavailable, "busy", "the portal is busy; try again in a few seconds")
		return
	}
	if err != nil {
		s.internal(w, "login", err)
		return
	}
	if !ok {
		s.backoff.failed(name, now)
		who := s.loginName(r.Context(), req.Username)
		s.log.Warn("login failed", "client", ip, "username", who)
		s.audit(r.Context(), who, "login", ip, "failed")
		httpjson.Error(w, http.StatusUnauthorized, "bad_credentials", "wrong username or password")
		return
	}
	if t, terr := s.store.GetTOTP(r.Context(), u.Username); terr == nil && t.Enabled {
		// The password was right; only now is the second factor asked for,
		// so the answer says nothing to someone without the password.
		if strings.TrimSpace(req.Code) == "" {
			s.limiter.refund(key)
			httpjson.Error(w, http.StatusUnauthorized, "totp_required", "enter the code from your authenticator app")
			return
		}
		good, recovery, cerr := s.checkSecondFactor(r, u.Username, t, req.Code)
		if errors.Is(cerr, errTOTPLocked) {
			s.limiter.refund(key)
			s.log.Warn("login refused (second factor locked)", "client", ip, "username", u.Username)
			s.totpLockedReply(w, u.Username)
			return
		}
		if cerr != nil {
			s.limiter.refund(key) // a server fault, not a wrong guess
			s.internal(w, "login totp", cerr)
			return
		}
		if !good {
			// No username backoff here: the password was right, and the
			// account-wide second-factor limit (checkSecondFactor) covers
			// this step without slowing down the real admin's next login.
			s.log.Warn("login failed (second factor)", "client", ip, "username", u.Username)
			s.audit(r.Context(), u.Username, "login.totp", ip, "failed")
			httpjson.Error(w, http.StatusUnauthorized, "bad_totp", "wrong or already used code")
			return
		}
		if recovery {
			s.audit(r.Context(), u.Username, "totp.recovery", ip, "ok")
		}
	} else if terr != nil && !errors.Is(terr, store.ErrNotFound) {
		s.limiter.refund(key)
		s.internal(w, "login totp", terr)
		return
	}
	// A session this browser still holds ends now (N4).
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		s.store.DeleteSession(r.Context(), c.Value)
	}
	s.limiter.reset(key)
	s.backoff.succeeded(name)
	if !s.startSession(w, r, u.Username) {
		return
	}
	s.log.Info("login", "client", ip, "username", u.Username)
	s.audit(r.Context(), u.Username, "login", ip, "ok")
	httpjson.Write(w, http.StatusOK, s.me(r.Context(), u))
}

func (s *Service) startSession(w http.ResponseWriter, r *http.Request, username string) bool {
	exp := s.now().Add(IdleTimeout)
	tok, err := s.store.CreateSession(r.Context(), username, exp)
	if err != nil {
		s.internal(w, "create session", err)
		return false
	}
	setCookie(w, r, tok, exp)
	return true
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil {
		if err := s.store.DeleteSession(r.Context(), c.Value); err != nil {
			s.internal(w, "logout", err)
			return
		}
	}
	user, _ := UserFromContext(r.Context())
	s.audit(r.Context(), user, "logout", httpjson.ClientIP(r), "ok")
	clearCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleMe(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	u, err := s.store.GetUser(r.Context(), user)
	if err != nil {
		s.internal(w, "me", err)
		return
	}
	httpjson.Write(w, http.StatusOK, s.me(r.Context(), u))
}

type passwordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

func (s *Service) handlePassword(w http.ResponseWriter, r *http.Request) {
	var req passwordRequest
	if !httpjson.Read(w, r, &req) {
		return
	}
	user, _ := UserFromContext(r.Context())
	_, ip, max := s.clientLimit(r) // the same key as the login limit
	if _, ok, _ := s.limiter.reserveN(ip, s.now(), max); !ok {
		httpjson.Error(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed attempts; try again later")
		return
	}
	err := s.withHashSlot(r.Context(), func() error {
		return s.changePassword(r.Context(), user, req.CurrentPassword, req.NewPassword)
	})
	switch {
	case errors.Is(err, errBusy):
		s.limiter.refund(ip)
		w.Header().Set("Retry-After", "5")
		httpjson.Error(w, http.StatusServiceUnavailable, "busy", "the portal is busy; try again in a few seconds")
		return
	case errors.Is(err, errWrongPassword):
		s.audit(r.Context(), user, "password.change", user, "failed")
		httpjson.Error(w, http.StatusForbidden, "bad_credentials", err.Error())
		return
	case errors.Is(err, ErrWeakPassword), errors.Is(err, errSamePassword):
		s.limiter.refund(ip) // the current password was right
		httpjson.Error(w, http.StatusBadRequest, "bad_password", err.Error())
		return
	case err != nil:
		s.internal(w, "change password", err)
		return
	}
	s.limiter.reset(ip)
	s.audit(r.Context(), user, "password.change", user, "ok")
	s.log.Info("password changed", "username", user)
	// Every old session ended; this browser gets a new one.
	if !s.startSession(w, r, user) {
		return
	}
	httpjson.Write(w, http.StatusOK, s.me(r.Context(), store.User{Username: user}))
}

func (s *Service) internal(w http.ResponseWriter, what string, err error) {
	s.log.Error("auth: "+what, "err", err)
	httpjson.Error(w, http.StatusInternalServerError, "internal", "internal error")
}

// secureCookie is true unless Traefik (or another proxy) says the browser
// came over plain http. The server itself speaks plain HTTP behind the proxy.
func secureCookie(r *http.Request) bool {
	return !strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "http")
}

func setCookie(w http.ResponseWriter, r *http.Request, token string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		Secure:   secureCookie(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func clearCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: secureCookie(r), SameSite: http.SameSiteStrictMode,
	})
}

// RequestHeader is the header the web app sends on every state-changing
// request. A cross-site page cannot set it (nor a JSON Content-Type) without
// a CORS preflight, which the portal never answers.
const RequestHeader = "X-Kubemap-Request"

// sameOrigin rejects state-changing requests whose Origin (or, failing that,
// Referer) names another host, and those that carry neither a JSON body nor
// RequestHeader: even without an Origin header, a body-less write (logout,
// accept, reject, delete) must be one only a same-origin script can send
// (review KM-44 L15). Together with SameSite=Strict cookies this blocks
// cross-site request forgery.
func sameOrigin(r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		return true
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" && r.Header.Get(RequestHeader) == "" {
		return false
	}
	src := r.Header.Get("Origin")
	if src == "" {
		src = r.Header.Get("Referer")
	}
	if src == "" {
		return true // non-browser client; browsers send Origin on POST
	}
	u, err := url.Parse(src)
	return err == nil && u.Host == r.Host
}

// trim bounds a user-supplied username and escapes control characters
// before it goes to logs or the audit log.
// loginName is the user name a failed or blocked login is logged and
// audited under. A name that is no user is often a password typed into the
// wrong field, so only its length is kept (review-km-44 L8).
func (s *Service) loginName(ctx context.Context, name string) string {
	if name != "" {
		if _, err := s.store.GetUser(ctx, name); err == nil {
			return trim(name)
		}
	}
	return fmt.Sprintf("(unknown user, %d characters)", len(name))
}

func trim(s string) string {
	const max = 64
	if len(s) > max {
		s = s[:max]
	}
	q := strconv.Quote(s)
	return q[1 : len(q)-1]
}

// Reauthenticate checks the logged-in user's password again before a
// dangerous change (for example turning two-factor login on or off). It shares the login
// limit per address, so it cannot be used to guess passwords. On failure it
// has written the answer and returns false.
func (s *Service) Reauthenticate(w http.ResponseWriter, r *http.Request, password string) bool {
	user, ok := UserFromContext(r.Context())
	if !ok {
		httpjson.Error(w, http.StatusUnauthorized, "unauthorized", "login required")
		return false
	}
	addr, ip, max := s.clientLimit(r) // the same key as the login limit
	now := s.now()
	if _, ok, until := s.limiter.reserveN(ip, now, max); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(until.Sub(now).Seconds())+1))
		httpjson.Error(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed password checks; try again later")
		return false
	}
	var good bool
	err := s.withHashSlot(r.Context(), func() (err error) {
		_, good, err = s.authenticate(r.Context(), user, password)
		return err
	})
	if errors.Is(err, errBusy) {
		s.limiter.refund(ip)
		w.Header().Set("Retry-After", "5")
		httpjson.Error(w, http.StatusServiceUnavailable, "busy", "the portal is busy; try again in a few seconds")
		return false
	}
	if err != nil {
		s.internal(w, "reauthenticate", err)
		return false
	}
	if !good {
		s.log.Warn("password check failed", "client", addr, "username", user)
		s.audit(r.Context(), user, "reauth", addr, "failed")
		httpjson.Error(w, http.StatusForbidden, "bad_password", "wrong password")
		return false
	}
	s.limiter.refund(ip)
	return true
}

// clientLimit is the client address and the failure-limit key and limit for
// it. A client the portal can tell apart gets MaxFailures under its own key
// (an IPv6 client by /64). When only a trusted proxy address is known
// (junk X-Forwarded-For, or SNAT), many visitors share the key, so it has
// its own bucket with SharedMaxFailures; the per-user second-factor limit
// is then the main defence.
func (s *Service) clientLimit(r *http.Request) (addr, key string, max int) {
	addr, shared := httpjson.ClientAddr(r)
	if shared {
		return addr, "shared:" + addr, SharedMaxFailures
	}
	return addr, clientKey(addr), MaxFailures
}
