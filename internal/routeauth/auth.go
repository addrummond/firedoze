package routeauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	CookieName        = "firedoze_route_auth"
	KeySize           = 32
	defaultRuntimeDir = "/run/firedoze"
)

type Manager struct {
	keyPath string
	logger  *slog.Logger
	mu      sync.Mutex
	key     []byte
}

func NewManager(keyPath string, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{keyPath: keyPath, logger: logger}
}

func KeyPath(stateDir string) string {
	return filepath.Join(stateDir, "route_auth.key")
}

func RuntimeKeyPath() string {
	runtimeDir := os.Getenv("RUNTIME_DIRECTORY")
	if runtimeDir == "" {
		runtimeDir = defaultRuntimeDir
	}
	if first, _, ok := strings.Cut(runtimeDir, ":"); ok {
		runtimeDir = first
	}
	return filepath.Join(runtimeDir, "route_auth.key")
}

func (m *Manager) Load() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key, loaded, err := loadOrGenerateKey(m.keyPath)
	if err != nil {
		return err
	}
	m.key = key
	if loaded {
		if err := os.Remove(m.keyPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		m.logger.Info("loaded and removed route auth key", "path", m.keyPath)
	}
	return nil
}

func (m *Manager) Save() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.key) != KeySize {
		return fmt.Errorf("route auth key has %d bytes, want %d", len(m.key), KeySize)
	}
	if err := os.MkdirAll(filepath.Dir(m.keyPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(m.keyPath, m.key, 0o600); err != nil {
		return err
	}
	return nil
}

func (m *Manager) Approved(r *http.Request, host string) bool {
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return false
	}
	_, ok := m.Validate(cookie.Value, host)
	return ok
}

func (m *Manager) SetCookie(w http.ResponseWriter, host string, expires time.Time) error {
	token, err := m.Token(host, expires)
	if err != nil {
		return err
	}
	maxAge := int(time.Until(expires).Seconds())
	if maxAge < 0 {
		maxAge = 0
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   maxAge,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

func (m *Manager) SignedURL(host string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		return "", fmt.Errorf("ttl must be positive")
	}
	expires := time.Now().Add(ttl)
	token, err := m.Token(host, expires)
	if err != nil {
		return "", err
	}
	u := url.URL{
		Scheme: "https",
		Host:   host,
		Path:   "/_firedoze/auth",
	}
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (m *Manager) Token(host string, expires time.Time) (string, error) {
	key, err := m.keyBytes()
	if err != nil {
		return "", err
	}
	hostPart := encodePart(host)
	expiryPart := encodePart(strconv.FormatInt(expires.Unix(), 10))
	message := hostPart + "." + expiryPart
	return message + "." + sign(key, message), nil
}

func (m *Manager) Validate(token string, host string) (time.Time, bool) {
	key, err := m.keyBytes()
	if err != nil {
		m.logger.Warn("load route auth key", "error", err)
		return time.Time{}, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	tokenHost, err := decodePart(parts[0])
	if err != nil || tokenHost != host {
		return time.Time{}, false
	}
	expiryText, err := decodePart(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	expiryUnix, err := strconv.ParseInt(expiryText, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	expires := time.Unix(expiryUnix, 0)
	if time.Now().After(expires) {
		return time.Time{}, false
	}
	message := parts[0] + "." + parts[1]
	if !validSignature(key, message, parts[2]) {
		return time.Time{}, false
	}
	return expires, true
}

func (m *Manager) keyBytes() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.key) == KeySize {
		return append([]byte(nil), m.key...), nil
	}
	key, _, err := loadOrGenerateKey(m.keyPath)
	if err != nil {
		return nil, err
	}
	m.key = key
	return append([]byte(nil), m.key...), nil
}

func loadOrGenerateKey(path string) ([]byte, bool, error) {
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != KeySize {
			return nil, false, fmt.Errorf("%s has %d bytes, want %d", path, len(key), KeySize)
		}
		return key, true, nil
	}
	if !os.IsNotExist(err) {
		return nil, false, err
	}
	key = make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, false, err
	}
	return key, false, nil
}

func sign(key []byte, message string) string {
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

func encodePart(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodePart(value string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}
