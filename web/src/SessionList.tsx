import { useEffect, useState } from 'react'
import { createPortal } from 'react-dom'
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
  onCollapse,
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
  onCollapse: () => void
}) {
  const [editing, setEditing] = useState<string | null>(null)
  const [draft, setDraft] = useState('')
  // El menú guarda el id, no la sesión: así el poll de App puede cambiarle el
  // pty_status por debajo y los items se recalculan solos.
  const [menu, setMenu] = useState<{ id: string; x: number; y: number } | null>(null)

  // Se cierra con Escape, con un click afuera y con cualquier cosa que lo
  // desalinee de la fila que lo abrió (scroll de la lista, resize de la
  // ventana): está en position: fixed, no sigue a la fila.
  useEffect(() => {
    if (!menu) return
    const close = () => setMenu(null)
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') close()
    }
    window.addEventListener('pointerdown', close)
    window.addEventListener('resize', close)
    window.addEventListener('scroll', close, true)
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('pointerdown', close)
      window.removeEventListener('resize', close)
      window.removeEventListener('scroll', close, true)
      window.removeEventListener('keydown', onKey)
    }
  }, [menu])

  // x/y son la esquina superior izquierda deseada; SessionMenu solo la corrige
  // si el menú no entra en la ventana.
  const openMenu = (id: string, x: number, y: number) => setMenu({ id, x, y })

  const startRename = (s: Session) => {
    setEditing(s.id)
    setDraft(s.title || label(s))
  }

  const commit = (id: string) => {
    setEditing(null)
    const title = draft.trim()
    if (title) onRename(id, title)
  }

  return (
    <aside className="sidebar">
      <div className="sidebar-head">
        <span>Sesiones</span>
        <button onClick={onCreate} disabled={busy} title="Nueva sesión" aria-label="Nueva sesión">
          +
        </button>
        <button className="collapse" onClick={onCollapse} title="Ocultar la lista de sesiones">
          ⟨
        </button>
      </div>

      <ul className="session-list">
        {sessions.length === 0 && <li className="empty">todavía no hay ninguna</li>}
        {sessions.map((s) => (
          <li
            key={s.id}
            className={'session' + (s.id === selectedId ? ' selected' : '')}
            onClick={() => onSelect(s.id)}
            onContextMenu={(e) => {
              e.preventDefault()
              openMenu(s.id, e.clientX, e.clientY)
            }}
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
                  startRename(s)
                }}
                title={`${s.cwd} · doble click para renombrar`}
              >
                <span className="name-inner">{label(s)}</span>
              </span>
            )}
            <span className="when">{relative(s.last_active_at)}</span>
            {s.pty_status === 'starting' && (
              <span className="starting-hint" title="La sesión está arrancando">
                …
              </span>
            )}
            <span className="actions" onClick={(e) => e.stopPropagation()}>
              <button
                className="menu-trigger"
                title="Acciones de la sesión"
                aria-label="Acciones de la sesión"
                aria-haspopup="menu"
                onClick={(e) => {
                  const r = e.currentTarget.getBoundingClientRect()
                  openMenu(s.id, r.right - MENU_WIDTH, r.bottom + 4)
                }}
              >
                ⋯
              </button>
            </span>
          </li>
        ))}
      </ul>

      {menu && (
        <SessionMenu
          session={sessions.find((s) => s.id === menu.id) ?? null}
          x={menu.x}
          y={menu.y}
          busy={busy}
          onClose={() => setMenu(null)}
          onRename={startRename}
          onKill={onKill}
          onRestart={onRestart}
          onDelete={onDelete}
        />
      )}
    </aside>
  )
}

const MENU_WIDTH = 180
const MENU_ITEM_H = 28

function SessionMenu({
  session,
  x,
  y,
  busy,
  onClose,
  onRename,
  onKill,
  onRestart,
  onDelete,
}: {
  session: Session | null
  x: number
  y: number
  busy: boolean
  onClose: () => void
  onRename: (s: Session) => void
  onKill: (id: string) => void
  onRestart: (id: string) => void
  onDelete: (id: string) => void
}) {
  // La sesión puede haber desaparecido entre que se abrió el menú y este
  // render: el poll corre cada 3s y pudo borrarla otra pestaña.
  if (!session) return null

  const run = (fn: () => void) => () => {
    onClose()
    fn()
  }

  // starting no ofrece ni Parar ni Reanudar: Restart verifica contra el daemon
  // que la sesión no esté viva y spawnea de nuevo, y dispararlo mientras el
  // spawn original sigue en vuelo podría chocar con él. Es transitorio, así que
  // alcanza con esperar. Borrar sí queda, porque no le exige nada al pty y es
  // la única forma de cancelar una sesión pegada sin esperar el sweep.
  const items: { label: string; onClick: () => void; danger?: boolean }[] = [
    { label: 'Renombrar', onClick: run(() => onRename(session)) },
  ]
  if (session.pty_status === 'running') {
    items.push({ label: 'Parar', onClick: run(() => onKill(session.id)) })
  }
  if (session.pty_status === 'exited') {
    items.push({ label: 'Reanudar', onClick: run(() => onRestart(session.id)) })
  }
  items.push({ label: 'Borrar', onClick: run(() => onDelete(session.id)), danger: true })

  // Alto estimado a partir de los items (más el separador y el padding) para
  // poder voltear el menú hacia arriba antes de pintarlo, sin un frame en el
  // que se vea desbordando la ventana.
  const height = items.length * MENU_ITEM_H + 9 + 8
  const left = Math.max(8, Math.min(x, window.innerWidth - MENU_WIDTH - 8))
  const top = y + height > window.innerHeight - 8 ? Math.max(8, y - height - 8) : y

  return createPortal(
    <div
      className="session-menu"
      role="menu"
      style={{ left, top, width: MENU_WIDTH }}
      onPointerDown={(e) => e.stopPropagation()}
      onClick={(e) => e.stopPropagation()}
      onContextMenu={(e) => e.preventDefault()}
    >
      {items.map((item, i) => (
        <div key={item.label}>
          {item.danger && i > 0 && <hr />}
          <button role="menuitem" className={item.danger ? 'danger' : ''} disabled={busy} onClick={item.onClick}>
            {item.label}
          </button>
        </div>
      ))}
    </div>,
    document.body,
  )
}
