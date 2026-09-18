package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"net/url"
	"time"
)

// cookieName guarda el token en el browser para no tener que arrastrarlo en
// la URL después del primer acceso.
const cookieName = "webterm_token"

// NewToken genera un token de acceso aleatorio.
func NewToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand no falla en la práctica; si falla, mejor no arrancar.
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// IsLoopback dice si addr escucha solo en la máquina local.
func IsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" {
		return false // ":7788" escucha en todas las interfaces
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// withAuth exige el token en cada request. El token puede venir como cookie o
// como ?token=... en la URL; en el segundo caso lo guardamos en una cookie y
// redirigimos a la URL limpia, así no queda dando vueltas en la barra.
//
// Cubre también /ws/terminal: el browser manda la cookie en el handshake.
func (s *Server) withAuth(next http.Handler) http.Handler {
	if s.cfg.Token == "" {
		return next
	}
	want := []byte(s.cfg.Token)

	ok := func(got string) bool {
		return subtle.ConstantTimeCompare([]byte(got), want) == 1
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(cookieName); err == nil && ok(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		if q := r.URL.Query().Get("token"); ok(q) {
			http.SetCookie(w, &http.Cookie{
				Name:     cookieName,
				Value:    q,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
				Expires:  time.Now().AddDate(0, 1, 0),
			})
			clean := *r.URL
			params := clean.Query()
			params.Del("token")
			clean.RawQuery = params.Encode()
			http.Redirect(w, r, clean.RequestURI(), http.StatusFound)
			return
		}
		http.Error(w, "no autorizado: falta ?token=...", http.StatusUnauthorized)
	})
}

// sameOrigin rechaza handshakes de WebSocket originados en otra página. Sin
// esto, un sitio cualquiera abierto en tu browser podría conectarse al backend
// usando tu cookie y quedarse con una shell.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // cliente no-browser (curl, tests): no hay cookie que robar.
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return u.Host == r.Host
}
