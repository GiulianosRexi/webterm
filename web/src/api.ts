export type PtyStatus = 'running' | 'exited'

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
  health: () => req<{ status: string; auth: boolean; sessions: number }>('/api/health'),
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
  checks_failing: number
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
