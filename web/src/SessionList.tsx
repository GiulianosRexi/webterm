import { Fragment, useEffect, useMemo, useState } from 'react'
import type { Folder, Session } from './api'
import { relative, sessionLabel, statusOf, STATUS_LABEL } from './session'
import { StatusBadge, statusItems } from './StatusPicker'
import { Menu, MENU_WIDTH, type MenuItem } from './Menu'
import {
  ChevronDown,
  ChevronRight,
  LoaderCircle,
  MoreHorizontal,
  PanelLeftClose,
  Plus,
} from 'lucide-react'

// Tamaño de los iconos de la sidebar. Uno solo para todos: lo que los hace
// verse como un conjunto es que compartan caja y grosor de trazo.
const ICON = 15

const COLLAPSED_KEY = 'webterm.foldersCollapsed'

// SIN_FOLDER es la clave del grupo de sesiones sueltas. No se dibuja como un
// folder: en la base eso es folder_id NULL, no la pertenencia a un folder
// llamado "sin folder", y darle encabezado prometía cosas que no puede hacer
// —renombrarlo, borrarlo, colapsarlo. Van al final y sin indentar, como los
// archivos sueltos de un explorador.
const SIN_FOLDER = '__sin_folder__'

interface Grupo {
  key: string
  folder: Folder | null
  sessions: Session[]
}

/**
 * agrupar reparte las sesiones por folder.
 *
 * Sin folders devuelve un único grupo sin encabezado: la sidebar tiene que
 * verse igual que antes de que existieran, que es como va a estar la primera
 * vez que alguien la abra.
 *
 * Con folders van todos primero, en el orden que trae el backend (alfabético),
 * y las sueltas al final sin encabezado. El orden importa: las sesiones nacen
 * sueltas, así que ponerlas arriba empujaría los folders hacia abajo cada vez
 * que se crea una.
 *
 * Un folder vacío se muestra igual: es la única señal de que existe, y
 * esconderlo haría que crear uno no tuviera efecto visible.
 */
function agrupar(sessions: Session[], folders: Folder[]): Grupo[] {
  if (folders.length === 0) {
    return [{ key: SIN_FOLDER, folder: null, sessions }]
  }

  const porFolder = new Map<string, Session[]>()
  const sueltas: Session[] = []
  const existe = new Set(folders.map((f) => f.id))

  for (const s of sessions) {
    // Un folder_id que ya no existe cuenta como suelta: sin foreign key esa
    // fila puede quedar colgada, y esconder la sesión sería peor que mostrarla
    // fuera de lugar.
    if (s.folder_id && existe.has(s.folder_id)) {
      const lista = porFolder.get(s.folder_id) ?? []
      lista.push(s)
      porFolder.set(s.folder_id, lista)
    } else {
      sueltas.push(s)
    }
  }

  const grupos: Grupo[] = folders.map((f) => ({
    key: f.id,
    folder: f,
    sessions: porFolder.get(f.id) ?? [],
  }))
  if (sueltas.length > 0) {
    grupos.push({ key: SIN_FOLDER, folder: null, sessions: sueltas })
  }
  return grupos
}

// view elige qué muestra el menú de una sesión: la raíz, la lista de folders
// o la de estados. Son vistas del mismo menú y no submenús, por lo mismo que
// explica itemsDeSesion.
type MenuSesion = {
  kind: 'session'
  id: string
  x: number
  y: number
  view?: 'moving' | 'status'
}
type MenuState = MenuSesion | { kind: 'folder'; id: string; x: number; y: number }

