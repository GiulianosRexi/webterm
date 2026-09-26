// starting es la ventana entre que el orquestador inserta la fila y el daemon
// confirma el spawn (ver internal/store/session.go). Normal que dure
// milisegundos, pero el frontend tiene que saber que no es lo mismo que
// exited: no hay proceso todavía, pero tampoco terminó nada.
export type PtyStatus = 'running' | 'exited' | 'starting'

export interface Session {
  id: string
  title: string
  description: string
  folder_id: string | null
  cwd: string
  shell: string
  cols: number
  rows: number
  pty_status: PtyStatus
  exit_reason?: string
  exit_code?: number
  work_status: string
  kanban_status: string
  created_at: number
  last_active_at: number
  exited_at?: number
  // Tipo de trabajo —bugfix, consulta, implementación—. Vienen normalizados
  // del backend (minúsculas, guiones) y ordenados.
  tags: string[]
}

async function req<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
  if (!res.ok) {
    // El backend manda {"error":"..."}; si no, nos quedamos con el status.
    let msg = `${res.status} ${res.statusText}`
    try {
      const body = (await res.json()) as { error?: string }
      if (body.error) msg = body.error
    } catch {
      /* respuesta sin JSON */
    }
    throw new Error(msg)
  }
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

// Folder agrupa sesiones. Un folder es un proyecto: no se anidan.
export interface Folder {
  id: string
  name: string
  created_at: number
}

export const api = {
  list: () => req<Session[]>('/api/sessions'),
  create: (body: { title?: string; cwd?: string; cols: number; rows: number }) =>
    req<Session>('/api/sessions', { method: 'POST', body: JSON.stringify(body) }),
  rename: (id: string, title: string) =>
    req<Session>(`/api/sessions/${id}`, { method: 'PATCH', body: JSON.stringify({ title }) }),
  kill: (id: string) => req<Session>(`/api/sessions/${id}/kill`, { method: 'POST' }),
  restart: (id: string, cols: number, rows: number) =>
    req<Session>(`/api/sessions/${id}/restart`, {
      method: 'POST',
      body: JSON.stringify({ cols, rows }),
    }),
  remove: (id: string) => req<void>(`/api/sessions/${id}`, { method: 'DELETE' }),
  // sessions es opcional a propósito: si el daemon no contesta, el backend lo
  // omite en vez de mandar un 0 que sería mentira, y avisa con
  // daemon: 'unreachable'. El tipo obliga a distinguir "no hay sesiones" de
  // "no se sabe cuántas hay".
  health: () =>
    req<{
      status: string
      auth: boolean
      daemon?: 'ok' | 'unreachable'
      daemon_error?: string
      sessions?: number
    }>('/api/health'),
  folders: {
    list: () => req<Folder[]>('/api/folders'),
    create: (name: string) =>
      req<Folder>('/api/folders', { method: 'POST', body: JSON.stringify({ name }) }),
    rename: (id: string, name: string) =>
      req<void>(`/api/folders/${id}`, { method: 'PATCH', body: JSON.stringify({ name }) }),
    remove: (id: string) => req<void>(`/api/folders/${id}`, { method: 'DELETE' }),
    // folderId null saca la sesión de su folder. Va en un endpoint propio y no
    // en el PATCH de la sesión porque ahí "no tocar" y "sacala" serían lo
    // mismo: ausente y null llegan igual al backend.
    setSession: (sessionId: string, folderId: string | null) =>
      req<void>(`/api/sessions/${sessionId}/folder`, {
        method: 'PUT',
        body: JSON.stringify({ folder_id: folderId }),
      }),
  },
  // tags reemplaza el conjunto entero y devuelve la sesión con los tags ya
  // normalizados. No hay endpoint para listar los existentes porque la UI ya
  // tiene todas las sesiones: se derivan de ahí (ver allTags en session.ts).
  tags: {
    set: (sessionId: string, tags: string[]) =>
      req<Session>(`/api/sessions/${sessionId}/tags`, {
        method: 'PUT',
        body: JSON.stringify({ tags }),
      }),
  },
  // El contexto persistido de una sesión: el key/value que Claude escribe por
  // MCP (set_context) y que la UI ahora también deja ver y editar. list devuelve
  // el objeto plano {clave: valor}. set manda el valor crudo en el body porque
  // así lo espera el handler (más cómodo que envolverlo en JSON).
  kv: {
    list: (sessionId: string) =>
      req<Record<string, string>>(`/api/sessions/${sessionId}/kv`),
    set: (sessionId: string, key: string, value: string) =>
      req<void>(`/api/sessions/${sessionId}/kv/${encodeURIComponent(key)}`, {
        method: 'PUT',
        body: value,
      }),
    remove: (sessionId: string, key: string) =>
      req<void>(`/api/sessions/${sessionId}/kv/${encodeURIComponent(key)}`, {
        method: 'DELETE',
      }),
  },
  resources: {
    list: (sessionId: string) =>
      req<LinkedResource[]>(`/api/sessions/${sessionId}/resources`),
    link: (sessionId: string, ref: string) =>
      req<LinkedResource>(`/api/sessions/${sessionId}/resources`, {
        method: 'POST',
        body: JSON.stringify({ ref }),
      }),
    unlink: (sessionId: string, id: number) =>
      req<void>(`/api/sessions/${sessionId}/resources/${id}`, { method: 'DELETE' }),
  },
}

export interface PRState {
  number: number
  repo: string
  title: string
  author: string
  state: 'OPEN' | 'CLOSED' | 'MERGED'
  is_draft: boolean
  mergeable: string
  review_decision: string
  unresolved_count: number
  threads_truncated: boolean
  checks_state: string
  checks_total: number
  checks_success: number
  checks_failing: number
  checks_pending: number
  checks_expected: number
  checks_skipped: number
  checks_cancelled: number
  checks_neutral: number
  checks_other: number
}

export interface Snapshot {
  fetched_at: number
  error?: string
  pr?: PRState
}

export interface LinkedResource {
  id: number
  session_id: string
  system: string
  type: string
  ref: string
  created_at: number
  snapshot?: Snapshot
}
