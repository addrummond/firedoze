package routeauth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagerLoadSaveTokenAndCookie(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "route_auth.key")
	manager := NewManager(keyPath, nil)
	if err := manager.Load(); err != nil {
		t.Fatal(err)
	}
	if err := manager.Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %o, want 600", info.Mode().Perm())
	}

	reloaded := NewManager(keyPath, nil)
	if err := reloaded.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("loaded key file was not removed: %v", err)
	}

	host := "demo.example.test"
	token, err := reloaded.Token(host, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Validate(token, host); !ok {
		t.Fatal("fresh token was not valid")
	}
	if _, ok := reloaded.Validate(token, "other.example.test"); ok {
		t.Fatal("token was valid for wrong host")
	}
	expired, err := reloaded.Token(host, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Validate(expired, host); ok {
		t.Fatal("expired token was valid")
	}
	if _, ok := reloaded.Validate("bad.cookie.parts", host); ok {
		t.Fatal("malformed token was valid")
	}

	resp := httptest.NewRecorder()
	if err := reloaded.SetCookie(resp, host, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "https://"+host+"/", nil)
	req.AddCookie(resp.Result().Cookies()[0])
	if !reloaded.Approved(req, host) {
		t.Fatal("request with fresh cookie was not approved")
	}

	signedURL, err := reloaded.SignedURL(host, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(signedURL, "https://"+host+"/_firedoze/auth?token=") {
		t.Fatalf("signed url = %q", signedURL)
	}
}

func TestManagerRejectsShortSavedKey(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "route_auth.key")
	if err := os.WriteFile(keyPath, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewManager(keyPath, nil).Load(); err == nil {
		t.Fatal("Load accepted short key")
	}
}

func TestRuntimeKeyPathUsesSystemdRuntimeDirectory(t *testing.T) {
	t.Setenv("RUNTIME_DIRECTORY", "/run/firedoze:/run/other")
	if got, want := RuntimeKeyPath(), "/run/firedoze/route_auth.key"; got != want {
		t.Fatalf("RuntimeKeyPath = %q, want %q", got, want)
	}
}