export function SessionList({
  sessions,
  folders,
  selectedId,
  busy,
  onSelect,
  onCreate,
  onRename,
  onKill,
  onRestart,
  onDelete,
  onSetStatus,
  onCollapse,
  onMove,
  onCreateFolderAndMove,
  onRenameFolder,
  onDeleteFolder,
}: {
  sessions: Session[]
  folders: Folder[]
  selectedId: string | null
  busy: boolean
  onSelect: (id: string) => void
  onCreate: () => void
  onRename: (id: string, title: string) => void
  onKill: (id: string) => void
  onRestart: (id: string) => void
  onDelete: (id: string) => void
  onSetStatus: (id: string, status: string) => void
  onCollapse: () => void
  onMove: (sessionId: string, folderId: string | null) => void
  onCreateFolderAndMove: (sessionId: string, name: string) => void
  onRenameFolder: (id: string, name: string) => void
  onDeleteFolder: (id: string) => void
}) {
  const [editing, setEditing] = useState<string | null>(null)
  const [draft, setDraft] = useState('')
  // El menú guarda el id, no el objeto: así el refresco puede cambiarle el
  // estado por debajo y los items se recalculan solos.
  const [menu, setMenu] = useState<MenuState | null>(null)
  // Nombre del folder nuevo mientras se escribe, o null si no se está creando.
  const [nuevoFolder, setNuevoFolder] = useState<string | null>(null)
  // id de la sesión que se está arrastrando, y folder sobre el que está
  // parada. null en dropTarget significa "fuera de todo folder".
  const [dragging, setDragging] = useState<string | null>(null)
  const [dropTarget, setDropTarget] = useState<string | null | undefined>(undefined)
  const [collapsed, setCollapsed] = useState<Record<string, boolean>>(() => {
    try {
      return JSON.parse(localStorage.getItem(COLLAPSED_KEY) ?? '{}') as Record<string, boolean>
    } catch {
      return {}
    }
  })

  useEffect(() => {
    localStorage.setItem(COLLAPSED_KEY, JSON.stringify(collapsed))
  }, [collapsed])

  const grupos = useMemo(() => agrupar(sessions, folders), [sessions, folders])

  const commit = (id: string) => {
    setEditing(null)
    const title = draft.trim()
    if (title) onRename(id, title)
  }

  const commitFolder = (id: string) => {
    setEditing(null)
    const name = draft.trim()
    if (name) onRenameFolder(id, name)
  }

  const cerrarMenu = () => {
    setMenu(null)
    setNuevoFolder(null)
  }

  const startRename = (s: Session) => {
    setEditing(s.id)
    setDraft(s.title || sessionLabel(s))
  }

  const toggle = (key: string) => setCollapsed((c) => ({ ...c, [key]: !c[key] }))

  const terminarDrag = () => {
    setDragging(null)
    setDropTarget(undefined)
  }

  // Un destino de drop. folderId null es "sacarla de su folder".
  const destino = (folderId: string | null) => ({
    onDragOver: (e: React.DragEvent) => {
      if (!dragging) return
      // preventDefault es lo que marca al elemento como destino válido: sin
      // esto el navegador rechaza el drop y muestra el cursor de prohibido.
      e.preventDefault()
      e.dataTransfer.dropEffect = 'move'
      setDropTarget(folderId)
    },
    onDragLeave: () => setDropTarget((actual) => (actual === folderId ? undefined : actual)),
    onDrop: (e: React.DragEvent) => {
      e.preventDefault()
      const id = e.dataTransfer.getData('text/plain') || dragging
      terminarDrag()
      if (!id) return
      const sesion = sessions.find((x) => x.id === id)
      // Soltarla donde ya está no es un error, pero tampoco vale un request.
      if (sesion && (sesion.folder_id ?? null) !== folderId) onMove(id, folderId)
    },
  })

  const arrastrando = dragging ? (sessions.find((s) => s.id === dragging) ?? null) : null

  // Al renombrar, el grupo queda expandido. Hace falta porque el primer click
  // del doble click llega al encabezado y lo colapsa: sin esto, renombrar un
  // folder abierto lo cerraría de paso.
  const abrirRenombre = (key: string, nombre: string) => {
    setCollapsed((c) => ({ ...c, [key]: false }))
    setEditing(key)
    setDraft(nombre)
  }

  // Los items del menú de una sesión. Tiene dos vistas: la raíz y la de mover,
  // que reemplaza el contenido en vez de abrir un submenú flotante —un submenú
  // al lado del borde de la sidebar termina saliéndose de la pantalla.
  const itemsDeSesion = (s: Session, estado: MenuSesion): MenuItem[] => {
    if (estado.view === 'status') {
      return [
        { label: 'Volver', onClick: () => setMenu({ ...estado, view: undefined }) },
        ...statusItems(statusOf(s), (st) => {
          cerrarMenu()
          if (st !== statusOf(s)) onSetStatus(s.id, st)
        }).map((item, i) => ({ ...item, separated: i === 0 })),
      ]
    }
    if (estado.view === 'moving') {
      const items: MenuItem[] = [
        { label: 'Volver', onClick: () => setMenu({ ...estado, view: undefined }) },
        {
          label: 'Ninguno',
          current: !s.folder_id,
          separated: true,
          onClick: () => {
            cerrarMenu()
            onMove(s.id, null)
          },
        },
      ]
      for (const f of folders) {
        items.push({
          label: f.name,
          current: s.folder_id === f.id,
          onClick: () => {
            cerrarMenu()
            onMove(s.id, f.id)
          },
        })
      }
      items.push({
        label: 'Nuevo folder…',
        separated: true,
        onClick: () => setNuevoFolder(''),
      })
      return items
    }

    const items: MenuItem[] = [
      {
        label: 'Renombrar',
        onClick: () => {
          cerrarMenu()
          startRename(s)
        },
      },
      { label: 'Mover a…', onClick: () => setMenu({ ...estado, view: 'moving' }) },
      {
        label: `Estado: ${STATUS_LABEL[statusOf(s)]}…`,
        onClick: () => setMenu({ ...estado, view: 'status' }),
      },
    ]
    // starting es la ventana en la que el orquestador ya pidió el spawn y
    // todavía no supo si el daemon lo confirmó. Ni Parar ni Reanudar tienen
    // sentido ahí: Restart verifica contra el daemon que la sesión no esté
    // viva y spawnea de nuevo, y dispararlo mientras el spawn original sigue
    // en vuelo podría chocar con él.
    if (s.pty_status === 'running') {
      items.push({
        label: 'Parar',
        onClick: () => {
          cerrarMenu()
          onKill(s.id)
        },
      })
    }
    if (s.pty_status === 'exited') {
      items.push({
        label: 'Reanudar',
        onClick: () => {
          cerrarMenu()
          onRestart(s.id)
        },
      })
    }
    // Borrar sí queda disponible en starting: no le exige nada al daemon sobre
    // el pty y es la única forma de cancelar una sesión que quedó pegada
    // arrancando sin esperar los 30s del sweep.
    items.push({
      label: 'Borrar',
      danger: true,
      separated: true,
      onClick: () => {
        cerrarMenu()
        onDelete(s.id)
      },
    })
    return items
  }

  const renderSesion = (s: Session, folderID: string | null) => {
    const enFolder = folderID !== null
    return (
    <li
      key={s.id}
      className={
        'session' +
        (s.id === selectedId ? ' selected' : '') +
        (enFolder ? ' en-folder' : '') +
        (dragging === s.id ? ' dragging' : '')
      }
      // Mientras se renombra no: arrastrar se comería la selección de texto
      // dentro del input.
      draggable={editing !== s.id}
      onDragStart={(e) => {
        e.dataTransfer.setData('text/plain', s.id)
        e.dataTransfer.effectAllowed = 'move'
        setDragging(s.id)
      }}
      onDragEnd={terminarDrag}
      // Soltar sobre una fila vale como soltar en su folder: así el blanco es
      // el grupo entero y no una franja de 32px.
      {...destino(folderID)}
      onClick={() => onSelect(s.id)}
      onContextMenu={(e) => {
        e.preventDefault()
        setMenu({ kind: 'session', id: s.id, x: e.clientX, y: e.clientY })
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
          <span className="name-inner">{sessionLabel(s)}</span>
        </span>
      )}
      {/* Los tags van apagados y después del nombre: dicen qué clase de
          trabajo es sin competir con el título, y si no entran se cortan
          ellos antes que el nombre. */}
      {s.tags.length > 0 && editing !== s.id && (
        <span className="tags" title={s.tags.join(' · ')}>
          {s.tags.map((t) => (
            <span key={t} className="tag">
              {t}
            </span>
          ))}
        </span>
      )}
      {/* "Not started" es el estado con el que nace toda sesión: mostrarlo
          en cada fila sería ruido, así que solo se ve lo que se movió. */}
      {statusOf(s) !== 'todo' && editing !== s.id && (
        <StatusBadge status={statusOf(s)} compact />
      )}
      <span className="when">{relative(s.last_active_at)}</span>
      {s.pty_status === 'starting' && (
        <span className="starting-hint" title="La sesión está arrancando">
          <LoaderCircle size={12} />
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
            setMenu({ kind: 'session', id: s.id, x: r.right - MENU_WIDTH, y: r.bottom + 4 })
          }}
        >
          <MoreHorizontal size={14} />
        </button>
      </span>
    </li>
    )
  }

  const sesionDelMenu =
    menu?.kind === 'session' ? (sessions.find((s) => s.id === menu.id) ?? null) : null
  const folderDelMenu =
    menu?.kind === 'folder' ? (folders.find((f) => f.id === menu.id) ?? null) : null

  return (
    <aside className="sidebar">
      <div className="sidebar-head">
        <span>Sesiones</span>
        <button onClick={onCreate} disabled={busy} title="Nueva sesión" aria-label="Nueva sesión">
          <Plus size={ICON} />
        </button>
        <button
          className="collapse"
          onClick={onCollapse}
          title="Ocultar la lista de sesiones"
          aria-label="Ocultar la lista de sesiones"
        >
          <PanelLeftClose size={ICON} />
        </button>
      </div>

      <ul className={'session-list' + (dragging ? ' arrastrando' : '')}>
        {sessions.length === 0 && folders.length === 0 && (
          <li className="empty">todavía no hay ninguna</li>
        )}

        {grupos.map((g) => (
          <Fragment key={g.key}>
            {/* Solo los folders de verdad llevan encabezado. Las sueltas van
                al final, sin título y sin indentar. */}
            {g.folder && (
              <li
                className={'folder-head' + (dropTarget === g.folder.id ? ' drop-target' : '')}
                {...destino(g.folder.id)}
                onClick={() => toggle(g.key)}
                onContextMenu={(e) => {
                  e.preventDefault()
                  setMenu({ kind: 'folder', id: g.folder!.id, x: e.clientX, y: e.clientY })
                }}
              >
                <span className="caret">
                  {collapsed[g.key] ? <ChevronRight size={14} /> : <ChevronDown size={14} />}
                </span>
                {editing === g.key ? (
                  <input
                    className="rename"
                    autoFocus
                    value={draft}
                    onChange={(e) => setDraft(e.target.value)}
                    onBlur={() => commitFolder(g.key)}
                    onKeyDown={(e) => {
                      if (e.key === 'Enter') commitFolder(g.key)
                      if (e.key === 'Escape') setEditing(null)
                    }}
                    onClick={(e) => e.stopPropagation()}
                  />
                ) : (
                  <span
                    className="folder-name"
                    onDoubleClick={(e) => {
                      e.stopPropagation()
                      abrirRenombre(g.key, g.folder!.name)
                    }}
                    title="doble click para renombrar"
                  >
                    {g.folder!.name}
                  </span>
                )}
                <span className="folder-count">{g.sessions.length}</span>
              </li>
            )}

            {!collapsed[g.key] &&
              g.sessions.map((s) => renderSesion(s, g.folder ? g.folder.id : null))}
          </Fragment>
        ))}

        {/* Sacar una sesión de su folder necesita un destino, y las sueltas no
            tienen encabezado donde soltarla. Esta franja aparece solo durante
            el arrastre, y solo si la sesión está en un folder: si ya está
            suelta no hay nada que sacar. */}
        {arrastrando?.folder_id && (
          <li
            className={'drop-fuera' + (dropTarget === null ? ' drop-target' : '')}
            {...destino(null)}
          >
            Sacar del folder
          </li>
        )}
      </ul>

      {menu && sesionDelMenu && (
        <Menu
          x={menu.x}
          y={menu.y}
          items={itemsDeSesion(sesionDelMenu, menu as MenuSesion)}
          onClose={cerrarMenu}
          header={
            menu.kind === 'session' && menu.view
              ? menu.view === 'moving'
                ? 'Mover a'
                : 'Estado'
              : undefined
          }
        />
      )}

      {menu && folderDelMenu && (
        <Menu
          x={menu.x}
          y={menu.y}
          header={folderDelMenu.name}
          onClose={cerrarMenu}
          items={[
            {
              label: 'Renombrar',
              onClick: () => {
                cerrarMenu()
                abrirRenombre(folderDelMenu.id, folderDelMenu.name)
              },
            },
            {
              label: 'Borrar folder',
              danger: true,
              separated: true,
              onClick: () => {
                cerrarMenu()
                onDeleteFolder(folderDelMenu.id)
              },
            },
          ]}
        />
      )}

      {/* El campo del folder nuevo se dibuja encima del menú, que sigue
          abierto: al confirmar se crea y la sesión se mueve ahí mismo. */}
      {nuevoFolder !== null && menu?.kind === 'session' && (
        <div
          className="folder-nuevo"
          style={{ left: menu.x, top: menu.y, width: MENU_WIDTH }}
          onPointerDown={(e) => e.stopPropagation()}
        >
          <input
            autoFocus
            value={nuevoFolder}
            placeholder="Nombre del folder"
            onChange={(e) => setNuevoFolder(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Escape') setNuevoFolder(null)
              if (e.key === 'Enter') {
                const name = nuevoFolder.trim()
                const id = menu.id
                cerrarMenu()
                if (name) onCreateFolderAndMove(id, name)
              }
            }}
          />
        </div>
      )}
    </aside>
  )
}
