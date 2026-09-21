import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type CSSProperties,
  type PointerEvent as ReactPointerEvent,
} from 'react'
import { TerminalView, type ConnState } from './TerminalView'
import { SessionList } from './SessionList'
import { ResourcePanel } from './ResourcePanel'
import { api, type Session } from './api'
import { useEvents, type ServerEvent } from './useEvents'
import { CommandPalette } from './CommandPalette'

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
const SIDEBAR_WIDTH_KEY = 'webterm.sidebarWidth'
const SIDEBAR_COLLAPSED_KEY = 'webterm.sidebarCollapsed'

// El rango existe para que el drag no deje la sidebar inusable: por debajo de
// 180px no entra el head y por encima de 480px le come el ancho al terminal.
const SIDEBAR_MIN = 180
const SIDEBAR_MAX = 480
const SIDEBAR_DEFAULT = 240

const clampWidth = (px: number) => Math.min(SIDEBAR_MAX, Math.max(SIDEBAR_MIN, px))

// El ancho guardado puede venir de una versión con otros límites, o directamente
// corrupto si alguien tocó el localStorage, así que se valida y se clampea.
function storedWidth(): number {
  const raw = Number(localStorage.getItem(SIDEBAR_WIDTH_KEY))
  return Number.isFinite(raw) && raw > 0 ? clampWidth(raw) : SIDEBAR_DEFAULT
}

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
  const [sidebarWidth, setSidebarWidth] = useState(storedWidth)
  const [paletteOpen, setPaletteOpen] = useState(false)
  // El handler del atajo se registra una sola vez, así que lee el estado de
  // acá en lugar de capturarlo en su closure —mismo patrón que selectedRef.
  const paletteOpenRef = useRef(paletteOpen)
  paletteOpenRef.current = paletteOpen
  const [collapsed, setCollapsed] = useState(
    () => localStorage.getItem(SIDEBAR_COLLAPSED_KEY) === '1',
  )
  const selectedRef = useRef(selected)
  selectedRef.current = selected
  // Mismo patrón que selectedRef: el handler del drag se crea una sola vez, así
  // que lee el ancho de acá en vez de capturarlo en su closure.
  const widthRef = useRef(sidebarWidth)
  widthRef.current = sidebarWidth

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

  // Al cerrar el buscador el foco tiene que volver a la terminal. Sin esto
  // queda en el body y el usuario no puede tipear: la peor forma de romper una
  // terminal. xterm escucha en un textarea propio, que es el que hay que
  // enfocar.
  const closePalette = useCallback(() => {
    setPaletteOpen(false)
    requestAnimationFrame(() => {
      document.querySelector<HTMLTextAreaElement>('.xterm-helper-textarea')?.focus()
    })
  }, [])

  // Cmd+K (Ctrl+K fuera de macOS) abre el buscador. Va en fase de captura
  // porque si no xterm se come la tecla: con el foco dentro de la terminal es
  // él quien recibe el teclado, y el evento nunca llegaría hasta acá.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key.toLowerCase() !== 'k' || !(e.metaKey || e.ctrlKey)) return
      e.preventDefault()
      e.stopPropagation()
      // Cerrar con el atajo tiene que devolver el foco igual que cerrar con
      // Escape, así que pasa por closePalette y no por setPaletteOpen.
      if (paletteOpenRef.current) closePalette()
      else setPaletteOpen(true)
    }
    window.addEventListener('keydown', onKey, true)
    return () => window.removeEventListener('keydown', onKey, true)
  }, [closePalette])

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

  useEffect(() => {
    localStorage.setItem(SIDEBAR_WIDTH_KEY, String(sidebarWidth))
  }, [sidebarWidth])

  useEffect(() => {
    localStorage.setItem(SIDEBAR_COLLAPSED_KEY, collapsed ? '1' : '0')
  }, [collapsed])

  // El drag usa pointer events con capture: así el puntero puede pasar por
  // encima del canvas de xterm sin que el arrastre se pierda, y anda igual con
  // trackpad y touch. El ancho no necesita throttle propio porque el fit() del
  // terminal ya va por requestAnimationFrame vía su ResizeObserver.
  const startResize = useCallback((e: ReactPointerEvent<HTMLDivElement>) => {
    if (e.button !== 0) return
    e.preventDefault()
    const handle = e.currentTarget
    const startX = e.clientX
    const startWidth = widthRef.current
    handle.setPointerCapture(e.pointerId)
    document.body.classList.add('resizing')

    const move = (ev: PointerEvent) => setSidebarWidth(clampWidth(startWidth + ev.clientX - startX))
    const stop = (ev: PointerEvent) => {
      handle.releasePointerCapture(ev.pointerId)
      handle.removeEventListener('pointermove', move)
      handle.removeEventListener('pointerup', stop)
      handle.removeEventListener('pointercancel', stop)
      document.body.classList.remove('resizing')
    }

    handle.addEventListener('pointermove', move)
    handle.addEventListener('pointerup', stop)
    handle.addEventListener('pointercancel', stop)
  }, [])

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

      <div
        className="layout"
        style={{ '--sidebar-w': `${sidebarWidth}px` } as CSSProperties}
      >
        {!collapsed && (
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
            onCollapse={() => setCollapsed(true)}
          />
        )}

        {collapsed ? (
          <div className="sidebar-resizer collapsed">
            <button
              className="sidebar-reveal"
              onClick={() => setCollapsed(false)}
              title="Mostrar la lista de sesiones"
            >
              ⟩
            </button>
          </div>
        ) : (
          <div
            className="sidebar-resizer"
            onPointerDown={startResize}
            onDoubleClick={() => setSidebarWidth(SIDEBAR_DEFAULT)}
            role="separator"
            aria-orientation="vertical"
            aria-label="Ancho de la lista de sesiones"
            aria-valuenow={sidebarWidth}
            aria-valuemin={SIDEBAR_MIN}
            aria-valuemax={SIDEBAR_MAX}
            title="Arrastrar para redimensionar · doble click para restaurar"
          />
        )}
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

      {paletteOpen && (
        <CommandPalette
          sessions={sessions}
          selectedId={selected}
          onSelect={setSelected}
          onClose={closePalette}
        />
      )}
    </div>
  )
}
