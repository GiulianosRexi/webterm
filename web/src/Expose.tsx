import { useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Terminal } from '@xterm/xterm'
import { Folder as FolderIcon, Search } from 'lucide-react'
import type { Folder, Session } from './api'
import { relative, sessionLabel } from './session'
import { rank } from './fuzzy'

// Vista tipo Exposé: todas las sesiones a la vez, cada una con una miniatura
// en vivo de su terminal. Es de solo mirar: las miniaturas nunca mandan input
// ni resize, así que abrir el Exposé no le cambia nada a ninguna sesión —ni el
// tamaño del pty, que lo sigue decidiendo la terminal de verdad—.

const theme = {
  background: '#141414',
  foreground: '#e4e4e4',
  cursor: '#141414',
  selectionBackground: '#3a3a3a',
}

// Cuánto dura el brillo de "está pasando algo" después del último output.
const HOT_MS = 1500

interface Grupo {
  key: string
  folder: Folder | null
  sessions: Session[]
}

export function Expose({
  sessions,
  folders,
  selectedId,
  onSelect,
  onClose,
}: {
  sessions: Session[]
  folders: Folder[]
  selectedId: string | null
  onSelect: (id: string) => void
  onClose: () => void
}) {
  const [query, setQuery] = useState('')
  const [active, setActive] = useState<string | null>(selectedId)
  const gridRef = useRef<HTMLDivElement>(null)

  // Sin búsqueda se agrupa por folder, lo más reciente primero adentro de cada
  // uno. Con búsqueda el orden lo decide el match y los grupos sobran: se
  // aplana todo en uno solo, igual que en el palette.
  const grupos = useMemo<Grupo[]>(() => {
    const folderDe = (s: Session) => folders.find((f) => f.id === s.folder_id) ?? null
    if (query.trim()) {
      const hits = rank(
        query,
        sessions,
        (s) => `${sessionLabel(s)} ${folderDe(s)?.name ?? ''} ${s.tags.join(' ')} ${s.cwd}`,
      ).map((r) => r.item)
      return [{ key: 'resultados', folder: null, sessions: hits }]
    }
    const recientes = [...sessions].sort((a, b) => b.last_active_at - a.last_active_at)
    const out: Grupo[] = folders
      .map((f) => ({
        key: f.id,
        folder: f,
        sessions: recientes.filter((s) => s.folder_id === f.id),
      }))
      .filter((g) => g.sessions.length > 0)
    const sueltas = recientes.filter((s) => !folderDe(s))
    if (sueltas.length > 0) out.push({ key: '__sin_folder__', folder: null, sessions: sueltas })
    return out
  }, [query, sessions, folders])

  const orden = useMemo(() => grupos.flatMap((g) => g.sessions.map((s) => s.id)), [grupos])

  // La selección tiene que apuntar siempre a algo visible: al filtrar puede
  // quedar afuera, y ahí salta al primer resultado.
  useEffect(() => {
    if (!active || !orden.includes(active)) setActive(orden[0] ?? null)
  }, [orden, active])

  useEffect(() => {
    gridRef.current
      ?.querySelector(`[data-id="${active}"]`)
      ?.scrollIntoView({ block: 'nearest', behavior: 'smooth' })
  }, [active])

  const abrir = (id: string) => {
    onSelect(id)
    onClose()
  }

  // mover resuelve las flechas mirando dónde quedó cada tile en pantalla y no
  // una cuenta de columnas: la grilla es fluida y además está partida en
  // secciones, así que "la de abajo" puede estar en el folder siguiente.
  const mover = (dir: 'up' | 'down' | 'left' | 'right') => {
    if (dir === 'left' || dir === 'right') {
      const i = active ? orden.indexOf(active) : -1
      const next = orden[i + (dir === 'right' ? 1 : -1)]
      if (next) setActive(next)
      return
    }
    const tiles = [...(gridRef.current?.querySelectorAll<HTMLElement>('.expose-tile') ?? [])]
    const cur = tiles.find((t) => t.dataset.id === active)
    if (!cur) return
    const r = cur.getBoundingClientRect()
    const cx = r.left + r.width / 2
    const candidatos = tiles
      .map((t) => ({ t, r: t.getBoundingClientRect() }))
      .filter(({ r: o }) => (dir === 'down' ? o.top > r.top + 4 : o.top < r.top - 4))
    if (candidatos.length === 0) return
    // La fila más cercana primero, y dentro de ella la columna más alineada.
    const filaTop =
      dir === 'down'
        ? Math.min(...candidatos.map((c) => c.r.top))
        : Math.max(...candidatos.map((c) => c.r.top))
    const fila = candidatos.filter((c) => Math.abs(c.r.top - filaTop) < 4)
    fila.sort(
      (a, b) =>
        Math.abs(a.r.left + a.r.width / 2 - cx) - Math.abs(b.r.left + b.r.width / 2 - cx),
    )
    setActive(fila[0].t.dataset.id ?? null)
  }

  const onKeyDown = (e: React.KeyboardEvent) => {
    const flechas: Record<string, 'up' | 'down' | 'left' | 'right'> = {
      ArrowUp: 'up',
      ArrowDown: 'down',
      ArrowLeft: 'left',
      ArrowRight: 'right',
    }
    if (e.key === 'Escape') {
      e.preventDefault()
      if (query) setQuery('')
      else onClose()
      return
    }
    if (e.key === 'Enter') {
      e.preventDefault()
      if (active) abrir(active)
      return
    }
    // Izquierda y derecha son del cursor del campo mientras haya texto: si no,
    // no habría forma de corregir la búsqueda.
    const dir = flechas[e.key]
    if (dir && (!query || dir === 'up' || dir === 'down')) {
      e.preventDefault()
      mover(dir)
    }
  }

  const corriendo = sessions.filter((s) => s.pty_status === 'running').length

  return createPortal(
    <div className="expose" role="dialog" aria-modal="true" aria-label="Todas las sesiones">
      <header className="expose-head">
        <div className="expose-title">
          <span>Sesiones</span>
          <span className="expose-count">
            <span className="dot" data-status="running" /> {corriendo} corriendo ·{' '}
            {sessions.length - corriendo} quietas
          </span>
        </div>
        <label className="expose-search">
          <Search size={14} />
          <input
            autoFocus
            value={query}
            placeholder="Filtrar por nombre, folder, tag o cwd…"
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={onKeyDown}
            // El foco no puede irse del campo: es el que escucha el teclado.
            onBlur={(e) => e.currentTarget.focus()}
          />
        </label>
        <span className="expose-hints">
          <kbd>←↑↓→</kbd> moverse <kbd>↵</kbd> abrir <kbd>esc</kbd> cerrar
        </span>
      </header>

      <div
        className="expose-grid-wrap"
        ref={gridRef}
        // Click en el vacío cierra, como en el Exposé de macOS.
        onMouseDown={(e) => e.target === e.currentTarget && onClose()}
      >
        {orden.length === 0 && <div className="expose-empty">sin resultados</div>}
        {grupos.map((g) => (
          <section key={g.key} className="expose-section">
            {!query.trim() && (g.folder || grupos.length > 1) && (
              <h2 className="expose-section-title">
                {g.folder ? (
                  <>
                    <FolderIcon size={13} /> {g.folder.name}
                  </>
                ) : (
                  'Sin folder'
                )}
                <span>{g.sessions.length}</span>
              </h2>
            )}
            <div className="expose-grid">
              {g.sessions.map((s) => (
                <Tile
                  key={s.id}
                  session={s}
                  current={s.id === selectedId}
                  active={s.id === active}
                  onHover={() => setActive(s.id)}
                  onOpen={() => abrir(s.id)}
                />
              ))}
            </div>
          </section>
        ))}
      </div>
    </div>,
    document.body,
  )
}

