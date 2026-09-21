import type { Session } from './api'

// sessionLabel decide con qué nombre se muestra una sesión cuando todavía no
// tiene título: primero el último tramo del cwd, y si tampoco sirve, un pedazo
// del id. Vive acá y no en la lista porque el palette necesita exactamente el
// mismo nombre —si divergieran, buscar por un texto encontraría una sesión que
// en pantalla se llama de otra forma.
export function sessionLabel(s: Session): string {
  return s.title.trim() || s.cwd.split('/').filter(Boolean).pop() || s.id.slice(-6)
}
