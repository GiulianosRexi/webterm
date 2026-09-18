package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"net/url"
	"strings"
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// El login tiene que ser alcanzable justamente sin estar autenticado.
		if r.URL.Path == loginPath {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(cookieName); err == nil && s.tokenOK(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		// Authorization: Bearer, que es lo que sabe mandar un cliente de API.
		// Ni la cookie ni el ?token= le sirven al servidor MCP.
		if tok, ok := bearerToken(r); ok && s.tokenOK(tok) {
			next.ServeHTTP(w, r)
			return
		}
		// Atajo: ?token=... entra directo y deja la cookie, para poder mandarse
		// un link ya autenticado.
		if q := r.URL.Query().Get("token"); s.tokenOK(q) {
			s.setSessionCookie(w, r)
			clean := *r.URL
			params := clean.Query()
			params.Del("token")
			clean.RawQuery = params.Encode()
			http.Redirect(w, r, clean.RequestURI(), http.StatusFound)
			return
		}
		// Una navegación del browser ve la pantalla de login; el WebSocket y
		// las llamadas de API, un 401 pelado.
		if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/html") {
			renderLogin(w, http.StatusUnauthorized, "")
			return
		}
		http.Error(w, "no autorizado", http.StatusUnauthorized)
	})
}

// bearerToken saca el token de un header Authorization: Bearer.
func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefijo = "Bearer "
	if len(h) <= len(prefijo) || !strings.EqualFold(h[:len(prefijo)], prefijo) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefijo):]), true
}

// tokenOK compara en tiempo constante contra el token configurado.
func (s *Server) tokenOK(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) == 1
}

// setSessionCookie deja la sesión guardada en el browser. Sin Secure porque
// servimos HTTP plano en la LAN; detrás de TLS conviene activarlo.
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    s.cfg.Token,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().AddDate(0, 1, 0),
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
