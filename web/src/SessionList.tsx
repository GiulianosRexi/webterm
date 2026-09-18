import { useState } from 'react'
import type { Session } from './api'

function label(s: Session): string {
  return s.title.trim() || s.cwd.split('/').filter(Boolean).pop() || s.id.slice(-6)
}

// relative formatea el "hace cuánto" de la última actividad, que es lo que
// más rápido dice cuál de todas las sesiones importa ahora.
function relative(ms: number): string {
  const secs = Math.max(0, Math.round((Date.now() - ms) / 1000))
  if (secs < 60) return 'recién'
  if (secs < 3600) return `hace ${Math.floor(secs / 60)} min`
  if (secs < 86400) return `hace ${Math.floor(secs / 3600)} h`
  return `hace ${Math.floor(secs / 86400)} d`
}

export function SessionList({
  sessions,
  selectedId,
  busy,
  onSelect,
  onCreate,
  onRename,
  onKill,
  onRestart,
  onDelete,
}: {
  sessions: Session[]
  selectedId: string | null
  busy: boolean
  onSelect: (id: string) => void
  onCreate: () => void
  onRename: (id: string, title: string) => void
  onKill: (id: string) => void
  onRestart: (id: string) => void
  onDelete: (id: string) => void
}) {
  const [editing, setEditing] = useState<string | null>(null)
  const [draft, setDraft] = useState('')

  const commit = (id: string) => {
    setEditing(null)
    const title = draft.trim()
    if (title) onRename(id, title)
  }

  return (
    <aside className="sidebar">
      <div className="sidebar-head">
        <span>Sesiones</span>
        <button onClick={onCreate} disabled={busy} title="Nueva sesión">
          + Nueva
        </button>
      </div>

      <ul className="session-list">
        {sessions.length === 0 && <li className="empty">todavía no hay ninguna</li>}
        {sessions.map((s) => (
          <li
            key={s.id}
            className={'session' + (s.id === selectedId ? ' selected' : '')}
            onClick={() => onSelect(s.id)}
          >
            <span className="dot" data-status={s.pty_status} />
            {editing === s.id ? (
              <input
                className="rename"
                autoFocus
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onBlur={() => commit(s.id)}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') commit(s.id)
                  if (e.key === 'Escape') setEditing(null)
                }}
                onClick={(e) => e.stopPropagation()}
              />
            ) : (
              <span
                className="name"
                onDoubleClick={(e) => {
                  e.stopPropagation()
                  setEditing(s.id)
                  setDraft(s.title || label(s))
                }}
                title={`${s.cwd} · doble click para renombrar`}
              >
                {label(s)}
              </span>
            )}
            <span className="when">{relative(s.last_active_at)}</span>
            <span className="actions" onClick={(e) => e.stopPropagation()}>
              {s.pty_status === 'running' && (
                <button onClick={() => onKill(s.id)} disabled={busy} title="Matar el proceso">
                  ■
                </button>
              )}
              {s.pty_status === 'exited' && (
                <button onClick={() => onRestart(s.id)} disabled={busy} title="Reanudar">
                  ▶
                </button>
              )}
              {/* starting es la ventana en la que el orquestador ya pidió el
                  spawn y todavía no supo si el daemon lo confirmó. Ni Matar
                  ni Reanudar tienen sentido ahí: Restart verifica contra el
                  daemon que la sesión no esté viva y spawnea de nuevo, y
                  dispararlo acá podría chocar con el spawn que ya está en
                  vuelo. Es transitorio (el poll de App refresca solo) así
                  que alcanza con mostrar que está arrancando. */}
              {s.pty_status === 'starting' && (
                <span className="starting-hint" title="La sesión está arrancando">
                  …
                </span>
              )}
              {/* Borrar sí queda disponible en starting, a diferencia de las
                  otras dos acciones: Delete no le exige nada al daemon sobre
                  el pty (Kill tolera que todavía no exista) y solo borra la
                  fila. Si el spawn en vuelo termina después de este borrado,
                  sus updates a la fila ya borrada son un no-op silencioso
                  (ErrNotFound, contemplado en el backend). Es la única forma
                  de cancelar una sesión que quedó pegada arrancando sin
                  esperar los 30s del sweep. */}
              <button
                className="danger"
                onClick={() => onDelete(s.id)}
                disabled={busy}
                title="Borrar la sesión y su historial"
              >
                ✕
              </button>
            </span>
          </li>
        ))}
      </ul>
    </aside>
  )
}
