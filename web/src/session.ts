import type { Session } from './api'

// sessionLabel decide con qué nombre se muestra una sesión cuando todavía no
// tiene título: primero el último tramo del cwd, y si tampoco sirve, un pedazo
// del id. Vive acá y no en la lista porque el palette necesita exactamente el
// mismo nombre —si divergieran, buscar por un texto encontraría una sesión que
// en pantalla se llama de otra forma.
export function sessionLabel(s: Session): string {
  return s.title.trim() || s.cwd.split('/').filter(Boolean).pop() || s.id.slice(-6)
}

// normalizeTag replica la normalización del backend (store.NormalizeTag) para
// que el autocompletado compare contra lo mismo que se va a guardar: si no,
// escribir "Bug fix" no reconocería que "bug-fix" ya existe.
export function normalizeTag(tag: string): string {
  return tag.trim().split(/\s+/).filter(Boolean).join('-').toLowerCase()
}

// TagCount es un tag en uso con cuántas sesiones lo llevan.
export interface TagCount {
  name: string
  count: number
}

// allTags deriva de las sesiones los tags existentes, los más usados primero:
// es el orden que sirve para el autocompletado, donde lo que importa es
// ofrecer primero el tag que ya se viene usando.
export function allTags(sessions: Session[]): TagCount[] {
  const counts = new Map<string, number>()
  for (const s of sessions) for (const t of s.tags ?? []) counts.set(t, (counts.get(t) ?? 0) + 1)
  return [...counts]
    .map(([name, count]) => ({ name, count }))
    .sort((a, b) => b.count - a.count || a.name.localeCompare(b.name))
}
