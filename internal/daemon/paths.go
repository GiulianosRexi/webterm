// Package daemon es el proceso dueño de los ptys: los spawnea, los mantiene
// vivos entre reinicios del orquestador y baja su output a SQLite.
//
// Es deliberadamente flaco. No sabe de títulos, KV, recursos externos ni
// tokens: todo eso vive en el orquestador, que puede reiniciarse cuantas veces
// quiera sin tocar lo que corre acá.
package daemon

import (
	"path/filepath"
	"strings"
)

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
func PathsFor(dbPath string) Paths {
	base := strings.TrimSuffix(dbPath, filepath.Ext(dbPath))
	return Paths{
		Socket: base + ".sock",
		Lock:   base + ".lock",
		Log:    base + ".log",
	}
}
