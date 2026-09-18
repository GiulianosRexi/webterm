package ptyapi

import (
	"bytes"
	"testing"
)

func TestSanitizeReplay(t *testing.T) {
	if got := SanitizeReplay(nil); got != nil {
		t.Fatalf("vacío dio %q; quería nil", got)
	}
	// Un corte en medio de un carácter multibyte: "ñ" es 0xC3 0xB1, y el tail
	// arranca en el continuation byte.
	got := SanitizeReplay([]byte{0xB1, 'h', 'o', 'l', 'a'})
	if !bytes.HasSuffix(got, []byte("hola")) {
		t.Fatalf("no descartó el byte de continuación: %q", got)
	}
	if !bytes.HasPrefix(got, []byte("\x1b[0m")) {
		t.Fatalf("no antepuso el reset de atributos: %q", got)
	}
}
