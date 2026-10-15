package api

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/saker-ai/grayling/internal/platform"
)

const (
	consoleCookieName = "grayling_admin_session"
	consoleSessionTTL = 8 * time.Hour
	// consoleDevSignedOutCookie records an explicit console sign-out while the
	// server runs in developer mode. It carries no value and lives for the
	// browser session only, so automatic sign-in resumes in a new session.
	consoleDevSignedOutCookie = "grayling_dev_signed_out"
	// devConsoleCredential is the sealed payload of a developer-mode session.
	// It is deliberately not the configured admin password: the server then
	// never needs the plaintext password in memory, and the value is
	// unreachable from outside — it only travels as a cookie this server
	// sealed with the console session key, so callers cannot present it
	// directly as a bearer credential.
	devConsoleCredential = "\x00grayling-dev-autologin\x00"
)

// devConsoleCredentialHash is the digest authenticate compares against.
var devConsoleCredentialHash = sha256.Sum256([]byte(devConsoleCredential))

func (s *Server) initConsoleSessionKey(secret string) error {
	if secret == "" {
		_, err := rand.Read(s.consoleSessionKey[:])
		return err
	}
	s.consoleSessionKey = sha256.Sum256([]byte("grayling console session v1\x00" + secret))
	return nil
}

func (s *Server) sealConsoleSession(token string) (string, error) {
	block, err := aes.NewCipher(s.consoleSessionKey[:])
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload := make([]byte, 8, 8+len(token))
	binary.BigEndian.PutUint64(payload, uint64(time.Now().Add(consoleSessionTTL).Unix()))
	payload = append(payload, token...)
	return base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, payload, nil)), nil
}

func (s *Server) consoleSessionToken(r *http.Request) string {
	cookie, err := r.Cookie(consoleCookieName)
	if err != nil || len(cookie.Value) > 8192 {
		return ""
	}
	sealed, err := base64.RawURLEncoding.DecodeString(cookie.Value)
	if err != nil {
		return ""
	}
	block, err := aes.NewCipher(s.consoleSessionKey[:])
	if err != nil {
		return ""
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(sealed) < aead.NonceSize()+aead.Overhead()+9 {
		return ""
	}
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], nil)
	if err != nil || len(plain) < 9 || time.Now().Unix() >= int64(binary.BigEndian.Uint64(plain[:8])) {
		return ""
	}
	return string(plain[8:])
}

func sameOriginRequest(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.Host != r.Host {
		return false
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return u.Scheme == scheme
}

func consoleSecureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func consoleLoopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) handleConsoleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow(adminLimiterKey(r)) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "admin request rate exceeded")
		return
	}
	if !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "bad_origin", "same-origin request required")
		return
	}
	if !consoleSecureRequest(r) && !consoleLoopbackHost(r.Host) {
		writeError(w, http.StatusForbidden, "tls_required", "console session requires HTTPS")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_form", "invalid session request")
		return
	}
	secret := r.PostForm.Get("token")
	if secret == "" {
		writeError(w, http.StatusBadRequest, "missing_token", "admin credential required")
		return
	}
	check := r.Clone(r.Context())
	check.Header.Set("Authorization", "Bearer "+secret)
	tok, ok := s.authenticate(w, check, platform.TokenAdmin)
	if !ok {
		return
	}
	sealed, err := s.sealConsoleSession(secret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_error", "could not create session")
		return
	}
	// A manual sign-in ends an earlier console sign-out, so developer-mode
	// automatic sign-in resumes.
	if s.devAutoLogin {
		http.SetCookie(w, &http.Cookie{Name: consoleDevSignedOutCookie, Path: "/", HttpOnly: true,
			Secure: consoleSecureRequest(r), SameSite: http.SameSiteStrictMode, MaxAge: -1})
	}
	http.SetCookie(w, &http.Cookie{Name: consoleCookieName, Value: sealed, Path: "/api/v1/admin", HttpOnly: true,
		Secure: consoleSecureRequest(r), SameSite: http.SameSiteStrictMode, MaxAge: int(consoleSessionTTL.Seconds())})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"name": tok.Name})
}

