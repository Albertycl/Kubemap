package auth

// Optional TOTP two-factor login (RFC 6238: HMAC-SHA1, 6 digits, 30 second
// steps, a window of one step either side). Standard library only.
//
// The secret is stored encrypted (AES-256-GCM, key in <data dir>/totp.key,
// mode 0600) and is shown once, in the answer to POST /api/totp/setup; no
// other API returns it. Recovery codes are stored as SHA-256 hashes and work
// once. A code of a step that was already accepted is refused (replay).
//
// API (all need a logged-in session):
//
//	GET  /api/totp          -> {"enabled","recoveryCodesLeft"}
//	POST /api/totp/setup    {"password"} -> {"secret","uri"}      (not when enabled)
//	POST /api/totp/enable   {"code"} -> {"recoveryCodes":[...]}   (shown once)
//	POST /api/totp/disable  {"password","code"}                   (code or recovery code)
//
// Lockout recovery: "kubemap-portal disable-totp" (see Service.DisableTOTP).

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"kubemap/portal/httpjson"
	"kubemap/portal/store"
)

const (
	totpDigits   = 6
	totpPeriod   = 30 // seconds
	totpWindow   = 1  // steps either side of now
	totpIssuer   = "Kubemap"
	totpKeyFile  = "totp.key"
	recoveryN    = 8
	recoveryRaw  = 8 // random bytes per recovery code (64 bits)
	secretBytes  = 20
	totpKeyBytes = 32
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// totpCode is the HOTP value (RFC 4226) of secret for the time step.
func totpCode(secret []byte, step int64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	s := strconv.FormatUint(uint64(v%1000000), 10)
	return strings.Repeat("0", totpDigits-len(s)) + s
}

// totpMatch returns the time step whose code equals code, within the window
// around now. All steps are compared (no early exit). With several matches
// the newest wins.
func totpMatch(secret []byte, code string, now time.Time) (int64, bool) {
	cur := now.Unix() / totpPeriod
	found := int64(-1)
	for d := -totpWindow; d <= totpWindow; d++ {
		st := cur + int64(d)
		if st >= 0 && subtle.ConstantTimeCompare([]byte(totpCode(secret, st)), []byte(code)) == 1 {
			found = st
		}
	}
	return found, found >= 0
}

func newTOTPSecret() ([]byte, error) {
	b := make([]byte, secretBytes)
	_, err := rand.Read(b)
	return b, err
}

// totpURI is the otpauth:// URI authenticator apps read (key URI format).
func totpURI(secret []byte, user string) string {
	v := url.Values{}
	v.Set("secret", b32.EncodeToString(secret))
	v.Set("issuer", totpIssuer)
	v.Set("algorithm", "SHA1")
	v.Set("digits", strconv.Itoa(totpDigits))
	v.Set("period", strconv.Itoa(totpPeriod))
	return "otpauth://totp/" + url.PathEscape(totpIssuer+":"+user) + "?" + v.Encode()
}

// isTOTPCode reports whether s looks like a 6-digit code.
func isTOTPCode(s string) bool {
	if len(s) != totpDigits {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// normalizeCode drops spaces and dashes and upper-cases, so "123 456" and
// "abcd-efgh-ijkl" are accepted.
func normalizeCode(s string) string {
	return strings.ToUpper(strings.NewReplacer(" ", "", "-", "", "\t", "").Replace(s))
}

// newRecoveryCodes returns recoveryN codes ("ABCD-EFGH-IJKL" style, 12
// base32 characters in three groups of four) and their hashes.
func newRecoveryCodes() (codes, hashes []string, err error) {
	for i := 0; i < recoveryN; i++ {
		b := make([]byte, recoveryRaw)
		if _, err := rand.Read(b); err != nil {
			return nil, nil, err
		}
		raw := b32.EncodeToString(b)[:12]
		codes = append(codes, raw[0:4]+"-"+raw[4:8]+"-"+raw[8:12])
		hashes = append(hashes, hashRecovery(raw))
	}
	return codes, hashes, nil
}

func hashRecovery(normalized string) string {
	sum := sha256.Sum256([]byte("kubemap-recovery:" + normalized))
	return hex.EncodeToString(sum[:])
}

// key returns the encryption key, creating the key file on first use.
func (s *Service) totpKey() ([]byte, error) {
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if s.key != nil {
		return s.key, nil
	}
	path := filepath.Join(s.dataDir, totpKeyFile)
	for try := 0; try < 2; try++ {
		b, err := os.ReadFile(path)
		if err == nil {
			if len(b) != totpKeyBytes {
				return nil, fmt.Errorf("auth: %s is damaged (%d bytes); not overwriting it", path, len(b))
			}
			s.key = b
			return b, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		// A missing key with enabled TOTP rows means the key file was lost
		// (new volume?). A new key could not open those secrets, and would
		// silently lock the user out, so refuse to make one.
		n, cerr := s.store.CountEnabledTOTP(context.Background())
		if cerr != nil {
			return nil, cerr
		}
		if n > 0 {
			err := fmt.Errorf("auth: %s is missing but %d user(s) have two-factor login on; not creating a new key. Restore the key file, or run the disable-totp command", path, n)
			s.log.Error("totp key missing", "path", path, "enabled_users", n)
			return nil, err
		}
		k := make([]byte, totpKeyBytes)
		if _, err := rand.Read(k); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue // another process created it; read it
		}
		if err != nil {
			return nil, err
		}
		_, werr := f.Write(k)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(path)
			return nil, werr
		}
	}
	return nil, errors.New("auth: could not read or create " + path)
}

func (s *Service) gcm() (cipher.AEAD, error) {
	k, err := s.totpKey()
	if err != nil {
		return nil, err
	}
	blk, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

// sealSecret encrypts a secret; the user name is authenticated with it, so a
// stolen record cannot be moved to another user.
func (s *Service) sealSecret(user string, secret []byte) ([]byte, error) {
	g, err := s.gcm()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return g.Seal(nonce, nonce, secret, []byte(user)), nil
}

func (s *Service) openSecret(user string, enc []byte) ([]byte, error) {
	g, err := s.gcm()
	if err != nil {
		return nil, err
	}
	if len(enc) < g.NonceSize() {
		return nil, errors.New("auth: totp secret record too short")
	}
	return g.Open(nil, enc[:g.NonceSize()], enc[g.NonceSize():], []byte(user))
}

// errTOTPLocked means the user has had MaxTOTPFailures wrong second-factor
// codes within FailureWindow, from any address.
var errTOTPLocked = errors.New("auth: second factor locked for this user")

// checkSecondFactor verifies a TOTP code or a recovery code of an enabled
// user. A TOTP code is accepted once per time step. It returns
// errTOTPLocked while the user's account-wide failure limit is reached.
func (s *Service) checkSecondFactor(r *http.Request, user string, t store.TOTP, code string) (ok bool, recovery bool, err error) {
	code = normalizeCode(code)
	if code == "" {
		return false, false, nil
	}
	// Account-wide limit, independent of the client address (an attacker
	// with the password could otherwise spread guesses over many addresses,
	// or hide behind a shared proxy address). The attempt is counted before
	// the code is checked, so parallel requests cannot all slip through.
	// Authenticator codes and recovery codes share one counter, and while
	// the user is locked neither is accepted, not even a correct recovery
	// code: the lock ends by itself after FailureWindow, it never needs the
	// admin, and an existing session is not touched. A right code resets
	// the counter.
	n, allowed, _ := s.totpFails.reserveN(user, s.now(), MaxTOTPFailures)
	if !allowed {
		return false, false, errTOTPLocked
	}
	defer func() {
		switch {
		case err != nil:
			s.totpFails.refund(user)
		case ok:
			s.totpFails.reset(user)
		case n == MaxTOTPFailures:
			s.log.Warn("second factor locked", "username", user, "for", FailureWindow.String())
			s.audit(r.Context(), user, "totp_locked", httpjson.ClientIP(r), "locked for "+FailureWindow.String())
		}
	}()
	if isTOTPCode(code) {
		secret, err := s.openSecret(user, t.SecretEnc)
		if err != nil {
			return false, false, err
		}
		step, match := totpMatch(secret, code, s.now())
		if !match {
			return false, false, nil
		}
		fresh, err := s.store.AdvanceTOTPStep(r.Context(), user, step)
		return fresh, false, err
	}
	used, err := s.store.UseRecoveryCode(r.Context(), user, hashRecovery(code))
	return used, true, err
}

// totpLockedReply answers a refused second-factor attempt.
func (s *Service) totpLockedReply(w http.ResponseWriter, user string) {
	wait := FailureWindow
	s.totpFails.mu.Lock()
	if f := s.totpFails.entries[user]; f != nil {
		wait = f.first.Add(FailureWindow).Sub(s.now())
	}
	s.totpFails.mu.Unlock()
	w.Header().Set("Retry-After", strconv.Itoa(max(int(wait.Seconds())+1, 1)))
	httpjson.Error(w, http.StatusTooManyRequests, "totp_locked", "too many wrong codes; two-factor login is paused for this user for up to 15 minutes")
}

// DisableTOTP removes the user's two-factor login (for the "disable-totp"
// command, when the phone and the recovery codes are lost). It reports
// whether TOTP was set up.
func (s *Service) DisableTOTP(ctx context.Context, username string) (bool, error) {
	had, err := s.store.DeleteTOTP(ctx, username)
	if err != nil {
		return false, err
	}
	res := "ok"
	if !had {
		res = "not set up"
	}
	s.audit(ctx, "cli", "totp.disable", username, res)
	return had, nil
}

type totpStatus struct {
	Enabled           bool `json:"enabled"`
	RecoveryCodesLeft int  `json:"recoveryCodesLeft"`
}

func (s *Service) handleTOTPStatus(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFromContext(r.Context())
	st := totpStatus{}
	t, err := s.store.GetTOTP(r.Context(), user)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internal(w, "totp status", err)
		return
	}
	if err == nil && t.Enabled {
		n, err := s.store.CountRecoveryCodes(r.Context(), user)
		if err != nil {
			s.internal(w, "totp status", err)
			return
		}
		st = totpStatus{Enabled: true, RecoveryCodesLeft: n}
	}
	httpjson.Write(w, http.StatusOK, st)
}

type totpSetupRequest struct {
	Password string `json:"password"`
}

type totpSetupResponse struct {
	Secret string `json:"secret"`
	URI    string `json:"uri"`
}

func (s *Service) handleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	var req totpSetupRequest
	if !httpjson.Read(w, r, &req) {
		return
	}
	user, _ := UserFromContext(r.Context())
	if !s.Reauthenticate(w, r, req.Password) {
		return
	}
	if t, err := s.store.GetTOTP(r.Context(), user); err == nil && t.Enabled {
		httpjson.Error(w, http.StatusConflict, "totp_enabled", "two-factor login is already on; turn it off first")
		return
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internal(w, "totp setup", err)
		return
	}
	secret, err := newTOTPSecret()
	if err == nil {
		var enc []byte
		if enc, err = s.sealSecret(user, secret); err == nil {
			err = s.store.SetPendingTOTP(r.Context(), user, enc)
		}
	}
	if err != nil {
		s.internal(w, "totp setup", err)
		return
	}
	s.audit(r.Context(), user, "totp.setup", user, "ok")
	httpjson.Write(w, http.StatusOK, totpSetupResponse{Secret: b32.EncodeToString(secret), URI: totpURI(secret, user)})
}

type totpCodeRequest struct {
	Code string `json:"code"`
}

// reserveAttempt counts one second-factor attempt against the login limit of
// this client. On false it has written the answer.
func (s *Service) reserveAttempt(w http.ResponseWriter, r *http.Request) (key string, ok bool) {
	_, key, max := s.clientLimit(r)
	now := s.now()
	if _, ok, until := s.limiter.reserveN(key, now, max); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(until.Sub(now).Seconds())+1))
		httpjson.Error(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed attempts; try again later")
		return key, false
	}
	return key, true
}

