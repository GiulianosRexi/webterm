import { useCallback, useEffect, useRef, useState } from 'react'
import { TerminalView, type ConnState } from './TerminalView'
import { SessionList } from './SessionList'
import { ResourcePanel } from './ResourcePanel'
import { api, type Session } from './api'
import { useEvents, type ServerEvent } from './useEvents'

const label: Record<ConnState, string> = {
  connecting: 'conectando…',
  open: 'conectado',
  readonly: 'solo lectura',
  closed: 'desconectado',
  exited: 'sesión terminada',
  // Distinto de "conectando…": ahí el socket es el que no está listo; acá el
  // socket ya contestó pero el daemon todavía no confirmó el pty.
  starting: 'arrancando…',
}

// El refresco en vivo llega por /api/events; este poll queda de red de
// seguridad para el caso en que el stream se caiga sin que el browser lo note.
const POLL_MS = 60000
const LAST_SESSION_KEY = 'webterm.lastSession'

export function App() {
  const [sessions, setSessions] = useState<Session[]>([])
  const [selected, setSelected] = useState<string | null>(() =>
    localStorage.getItem(LAST_SESSION_KEY),
  )
  const [state, setState] = useState<ConnState>('connecting')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [auth, setAuth] = useState(false)
  // Un contador en vez de un booleano: lo que le importa a ResourcePanel es que
  // cambió, no qué valor tiene.
  const [resourceTick, setResourceTick] = useState(0)
  const selectedRef = useRef(selected)
  selectedRef.current = selected

  const refresh = useCallback(async () => {
    try {
      const list = await api.list()
      setSessions(list)
      setError(null)
      // Si la sesión elegida ya no existe (la borramos, o es de otra máquina),
      // caemos a la primera de la lista.
      if (!list.some((s) => s.id === selectedRef.current)) {
        setSelected(list[0]?.id ?? null)
      }
    } catch (err) {
      setError(String(err))
    }
  }, [])

  useEvents(
    useCallback(
      (ev: ServerEvent | null) => {
        // null es resync: no sabemos qué nos perdimos, así que se refresca todo.
        if (!ev) {
          void refresh()
          setResourceTick((t) => t + 1)
          return
        }
        if (ev.kind.startsWith('session.')) void refresh()
        if (ev.kind.startsWith('resource.') && ev.session_id === selectedRef.current) {
          setResourceTick((t) => t + 1)
        }
      },
      [refresh],
    ),
  )

  useEffect(() => {
    void refresh()
    const t = setInterval(() => void refresh(), POLL_MS)
    return () => clearInterval(t)
  }, [refresh])

  // El bus nunca avisa la muerte espontánea de un pty (el daemon la escribe
  // sincrónicamente, así que el sweep del orquestador nunca la encuentra),
  // pero el socket de la terminal ya se entera al instante. Este efecto
  // reusa ese aviso para refrescar la lista sin esperar los 60s del poll.
  useEffect(() => {
    if (state === 'exited') void refresh()
  }, [state, refresh])

  useEffect(() => {
    api
      .health()
      .then((h) => setAuth(h.auth))
      .catch(() => {})
  }, [])

  useEffect(() => {
    if (selected) localStorage.setItem(LAST_SESSION_KEY, selected)
    else localStorage.removeItem(LAST_SESSION_KEY)
  }, [selected])

  // run envuelve las acciones del ABM: una sola a la vez, error visible y
  // refresco inmediato en vez de esperar al próximo poll.
  const run = useCallback(
    async (fn: () => Promise<unknown>) => {
      setBusy(true)
      try {
        await fn()
        setError(null)
      } catch (err) {
        setError(String(err))
      } finally {
        setBusy(false)
        await refresh()
      }
    },
    [refresh],
  )

  const create = () =>
    run(async () => {
      const s = await api.create({ cols: 80, rows: 24 })
      setSelected(s.id)
    })

  const remove = (id: string) =>
    run(async () => {
      if (!confirm('¿Borrar la sesión y todo su historial?')) return
      await api.remove(id)
      if (selectedRef.current === id) setSelected(null)
    })

  const current = sessions.find((s) => s.id === selected) ?? null

  return (
    <div className="app">
      <header className="topbar">
        <span className="brand">WebTerm</span>
        {current && <span className="cwd">{current.cwd}</span>}
        <span className="status" data-state={state}>
          <span className="dot" />
          {label[state]}
        </span>
        <span className="spacer" />
        {error && <span className="error">{error}</span>}
        {auth && (
          <a className="button" href="/api/logout">
            Salir
          </a>
        )}
      </header>

      <div className="layout">
        <SessionList
          sessions={sessions}
          selectedId={selected}
          busy={busy}
          onSelect={setSelected}
          onCreate={create}
          onRename={(id, title) => run(() => api.rename(id, title))}
          onKill={(id) => run(() => api.kill(id))}
          onRestart={(id) => run(() => api.restart(id, 80, 24))}
          onDelete={remove}
        />
        <main className="main">
          {selected ? (
            <>
              <ResourcePanel key={'res-' + selected} sessionId={selected} reloadKey={resourceTick} />
              {/* key fuerza un remount al cambiar de sesión: cada una tiene su
                  propio xterm y su propio socket. */}
              <TerminalView key={selected} sessionId={selected} onState={setState} />
            </>
          ) : (
            <div className="placeholder">
              No hay ninguna sesión abierta. Creá una con <b>+ Nueva</b>.
            </div>
          )}
        </main>
      </div>
    </div>
  )
}
