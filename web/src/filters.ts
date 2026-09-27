import type { Session } from './api'
import { KANBAN_STATUSES, statusOf, type KanbanStatus } from './session'

// SessionFilters son los filtros de la lista de sesiones. Cada campo es un
// tipo de filtro, y null significa "sin filtrar por esto": así sumar uno nuevo
// es agregar un campo, su chequeo en matches y su entrada en el menú, sin
// tocar a los demás.
export interface SessionFilters {
  // Los estados que se muestran. null = todos.
  statuses: KanbanStatus[] | null
}

export const NO_FILTERS: SessionFilters = { statuses: null }

const STORAGE_KEY = 'webterm.sessionFilters'

// isFiltering dice si hay algo que achique la lista: es lo que prende el
// indicador del botón.
export function isFiltering(f: SessionFilters): boolean {
  return f.statuses !== null
}

export function matches(s: Session, f: SessionFilters): boolean {
  if (f.statuses !== null && !f.statuses.includes(statusOf(s))) return false
  return true
}

// setStatuses normaliza la elección: tener todos tildados es lo mismo que no
// filtrar, y guardarlo como null evita que un estado nuevo quede escondido
// solo porque no existía cuando se armó el filtro.
export function setStatuses(f: SessionFilters, statuses: KanbanStatus[]): SessionFilters {
  const all = KANBAN_STATUSES.every((st) => statuses.includes(st))
  return { ...f, statuses: all ? null : KANBAN_STATUSES.filter((st) => statuses.includes(st)) }
}

// loadFilters lee lo guardado y descarta lo que no reconoce: el localStorage
// puede venir de otra versión o estar tocado a mano.
export function loadFilters(): SessionFilters {
  try {
    const raw = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? 'null') as Partial<SessionFilters> | null
    if (!raw || !Array.isArray(raw.statuses)) return NO_FILTERS
    const known = raw.statuses.filter((st): st is KanbanStatus =>
      (KANBAN_STATUSES as readonly string[]).includes(st),
    )
    return setStatuses(NO_FILTERS, known)
  } catch {
    return NO_FILTERS
  }
}

export function saveFilters(f: SessionFilters): void {
  localStorage.setItem(STORAGE_KEY, JSON.stringify(f))
}
