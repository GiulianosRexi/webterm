package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadOrCreateToken lee el token de disco y, si no hay, genera uno y lo guarda.
//
// Persistirlo es un requisito de M10, no una comodidad: con sesiones que
// sobreviven al reinicio del orquestador, un token nuevo por arranque invalida
// el WEBTERM_TOKEN que quedó inyectado en los ptys vivos y rompe el servidor
// MCP justo adentro de las sesiones que sobrevivieron.
func LoadOrCreateToken(path string) (string, error) {
	switch data, err := os.ReadFile(path); {
	case err == nil:
		// Un archivo vacío o con solo espacios es basura, no un token:
		// devolverlo sería arrancar con la auth efectivamente desactivada.
		if tok := strings.TrimSpace(string(data)); tok != "" {
			return tok, nil
		}
	case !os.IsNotExist(err):
		return "", fmt.Errorf("leyendo el token de %s: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("creando %s: %w", dir, err)
		}
	}
	tok := NewToken()
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("guardando el token en %s: %w", path, err)
	}
	return tok, nil
}
