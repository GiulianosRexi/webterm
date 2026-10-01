import {
  useCallback,
  useRef,
  useState,
  type CSSProperties,
  type PointerEvent as ReactPointerEvent,
} from 'react'
import { FileText, Link2, PanelRightOpen, PanelRightClose } from 'lucide-react'
import { LinksTab } from './ResourcePanel'
import { ContextPanel } from './ContextPanel'

type Tab = 'context' | 'links'

const WIDTH_KEY = 'webterm.rightWidth'
const COLLAPSED_KEY = 'webterm.rightCollapsed'
const TAB_KEY = 'webterm.rightTab'
const MARKDOWN_KEY = 'webterm.contextMarkdown'

// Por debajo del mínimo no entra el contenido. Máximo no tiene: a veces el
// contexto importa más que el terminal, y el doble click lo devuelve al default.
const MIN = 220
const DEFAULT = 340

const clampWidth = (px: number) => Math.max(MIN, px)

function storedWidth(): number {
  const raw = Number(localStorage.getItem(WIDTH_KEY))
  return Number.isFinite(raw) && raw > 0 ? clampWidth(raw) : DEFAULT
}

export function RightPanel({
  sessionId,
  sessionName,
  reloadKey,
}: {
  sessionId: string
  sessionName: string
  reloadKey: number
}) {
  const [collapsed, setCollapsed] = useState(
    () => localStorage.getItem(COLLAPSED_KEY) === '1',
  )
  const [width, setWidth] = useState(storedWidth)
  const [tab, setTab] = useState<Tab>(
    () => (localStorage.getItem(TAB_KEY) === 'links' ? 'links' : 'context'),
  )
  const [markdown, setMarkdown] = useState(
    () => localStorage.getItem(MARKDOWN_KEY) !== '0',
  )
  // El contador del tab Contexto lo reporta ContextPanel, que es el único que
  // tiene los datos. Se sigue mostrando aunque el tab activo sea Linkeado.
  const [contextCount, setContextCount] = useState(0)

  // Mismo patrón que la sidebar: el handler del drag se crea una vez y lee el
  // ancho de un ref para no capturarlo en su closure.
  const widthRef = useRef(width)
  widthRef.current = width

  const persistWidth = (px: number) => {
    setWidth(px)
    localStorage.setItem(WIDTH_KEY, String(px))
  }

  const toggleCollapsed = () => {
    setCollapsed((c) => {
      const next = !c
      localStorage.setItem(COLLAPSED_KEY, next ? '1' : '0')
      return next
    })
  }

  const selectTab = (t: Tab) => {
    setTab(t)
    localStorage.setItem(TAB_KEY, t)
  }

  const toggleMarkdown = () => {
    setMarkdown((m) => {
      const next = !m
      localStorage.setItem(MARKDOWN_KEY, next ? '1' : '0')
      return next
    })
  }

  // El resizer vive a la izquierda del panel: arrastrar hacia la izquierda lo
  // agranda, así que el signo va invertido respecto de la sidebar.
  const startResize = useCallback((e: ReactPointerEvent<HTMLDivElement>) => {
    if (e.button !== 0) return
    e.preventDefault()
    const handle = e.currentTarget
    const startX = e.clientX
    const startWidth = widthRef.current
    handle.setPointerCapture(e.pointerId)
    document.body.classList.add('resizing')

    const move = (ev: PointerEvent) =>
      persistWidth(clampWidth(startWidth - (ev.clientX - startX)))
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

  if (collapsed) {
    return (
      <div className="sidebar-resizer collapsed right">
        <button
          className="sidebar-reveal"
          onClick={toggleCollapsed}
          title="Mostrar el panel"
          aria-label="Mostrar el panel"
        >
          <PanelRightOpen size={15} />
        </button>
      </div>
    )
  }

  return (
    <>
      <div
        className="sidebar-resizer"
        onPointerDown={startResize}
        onDoubleClick={() => persistWidth(DEFAULT)}
        role="separator"
        aria-orientation="vertical"
        aria-label="Ancho del panel"
        aria-valuenow={width}
        aria-valuemin={MIN}
        title="Arrastrar para redimensionar · doble click para restaurar"
      />
      <aside
        className="rightpanel"
        style={{ '--rightpanel-w': `${width}px` } as CSSProperties}
      >
        <div className="rightpanel-tabs">
          <button
            className={tab === 'context' ? 'active' : ''}
            onClick={() => selectTab('context')}
          >
            <FileText size={13} /> Contexto
            {contextCount > 0 && <span className="count">{contextCount}</span>}
          </button>
          <button
            className={tab === 'links' ? 'active' : ''}
            onClick={() => selectTab('links')}
          >
            <Link2 size={13} /> Linkeado
          </button>
          <span className="rightpanel-tools">
            {tab === 'context' && (
              <button
                className={markdown ? 'toggle on' : 'toggle'}
                onClick={toggleMarkdown}
                title={markdown ? 'Ver como texto plano' : 'Renderizar markdown'}
                aria-pressed={markdown}
              >
                md
              </button>
            )}
            <button
              className="collapse"
              onClick={toggleCollapsed}
              title="Ocultar el panel"
              aria-label="Ocultar el panel"
            >
              <PanelRightClose size={15} />
            </button>
          </span>
        </div>

        {/* Los dos tabs quedan montados y se oculta el inactivo: así el contador
            de Contexto se mantiene al día aunque estés en Linkeado, y no se
            pierde lo que estabas editando al cambiar de tab. */}
        <div className="rightpanel-body">
          <div style={tab === 'context' ? undefined : { display: 'none' }}>
            <ContextPanel
              sessionId={sessionId}
              sessionName={sessionName}
              reloadKey={reloadKey}
              markdown={markdown}
              onCount={setContextCount}
            />
          </div>
          <div style={tab === 'links' ? undefined : { display: 'none' }}>
            <LinksTab
              sessionId={sessionId}
              reloadKey={reloadKey}
              active={tab === 'links' && !collapsed}
            />
          </div>
        </div>
      </aside>
    </>
  )
}
