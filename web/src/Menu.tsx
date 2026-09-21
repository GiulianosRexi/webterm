import { useEffect } from 'react'
import { createPortal } from 'react-dom'

export interface MenuItem {
  label: string
  onClick: () => void
  danger?: boolean
  // Dibuja una línea encima, para separar lo destructivo del resto.
  separated?: boolean
  disabled?: boolean
  // Marca el item como el estado actual (por ejemplo, el folder en el que la
  // sesión ya está).
  current?: boolean
}

export const MENU_WIDTH = 200
const ITEM_HEIGHT = 28

/**
 * Menu es el menú flotante que usan las filas de sesión y los encabezados de
 * folder.
 *
 * Vive en un portal sobre el body para que no lo recorte el overflow de la
 * sidebar, y se posiciona en coordenadas de ventana. x e y son la esquina
 * superior izquierda deseada; solo se corrigen si el menú no entra.
 */
export function Menu({
  x,
  y,
  items,
  onClose,
  header,
}: {
  x: number
  y: number
  items: MenuItem[]
  onClose: () => void
  // Texto chico arriba de todo, para decir sobre qué se está actuando cuando
  // el menú se aleja de la fila que lo abrió.
  header?: string
}) {
  // Se cierra con Escape, con un click afuera y con cualquier cosa que lo
  // desalinee de lo que lo abrió (scroll, resize): está en position: fixed y
  // no sigue a la fila.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('pointerdown', onClose)
    window.addEventListener('resize', onClose)
    window.addEventListener('scroll', onClose, true)
    window.addEventListener('keydown', onKey)
    return () => {
      window.removeEventListener('pointerdown', onClose)
      window.removeEventListener('resize', onClose)
      window.removeEventListener('scroll', onClose, true)
      window.removeEventListener('keydown', onKey)
    }
  }, [onClose])

  // Alto estimado para poder voltear el menú hacia arriba antes de pintarlo,
  // sin un frame en el que se vea desbordando la ventana.
  const height = items.length * ITEM_HEIGHT + (header ? 24 : 0) + 8
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
      {header && <div className="menu-header">{header}</div>}
      {items.map((item, i) => (
        <div key={item.label + i}>
          {item.separated && i > 0 && <hr />}
          <button
            role="menuitem"
            className={
              (item.danger ? 'danger' : '') + (item.current ? ' current' : '')
            }
            disabled={item.disabled}
            onClick={item.onClick}
          >
            {item.label}
          </button>
        </div>
      ))}
    </div>,
    document.body,
  )
}