func (s *Server) handleConsoleSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	actor := actorFrom(r)
	writeJSON(w, http.StatusOK, map[string]string{"name": actor.Name, "principalId": actor.TokenID})
}

func (s *Server) handleConsoleLogout(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "bad_origin", "same-origin request required")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: consoleCookieName, Path: "/api/v1/admin", HttpOnly: true,
		Secure: consoleSecureRequest(r), SameSite: http.SameSiteStrictMode, MaxAge: -1})
	if s.devAutoLogin {
		// An explicit sign-out must not be undone by the automatic sign-in on
		// the next console request; the marker lasts for the browser session.
		http.SetCookie(w, &http.Cookie{Name: consoleDevSignedOutCookie, Value: "1", Path: "/", HttpOnly: true,
			Secure: consoleSecureRequest(r), SameSite: http.SameSiteStrictMode})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed_out"})
}

// devAutoLoginHandler signs console requests in as the configured
// administrator while the server runs in developer mode (-dev-autologin). It
// is a no-op at every other time: the option defaults to false and only
// cmd/grayling, driven by GRAYLING_DEV_AUTOLOGIN, can enable it.
//
// The session is minted for the request being served as well as for the
// browser: the console's first probe (web/app.js asks for
// /api/v1/admin/session as it loads) must already see the owner, or the
// sign-in form would flash before a refresh fixed it.
//
// The credential is never disclosed to the browser, and the minted cookie is
// an ordinary console session: an explicit sign-out (consoleDevSignedOutCookie)
// and revoked or absent admin configuration still take effect.
func (s *Server) devAutoLoginHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.devAutoLoginEnabled() || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			next.ServeHTTP(w, r)
			return
		}
		if s.consoleSessionToken(r) != "" {
			// The browser already holds a usable session.
			next.ServeHTTP(w, r)
			return
		}
		if _, err := r.Cookie(consoleDevSignedOutCookie); err == nil {
			// An explicit sign-out stands until the browser session ends.
			next.ServeHTTP(w, r)
			return
		}
		cookie, err := s.autoLoginCookie(r)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		http.SetCookie(w, cookie)
		// Hand the same session to the handler that is about to run, so the
		// request that discovers the console is also the one that signs it in.
		// A stale cookie of the same name is dropped first: the authentication
		// path reads the first match, and a leftover value from an earlier run
		// must not shadow the session just minted.
		forward := r.Clone(r.Context())
		carried := make([]string, 0, len(r.Cookies())+1)
		for _, existing := range r.Cookies() {
			if existing.Name != consoleCookieName {
				carried = append(carried, existing.Name+"="+existing.Value)
			}
		}
		forward.Header.Set("Cookie", strings.Join(append(carried, cookie.Name+"="+cookie.Value), "; "))
		next.ServeHTTP(w, forward)
	})
}

// devAutoLoginEnabled reports whether automatic developer sign-in can mint a
// session: it needs both the opt-in and a configured administrator password,
// because the session it mints is that credential.
func (s *Server) devAutoLoginEnabled() bool { return s.devAutoLogin && s.adminPasswordEnabled }

// autoLoginCookie seals the developer-mode marker into a console session.
func (s *Server) autoLoginCookie(r *http.Request) (*http.Cookie, error) {
	sealed, err := s.sealConsoleSession(devConsoleCredential)
	if err != nil {
		return nil, err
	}
	return &http.Cookie{Name: consoleCookieName, Value: sealed, Path: "/api/v1/admin", HttpOnly: true,
		Secure: consoleSecureRequest(r), SameSite: http.SameSiteStrictMode,
		MaxAge: int(consoleSessionTTL.Seconds())}, nil
}