type totpEnableResponse struct {
	RecoveryCodes []string `json:"recoveryCodes"`
}

func (s *Service) handleTOTPEnable(w http.ResponseWriter, r *http.Request) {
	var req totpCodeRequest
	if !httpjson.Read(w, r, &req) {
		return
	}
	user, _ := UserFromContext(r.Context())
	key, ok := s.reserveAttempt(w, r)
	if !ok {
		return
	}
	t, err := s.store.GetTOTP(r.Context(), user)
	if errors.Is(err, store.ErrNotFound) || (err == nil && t.Enabled) {
		s.limiter.refund(key)
		httpjson.Error(w, http.StatusConflict, "no_pending_totp", "start the setup first (or two-factor login is already on)")
		return
	}
	var secret []byte
	if err == nil {
		secret, err = s.openSecret(user, t.SecretEnc)
	}
	if err != nil {
		s.limiter.refund(key) // a server fault, not a wrong guess
		s.internal(w, "totp enable", err)
		return
	}
	code := normalizeCode(req.Code)
	step, match := totpMatch(secret, code, s.now())
	if !isTOTPCode(code) || !match {
		s.log.Warn("totp enable failed", "client", httpjson.ClientIP(r), "username", user)
		s.audit(r.Context(), user, "totp.enable", user, "failed")
		httpjson.Error(w, http.StatusForbidden, "bad_totp", "that code is wrong; check the time on your phone and try the next code")
		return
	}
	codes, hashes, err := newRecoveryCodes()
	if err == nil {
		err = s.store.EnableTOTP(r.Context(), user, step, hashes)
	}
	if err != nil {
		s.limiter.refund(key) // a server fault, not a wrong guess
		s.internal(w, "totp enable", err)
		return
	}
	s.limiter.refund(key)
	// Turning the second factor on ends every other session of this user
	// (one stolen earlier no longer works); this browser keeps its own.
	if c, cerr := r.Cookie(CookieName); cerr == nil && c.Value != "" {
		if derr := s.store.DeleteOtherSessions(r.Context(), user, c.Value); derr != nil {
			s.log.Error("auth: end other sessions after totp enable", "err", derr)
		}
	}
	s.audit(r.Context(), user, "totp.enable", user, "ok")
	s.log.Info("totp enabled", "username", user)
	httpjson.Write(w, http.StatusOK, totpEnableResponse{RecoveryCodes: codes})
}

type totpDisableRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

func (s *Service) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	var req totpDisableRequest
	if !httpjson.Read(w, r, &req) {
		return
	}
	user, _ := UserFromContext(r.Context())
	if !s.Reauthenticate(w, r, req.Password) {
		return
	}
	key, ok := s.reserveAttempt(w, r)
	if !ok {
		return
	}
	t, err := s.store.GetTOTP(r.Context(), user)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !t.Enabled) {
		s.limiter.refund(key)
		httpjson.Error(w, http.StatusConflict, "totp_not_enabled", "two-factor login is not on")
		return
	}
	var good bool
	if err == nil {
		good, _, err = s.checkSecondFactor(r, user, t, req.Code)
	}
	if errors.Is(err, errTOTPLocked) {
		s.limiter.refund(key)
		s.totpLockedReply(w, user)
		return
	}
	if err != nil {
		s.limiter.refund(key) // a server fault, not a wrong guess
		s.internal(w, "totp disable", err)
		return
	}
	if !good {
		s.log.Warn("totp disable failed", "client", httpjson.ClientIP(r), "username", user)
		s.audit(r.Context(), user, "totp.disable", user, "failed")
		httpjson.Error(w, http.StatusForbidden, "bad_totp", "wrong or already used code")
		return
	}
	if _, err := s.store.DeleteTOTP(r.Context(), user); err != nil {
		s.limiter.refund(key) // a server fault, not a wrong guess
		s.internal(w, "totp disable", err)
		return
	}
	s.limiter.refund(key)
	s.audit(r.Context(), user, "totp.disable", user, "ok")
	s.log.Info("totp disabled", "username", user)
	w.WriteHeader(http.StatusNoContent)
}
