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

	"github.com/godeps/golab/internal/platform"
)

const (
	consoleCookieName = "golab_admin_session"
	consoleSessionTTL = 8 * time.Hour
)

func (s *Server) initConsoleSessionKey(secret string) error {
	if secret == "" {
		_, err := rand.Read(s.consoleSessionKey[:])
		return err
	}
	s.consoleSessionKey = sha256.Sum256([]byte("golab console session v1\x00" + secret))
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
	http.SetCookie(w, &http.Cookie{Name: consoleCookieName, Value: sealed, Path: "/api/v1/admin", HttpOnly: true,
		Secure: consoleSecureRequest(r), SameSite: http.SameSiteStrictMode, MaxAge: int(consoleSessionTTL.Seconds())})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"name": tok.Name})
}

func (s *Server) handleConsoleSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"name": actorFrom(r).Name})
}

func (s *Server) handleConsoleLogout(w http.ResponseWriter, r *http.Request) {
	if !sameOriginRequest(r) {
		writeError(w, http.StatusForbidden, "bad_origin", "same-origin request required")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: consoleCookieName, Path: "/api/v1/admin", HttpOnly: true,
		Secure: consoleSecureRequest(r), SameSite: http.SameSiteStrictMode, MaxAge: -1})
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"status": "signed_out"})
}
