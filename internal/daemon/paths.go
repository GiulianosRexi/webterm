// Package daemon es el proceso dueño de los ptys: los spawnea, los mantiene
// vivos entre reinicios del orquestador y baja su output a SQLite.
//
// Es deliberadamente flaco. No sabe de títulos, KV, recursos externos ni
// tokens: todo eso vive en el orquestador, que puede reiniciarse cuantas veces
// quiera sin tocar lo que corre acá.
package daemon

import (
	"fmt"
	"path/filepath"
	"strings"
)

// maxSocketPathLen es el largo máximo utilizable de un path de socket Unix.
//
// sockaddr_un.sun_path mide 104 bytes en macOS y hay que dejarle lugar al
// terminador nulo, así que el string que se puede pasar entra en 103 bytes;
// en Linux el campo es más grande (108). Usamos el límite del sistema más
// chico entre los que soporta el proyecto: un -db que entra acá entra en los
// dos, y no al revés.
const maxSocketPathLen = 103

// Paths son los archivos del daemon para una base dada.
type Paths struct {
	Socket string // socket Unix donde escucha
	Lock   string // flock que evita dos daemons para la misma base
	Log    string // stdout y stderr del daemon
}

// PathsFor deriva las rutas del path de la base.
//
// Derivarlas en vez de fijarlas es lo que aísla una instancia de desarrollo:
// con rutas fijas, un `-db dev.db` en otro puerto se conectaría igual al
// daemon de producción y le spawnearía y mataría sesiones.
//
// Devuelve error si el socket derivado no entra en un sockaddr_un. Se chequea
// acá y no en Serve porque acá es donde el usuario todavía puede hacer algo:
// antes del flock, antes de intentar el bind, y con el path completo a mano
// para explicar qué pasó. Después de esto el fallo sería un "bind: invalid
// argument" desnudo, que no dice ni el largo ni el límite ni la salida.
func PathsFor(dbPath string) (Paths, error) {
	base := strings.TrimSuffix(dbPath, filepath.Ext(dbPath))
	p := Paths{
		Socket: base + ".sock",
		Lock:   base + ".lock",
		Log:    base + ".log",
	}
	if len(p.Socket) > maxSocketPathLen {
		return Paths{}, fmt.Errorf(
			"el socket derivado de -db mide %d caracteres (%s), pero un socket Unix no admite más de %d: usá un -db más corto",
			len(p.Socket), p.Socket, maxSocketPathLen)
	}
	return p, nil
}
