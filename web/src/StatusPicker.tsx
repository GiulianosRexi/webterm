import { useState } from 'react'
import { ChevronDown } from 'lucide-react'
import { Menu, type MenuItem } from './Menu'
import { KANBAN_STATUSES, STATUS_LABEL, type KanbanStatus } from './session'

// StatusBadge es el estado de trabajo como pastilla de color. compact la
// achica para las filas de la sidebar y los tiles del Exposé.
export function StatusBadge({ status, compact }: { status: KanbanStatus; compact?: boolean }) {
  return (
    <span className={'status-badge' + (compact ? ' compact' : '')} data-kanban={status}>
      {STATUS_LABEL[status]}
    </span>
  )
}

// statusItems arma las opciones de estado para un Menu. Lo comparten el
// selector de la barra de arriba y el menú de la fila en la sidebar.
export function statusItems(current: KanbanStatus, onPick: (s: KanbanStatus) => void): MenuItem[] {
  return KANBAN_STATUSES.map((st) => ({
    label: STATUS_LABEL[st],
    current: st === current,
    icon: <span className="status-swatch" data-kanban={st} />,
    onClick: () => onPick(st),
  }))
}

/**
 * StatusPicker muestra el estado de la sesión abierta y deja cambiarlo. Es un
 * menú y no un <select> para que cada opción lleve su color: lo que se
 * reconoce de un vistazo en la sidebar es el color, no el texto.
 */
export function StatusPicker({
  status,
  onChange,
}: {
  status: KanbanStatus
  onChange: (s: KanbanStatus) => void
}) {
  const [pos, setPos] = useState<{ x: number; y: number } | null>(null)
  return (
    <>
      <button
        className="status-picker"
        title="Estado de trabajo de la sesión"
        aria-haspopup="menu"
        onClick={(e) => {
          const r = e.currentTarget.getBoundingClientRect()
          setPos({ x: r.left, y: r.bottom + 4 })
        }}
      >
        <StatusBadge status={status} />
        <ChevronDown size={12} />
      </button>
      {pos && (
        <Menu
          x={pos.x}
          y={pos.y}
          header="Estado"
          onClose={() => setPos(null)}
          items={statusItems(status, (st) => {
            setPos(null)
            if (st !== status) onChange(st)
          })}
        />
      )}
    </>
  )
}
