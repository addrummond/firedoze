package proxy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"firedoze/internal/config"

	"github.com/dchest/captcha"
)

const (
	wakeGateCookieName = "firedoze_wake"
	wakeGateCookieTTL  = 30 * 24 * time.Hour
	wakeGateKeySize    = 32
	captchaWidth       = 240
	captchaHeight      = 80
	captchaLength      = 5
)

type wakeGate struct {
	keyPath string
	logger  *slog.Logger
	once    sync.Once
	key     []byte
	err     error
}

func newWakeGate(cfg config.Config, logger *slog.Logger) *wakeGate {
	return &wakeGate{
		keyPath: filepath.Join(cfg.StateDir, "wake_gate.key"),
		logger:  logger,
	}
}

func (g *wakeGate) approved(r *http.Request, host string) bool {
	cookie, err := r.Cookie(wakeGateCookieName)
	if err != nil {
		return false
	}
	key, err := g.signingKey()
	if err != nil {
		g.logger.Warn("load wake gate key", "error", err)
		return false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 3 {
		return false
	}
	cookieHost, err := decodeCookiePart(parts[0])
	if err != nil || cookieHost != host {
		return false
	}
	expiryText, err := decodeCookiePart(parts[1])
	if err != nil {
		return false
	}
	expiry, err := strconv.ParseInt(expiryText, 10, 64)
	if err != nil || time.Now().Unix() > expiry {
		return false
	}
	message := parts[0] + "." + parts[1]
	if !validSignature(key, message, parts[2]) {
		return false
	}
	return true
}

func (g *wakeGate) handle(w http.ResponseWriter, r *http.Request, host string) bool {
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/_firedoze/wake-captcha/"):
		g.serveCaptcha(w, r)
		return true
	case r.Method == http.MethodPost && r.URL.Path == "/_firedoze/wake":
		g.verify(w, r, host)
		return true
	case r.Method == http.MethodGet:
		g.challenge(w, r)
		return true
	default:
		http.Error(w, "firedoze wake captcha required", http.StatusForbidden)
		return true
	}
}

func (g *wakeGate) challenge(w http.ResponseWriter, r *http.Request) {
	id := captcha.NewLen(captchaLength)
	next := r.URL.RequestURI()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Are you human?</title>
  <style>
    body { font-family: system-ui, sans-serif; margin: 2rem; line-height: 1.4; max-width: 34rem; }
    img { display: block; margin: 1rem 0; border: 1px solid #ccc; }
    input, button { font: inherit; padding: .5rem .6rem; }
  </style>
</head>
<body>
  <h1>Are you human?</h1>
  <form method="post" action="/_firedoze/wake">
    <input type="hidden" name="id" value="%s">
    <input type="hidden" name="next" value="%s">
    <img src="/_firedoze/wake-captcha/%s.png" width="%d" height="%d" alt="CAPTCHA">
    <label>Type the digits shown above</label><br>
    <input name="answer" inputmode="numeric" autocomplete="off" required autofocus>
    <button type="submit">Continue</button>
  </form>
</body>
</html>
`, html.EscapeString(id), html.EscapeString(next), html.EscapeString(id), captchaWidth, captchaHeight)
}

func (g *wakeGate) serveCaptcha(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/_firedoze/wake-captcha/")
	id := strings.TrimSuffix(name, ".png")
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	if err := captcha.WriteImage(w, id, captchaWidth, captchaHeight); err != nil {
		http.Error(w, "captcha not found", http.StatusNotFound)
	}
}

func (g *wakeGate) verify(w http.ResponseWriter, r *http.Request, host string) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid captcha form", http.StatusBadRequest)
		return
	}
	id := r.Form.Get("id")
	answer := r.Form.Get("answer")
	if !captcha.VerifyString(id, answer) {
		http.Error(w, "captcha failed", http.StatusForbidden)
		return
	}
	key, err := g.signingKey()
	if err != nil {
		g.logger.Warn("load wake gate key", "error", err)
		http.Error(w, "wake gate unavailable", http.StatusServiceUnavailable)
		return
	}
	expires := time.Now().Add(wakeGateCookieTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     wakeGateCookieName,
		Value:    signedWakeCookie(key, host, expires),
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(wakeGateCookieTTL.Seconds()),
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	next := r.Form.Get("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (g *wakeGate) signingKey() ([]byte, error) {
	g.once.Do(func() {
		g.key, g.err = ensureWakeGateKey(g.keyPath)
	})
	return g.key, g.err
}

func ensureWakeGateKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != wakeGateKeySize {
			return nil, fmt.Errorf("%s has %d bytes, want %d", path, len(key), wakeGateKeySize)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, wakeGateKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func signedWakeCookie(key []byte, host string, expires time.Time) string {
	hostPart := encodeCookiePart(host)
	expiryPart := encodeCookiePart(strconv.FormatInt(expires.Unix(), 10))
	message := hostPart + "." + expiryPart
	return message + "." + signCookieValue(key, message)
}

func signCookieValue(key []byte, message string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func validSignature(key []byte, message string, encodedSignature string) bool {
	got, err := base64.RawURLEncoding.DecodeString(encodedSignature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	return hmac.Equal(got, mac.Sum(nil))
}

func encodeCookiePart(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeCookiePart(value string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}
