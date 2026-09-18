package server

import (
	"html/template"
	"net/http"
	"time"
)

// loginPath es la única ruta que el middleware de auth deja pasar.
const loginPath = "/api/login"

// loginTmpl es la pantalla de login. Va embebida acá (y no en el bundle de
// React) para que el frontend no tenga que saber nada de autenticación: si
// llegaste a la SPA, ya estás adentro.
var loginTmpl = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="es">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="color-scheme" content="dark">
<title>WebTerm</title>
<style>
  :root { color-scheme: dark; }
  * { box-sizing: border-box; }
  body {
    margin: 0; min-height: 100dvh; display: flex; align-items: center; justify-content: center;
    background: #11131a; color: #d6dae4; padding: 24px;
    font-family: ui-sans-serif, -apple-system, "SF Pro Text", system-ui, sans-serif;
  }
  form { width: 100%; max-width: 340px; }
  h1 { font-size: 20px; margin: 0 0 4px; letter-spacing: .02em; }
  p { margin: 0 0 20px; color: #838a9c; font-size: 14px; }
  label { display: block; font-size: 13px; color: #838a9c; margin-bottom: 6px; }
  input {
    width: 100%; font: inherit; font-size: 16px; /* 16px evita el zoom automático en iOS */
    padding: 12px 14px; border-radius: 10px; border: 1px solid #272b38;
    background: #181b24; color: #d6dae4;
  }
  input:focus { outline: none; border-color: #5ac8a8; }
  button {
    width: 100%; margin-top: 12px; font: inherit; font-size: 15px; font-weight: 600;
    padding: 12px 14px; border-radius: 10px; border: 0;
    background: #5ac8a8; color: #11131a; cursor: pointer;
  }
  button:active { opacity: .85; }
  .error { color: #e2696b; font-size: 13px; margin: 12px 0 0; }
</style>
</head>
<body>
  <form method="post" action="` + loginPath + `">
    <h1>WebTerm</h1>
    <p>Entrá para abrir una terminal.</p>
    <label for="token">Token de acceso</label>
    <input id="token" name="token" type="password" autocomplete="current-password"
           autofocus required autocapitalize="off" autocorrect="off" spellcheck="false">
    <button type="submit">Entrar</button>
    {{if .Error}}<p class="error">{{.Error}}</p>{{end}}
  </form>
</body>
</html>`))

// renderLogin escribe la pantalla de login con el status que corresponda.
func renderLogin(w http.ResponseWriter, status int, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = loginTmpl.Execute(w, struct{ Error string }{errMsg})
}

// handleLogin valida el token del formulario y deja la cookie de sesión.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Token == "" {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if r.Method != http.MethodPost {
		renderLogin(w, http.StatusOK, "")
		return
	}
	if err := r.ParseForm(); err != nil {
		renderLogin(w, http.StatusBadRequest, "No se pudo leer el formulario.")
		return
	}
	if !s.tokenOK(r.PostFormValue("token")) {
		// Frenar el tanteo por fuerza bruta desde la red local.
		time.Sleep(time.Second)
		renderLogin(w, http.StatusUnauthorized, "Token incorrecto.")
		return
	}
	s.setSessionCookie(w, r)
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleLogout borra la cookie y devuelve al login.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}
