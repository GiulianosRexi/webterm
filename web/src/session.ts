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

// relative formatea el "hace cuánto" de la última actividad, que es lo que
// más rápido dice cuál de todas las sesiones importa ahora.
export function relative(ms: number): string {
  const secs = Math.max(0, Math.round((Date.now() - ms) / 1000))
  if (secs < 60) return 'recién'
  if (secs < 3600) return `hace ${Math.floor(secs / 60)} min`
  if (secs < 86400) return `hace ${Math.floor(secs / 3600)} h`
  return `hace ${Math.floor(secs / 86400)} d`
}

// KanbanStatus es el estado de trabajo de una sesión. El orden es el del
// backend (store.KanbanStatuses): el que sigue el trabajo, y el que usan el
// selector y el buscador. "todo" es "todavía no arrancó" por compatibilidad:
// es el default con el que el daemon crea las sesiones.
export const KANBAN_STATUSES = [
  'todo',
  'in_progress',
  'blocked',
  'in_review',
  'needs_testing',
  'done',
] as const

export type KanbanStatus = (typeof KANBAN_STATUSES)[number]

export const STATUS_LABEL: Record<KanbanStatus, string> = {
  todo: 'Not started',
  in_progress: 'WIP',
  blocked: 'Blocked',
  in_review: 'In Review',
  needs_testing: 'Needs Testing',
  done: 'Done',
}

// statusOf tolera valores que el frontend no conoce (una base escrita por otra
// versión): los trata como "todavía no arrancó" en vez de romper el render.
export function statusOf(s: Session): KanbanStatus {
  return (KANBAN_STATUSES as readonly string[]).includes(s.kanban_status)
    ? (s.kanban_status as KanbanStatus)
    : 'todo'
}

// WorkStatus es qué está haciendo Claude ahora en la sesión. Lo mueven los
// hooks de Claude Code (control.nextWorkStatus); idle es también el estado de
// toda sesión donde no corre Claude.
export type WorkStatus = 'idle' | 'working' | 'waiting_input' | 'error' | 'subagents'

const WORK_STATUSES: readonly WorkStatus[] = ['idle', 'working', 'waiting_input', 'error', 'subagents']

// agentsCount es "1 subagente" / "2 subagentes": se usa en la descripción larga.
function agentsCount(n: number): string {
  return n === 1 ? '1 subagente' : `${n} subagentes`
}

// workLabel es la descripción larga del estado, para el tooltip.
export function workLabel(status: WorkStatus, agents: number): string {
  switch (status) {
    case 'idle':
      return 'Idle'
    case 'working':
      return 'Claude está trabajando'
    case 'waiting_input':
      return 'Claude espera tu respuesta'
    case 'error':
      return 'El turno terminó con un error de la API'
    case 'subagents':
      return `Claude terminó; esperando a ${agentsCount(agents)}`
  }
}

// workOf solo reporta estado para un pty vivo: si el proceso murió, lo que
// haya quedado escrito es de un Claude que ya no existe. También tolera
// valores desconocidos igual que statusOf.
export function workOf(s: Session): WorkStatus {
  if (s.pty_status !== 'running') return 'idle'
  return (WORK_STATUSES as readonly string[]).includes(s.work_status)
    ? (s.work_status as WorkStatus)
    : 'idle'
}
