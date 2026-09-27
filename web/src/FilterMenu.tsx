import { useState } from 'react'
import { Check, Filter } from 'lucide-react'
import { Menu, MENU_WIDTH, type MenuItem } from './Menu'
import { KANBAN_STATUSES, STATUS_LABEL } from './session'
import { isFiltering, NO_FILTERS, setStatuses, type SessionFilters } from './filters'

type View = 'root' | 'status'

/**
 * FilterMenu es el botón de filtros de la sidebar y su menú.
 *
 * La raíz lista los tipos de filtro —hoy solo status— y cada uno abre su vista
 * en el mismo menú, como "Mover a…" en el de la sesión. Tildar no cierra el
 * menú: elegir varios estados seguidos es el caso normal, como en el filtro de
 * una columna de Excel.
 */
export function FilterMenu({
  filters,
  onChange,
  size,
}: {
  filters: SessionFilters
  onChange: (f: SessionFilters) => void
  size: number
}) {
  const [menu, setMenu] = useState<{ x: number; y: number; view: View } | null>(null)
  const active = isFiltering(filters)
  const close = () => setMenu(null)

  const selected = filters.statuses ?? [...KANBAN_STATUSES]

  const items = (view: View): MenuItem[] => {
    if (view === 'status') {
      return [
        { label: 'Volver', onClick: () => setMenu((m) => m && { ...m, view: 'root' }) },
        {
          label: 'Todos',
          separated: true,
          current: filters.statuses === null,
          onClick: () => onChange(setStatuses(filters, [...KANBAN_STATUSES])),
        },
        {
          label: 'Ninguno',
          current: selected.length === 0,
          onClick: () => onChange(setStatuses(filters, [])),
        },
        ...KANBAN_STATUSES.map((st, i) => {
          const on = selected.includes(st)
          return {
            label: STATUS_LABEL[st],
            separated: i === 0,
            icon: (
              <>
                <span className="filter-check" data-on={on}>
                  {on && <Check size={10} strokeWidth={3} />}
                </span>
                <span className="status-swatch" data-kanban={st} />
              </>
            ),
            onClick: () =>
              onChange(setStatuses(filters, on ? selected.filter((x) => x !== st) : [...selected, st])),
          }
        }),
      ]
    }

    const out: MenuItem[] = [
      {
        label:
          'Status' +
          (filters.statuses ? ` · ${filters.statuses.length} de ${KANBAN_STATUSES.length}` : ''),
        onClick: () => setMenu((m) => m && { ...m, view: 'status' }),
      },
    ]
    if (active) {
      out.push({
        label: 'Quitar filtros',
        separated: true,
        onClick: () => {
          close()
          onChange(NO_FILTERS)
        },
      })
    }
    return out
  }

  return (
    <>
      <button
        className={'filter-button' + (active ? ' active' : '')}
        onClick={(e) => {
          const r = e.currentTarget.getBoundingClientRect()
          setMenu({ x: r.right - MENU_WIDTH, y: r.bottom + 4, view: 'root' })
        }}
        title={active ? 'Filtros (hay filtros aplicados)' : 'Filtros'}
        aria-label="Filtros"
        aria-haspopup="menu"
      >
        <Filter size={size} />
      </button>
      {menu && (
        <Menu
          x={menu.x}
          y={menu.y}
          header={menu.view === 'status' ? 'Status' : 'Filtros'}
          items={items(menu.view)}
          onClose={close}
        />
      )}
    </>
  )
}
