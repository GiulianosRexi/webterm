// Package daemonclient habla con el daemon por su socket Unix.
//
// Implementa ptyapi.Client igual que el manager en proceso, así que el
// orquestador no sabe cuál de los dos tiene enfrente.
package daemonclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/gorilla/websocket"

	"github.com/giuliano/webterm/internal/daemon"
	"github.com/giuliano/webterm/internal/ptyapi"
	"github.com/giuliano/webterm/internal/store"
)

// ErrProtocolMismatch dice que el daemon que está corriendo quedó viejo. Es el
// precio de tener un solo binario con dos modos: recompilar no reemplaza al
// proceso que ya corre.
var ErrProtocolMismatch = errors.New("el daemon corriendo habla otra versión del protocolo")

// El host de las URLs es irrelevante —se disca siempre el mismo socket— pero
// http.Client necesita una URL bien formada.
const baseURL = "http://daemon"

// Client es el lado cliente del socket del daemon.
type Client struct {
	socket string
	http   *http.Client
	dialer *websocket.Dialer
}

func New(socketPath string) *Client {
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}
	return &Client{
		socket: socketPath,
		http:   &http.Client{Transport: &http.Transport{DialContext: dial}},
		dialer: &websocket.Dialer{
			NetDialContext:  dial,
			ReadBufferSize:  4 * 1024,
			WriteBufferSize: 32 * 1024,
		},
	}
}

// Info pregunta quién está del otro lado.
func (c *Client) Info() (daemon.Info, error) {
	var info daemon.Info
	res, err := c.http.Get(baseURL + "/info")
	if err != nil {
		return info, fmt.Errorf("consultando el daemon en %s: %w", c.socket, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return info, fmt.Errorf("el daemon contestó %d a /info", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
		return info, fmt.Errorf("respuesta de /info ilegible: %w", err)
	}
	return info, nil
}

// Check verifica que del otro lado haya un daemon que hable nuestro protocolo.
// El orquestador lo corre antes de escuchar: es mejor no arrancar que arrancar
// y fallar raro a mitad de un attach.
func (c *Client) Check() error {
	info, err := c.Info()
	if err != nil {
		return err
	}
	if info.ProtocolVersion != daemon.ProtocolVersion {
		return fmt.Errorf("%w: el daemon (pid %d) habla %d y este binario habla %d",
			ErrProtocolMismatch, info.PID, info.ProtocolVersion, daemon.ProtocolVersion)
	}
	return nil
}

func (c *Client) Spawn(o ptyapi.SpawnOpts) error {
	body, err := json.Marshal(o)
	if err != nil {
		return err
	}
	res, err := c.http.Post(baseURL+"/sessions", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("spawneando %s: %w", o.ID, err)
	}
	defer res.Body.Close()
	return statusError(res)
}

func (c *Client) Kill(id string) error {
	res, err := c.http.Post(baseURL+"/sessions/"+id+"/kill", "application/json", nil)
	if err != nil {
		return fmt.Errorf("matando %s: %w", id, err)
	}
	defer res.Body.Close()
	return statusError(res)
}

func (c *Client) LiveIDs() ([]string, error) {
	res, err := c.http.Get(baseURL + "/sessions")
	if err != nil {
		return nil, fmt.Errorf("listando sesiones vivas: %w", err)
	}
	defer res.Body.Close()
	if err := statusError(res); err != nil {
		return nil, err
	}
	var out struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.IDs, nil
}

// Close suelta las conexiones ociosas. No toca las sesiones del daemon: el
// orquestador se va, los ptys se quedan, que es todo el punto de M10.
func (c *Client) Close() error {
	c.http.CloseIdleConnections()
	return nil
}

// statusError reconstruye el error del contrato a partir del status.
//
// Son cuatro casos y no tres: al 404/410/409 del brief original se le suma el
// 503 de ptyapi.ErrClosed (el dueño de los ptys se está apagando). La tabla
// completa —y el porqué de cada status— vive documentada en ptyapi.go; este
// switch es solo su reflejo en el lado cliente, no una fuente aparte.
func statusError(res *http.Response) error {
	switch res.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return store.ErrNotFound
	case http.StatusGone:
		return ptyapi.ErrNotLive
	case http.StatusConflict:
		return ptyapi.ErrAlreadyLive
	case http.StatusServiceUnavailable:
		return ptyapi.ErrClosed
	default:
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		return fmt.Errorf("el daemon contestó %d: %s", res.StatusCode, bytes.TrimSpace(msg))
	}
}

var _ ptyapi.Client = (*Client)(nil)