function Tile({
  session: s,
  current,
  active,
  onHover,
  onOpen,
}: {
  session: Session
  current: boolean
  active: boolean
  onHover: () => void
  onOpen: () => void
}) {
  const tileRef = useRef<HTMLButtonElement>(null)
  return (
    <button
      ref={tileRef}
      className="expose-tile"
      data-id={s.id}
      data-active={active}
      data-current={current}
      data-status={s.pty_status}
      onMouseEnter={onHover}
      onClick={onOpen}
      tabIndex={-1}
    >
      <MiniTerm session={s} tileRef={tileRef} />
      <div className="expose-tile-foot">
        <span className="dot" data-status={s.pty_status} />
        <span className="expose-tile-name">{sessionLabel(s)}</span>
        {s.tags.map((t) => (
          <span key={t} className="palette-tag">
            {t}
          </span>
        ))}
        <span className="expose-tile-when">
          {s.pty_status === 'exited' ? 'terminó' : relative(s.last_active_at)}
        </span>
      </div>
      {/* rtl para que el recorte se coma el principio del path y quede a la
          vista el final, que es lo que distingue una sesión de otra; bdi
          evita que la barra inicial salte al otro extremo. */}
      <div className="expose-tile-cwd">
        <bdi>{s.cwd}</bdi>
      </div>
    </button>
  )
}

