package proxy

import (
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"firedoze/internal/config"
	"firedoze/internal/routeauth"

	"github.com/dchest/captcha"
)

const (
	wakeGateCookieName = routeauth.CookieName
	wakeGateCookieTTL  = 30 * 24 * time.Hour
	captchaWidth       = 240
	captchaHeight      = 80
	captchaLength      = 5
)

type wakeGate struct {
	auth   *routeauth.Manager
	logger *slog.Logger
}

func newWakeGate(cfg config.Config, auth *routeauth.Manager, logger *slog.Logger) *wakeGate {
	if auth == nil {
		auth = routeauth.NewManager(routeauth.KeyPath(cfg.StateDir), logger)
	}
	return &wakeGate{
		auth:   auth,
		logger: logger,
	}
}

func (g *wakeGate) approved(r *http.Request, host string) bool {
	return g.auth.Approved(r, host)
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
	expires := time.Now().Add(wakeGateCookieTTL)
	if err := g.auth.SetCookie(w, host, expires); err != nil {
		g.logger.Warn("load wake gate key", "error", err)
		http.Error(w, "wake gate unavailable", http.StatusServiceUnavailable)
		return
	}
	next := r.Form.Get("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}