// MiniTerm es una terminal de solo lectura dibujada al tamaño real de la
// sesión y achicada con transform para que entre en el tile. Achicar en vez de
// usar una grilla más chica es lo que la hace fiel: con otras columnas el
// replay reflowearía distinto y los TUIs (vim, htop, claude) se verían rotos.
function MiniTerm({
  session,
  tileRef,
}: {
  session: Session
  tileRef: React.RefObject<HTMLButtonElement | null>
}) {
  const boxRef = useRef<HTMLDivElement>(null)
  const hostRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const [visible, setVisible] = useState(false)

  // Se conecta recién cuando el tile entra en pantalla: cada socket trae hasta
  // 1 MiB de historial, y con muchas sesiones no tiene sentido pagarlo por las
  // que están scrolleadas fuera. Una vez conectada se queda conectada.
  useEffect(() => {
    const box = boxRef.current
    if (!box || visible) return
    const io = new IntersectionObserver((entries) => {
      if (entries.some((e) => e.isIntersecting)) setVisible(true)
    })
    io.observe(box)
    return () => io.disconnect()
  }, [visible])

  const fit = () => {
    const box = boxRef.current
    const host = hostRef.current
    const screen = host?.querySelector<HTMLElement>('.xterm-screen')
    if (!box || !host || !screen || screen.offsetWidth === 0) return
    const scale = Math.min(
      box.clientWidth / screen.offsetWidth,
      box.clientHeight / screen.offsetHeight,
    )
    host.style.transform = `scale(${scale})`
  }

  useEffect(() => {
    const host = hostRef.current
    const tile = tileRef.current
    if (!visible || !host || !tile) return

    const term = new Terminal({
      theme,
      cols: Math.max(1, session.cols),
      rows: Math.max(1, session.rows),
      fontFamily: '"SF Mono", Menlo, ui-monospace, monospace',
      fontSize: 13,
      // Nada de scrollback: solo se ve la pantalla actual, y el replay entero
      // en memoria por cada miniatura sería puro desperdicio.
      scrollback: 0,
      disableStdin: true,
      cursorBlink: false,
      cursorInactiveStyle: 'none',
    })
    // Sin WebGL a propósito: el browser limita los contextos WebGL por página
    // (~16) y con más sesiones que eso las primeras se quedarían en negro.
    term.open(host)
    termRef.current = term
    requestAnimationFrame(fit)

    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const ws = new WebSocket(
      `${proto}//${location.host}/ws/terminal?session_id=${encodeURIComponent(session.id)}`,
    )
    ws.binaryType = 'arraybuffer'

    // Hasta "ready" lo que llega es el replay, que no es actividad: sin esto
    // todos los tiles brillarían a la vez al abrir.
    let replayed = false
    let hot = 0
    ws.onmessage = (ev) => {
      if (typeof ev.data !== 'string') {
        term.write(new Uint8Array(ev.data as ArrayBuffer))
        if (replayed) {
          // Directo al DOM y no por estado: el output llega decenas de veces
          // por segundo y re-renderizar el tile por cada chunk no aporta nada.
          tile.dataset.hot = 'true'
          clearTimeout(hot)
          hot = window.setTimeout(() => delete tile.dataset.hot, HOT_MS)
        }
        return
      }
      try {
        if ((JSON.parse(ev.data) as { type: string }).type === 'ready') replayed = true
      } catch {
        /* mensaje de control que no nos importa */
      }
    }

    const observer = new ResizeObserver(fit)
    observer.observe(boxRef.current!)

    return () => {
      clearTimeout(hot)
      delete tile.dataset.hot
      observer.disconnect()
      ws.onmessage = null
      ws.close()
      term.dispose()
      termRef.current = null
    }
    // Las dimensiones se siguen en su propio effect: reconectar por un resize
    // volvería a bajar todo el historial.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [visible, session.id])

  // Si la sesión cambia de tamaño con el Exposé abierto, la miniatura la sigue.
  useLayoutEffect(() => {
    const term = termRef.current
    if (!term) return
    if (term.cols !== session.cols || term.rows !== session.rows) {
      term.resize(Math.max(1, session.cols), Math.max(1, session.rows))
      requestAnimationFrame(fit)
    }
  }, [session.cols, session.rows])

  return (
    <div className="expose-preview" ref={boxRef}>
      <div className="expose-preview-host" ref={hostRef} />
    </div>
  )
}
