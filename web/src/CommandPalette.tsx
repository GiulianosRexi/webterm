import { useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { Folder as FolderIcon, Tag as TagIcon, X } from 'lucide-react'
import type { Folder, Session } from './api'
import { allTags, sessionLabel, type TagCount } from './session'
import { fuzzyMatch, rank, type FuzzyMatch } from './fuzzy'

// Highlight parte el texto en tramos que matchearon y tramos que no, en vez de
// envolver cada letra por separado: un título de 45 caracteres generaría 45
// nodos para marcar tres.
function Highlight({ text, positions }: { text: string; positions: number[] }) {
  if (positions.length === 0) return <>{text}</>

  const hit = new Set(positions)
  const runs: { text: string; match: boolean }[] = []
  for (let i = 0; i < text.length; i++) {
    const match = hit.has(i)
    const last = runs[runs.length - 1]
    if (last && last.match === match) last.text += text[i]
    else runs.push({ text: text[i], match })
  }

  return (
    <>
      {runs.map((run, i) =>
        run.match ? <mark key={i}>{run.text}</mark> : <span key={i}>{run.text}</span>,
      )}
    </>
  )
}

// Un resultado navegable. Los encabezados de sección no entran acá: se dibujan
// al vuelo y no se pueden seleccionar con las flechas.
type Item =
  | {
      kind: 'session'
      session: Session
      // El folder al que pertenece, para mostrarlo al lado del nombre.
      folder: Folder | null
      // Highlights por campo. El orden lo decide el match contra título y
      // folder juntos, pero resaltar necesita saber qué letras cayeron en cada
      // uno: las posiciones del texto concatenado no sirven para pintar.
      tituloPos: number[]
      folderPos: number[]
      tagPos: number[][]
    }
  | { kind: 'folder'; folder: Folder; match: FuzzyMatch }
  | { kind: 'tag'; tag: TagCount; match: FuzzyMatch }

// Por qué se está acotando la búsqueda: un folder o un tag. Uno a la vez: con
// dos chips el Backspace tendría que decidir cuál sacar, y el caso de
// combinarlos todavía no apareció.
type Filter = { kind: 'folder'; folder: Folder } | { kind: 'tag'; tag: string }

export function CommandPalette({
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
  const [active, setActive] = useState(0)
  const [filtro, setFiltro] = useState<Filter | null>(null)
  const listRef = useRef<HTMLUListElement>(null)

  const items = useMemo<Item[]>(() => {
    const visibles = !filtro
      ? sessions
      : filtro.kind === 'folder'
        ? sessions.filter((s) => s.folder_id === filtro.folder.id)
        : sessions.filter((s) => s.tags.includes(filtro.tag))
    const folderDe = (s: Session) => folders.find((f) => f.id === s.folder_id) ?? null

    // Se busca contra el título, el nombre del folder y los tags juntos: así
    // "webterm mejoras" encuentra algo que en ningún campo está escrito
    // completo, y buscar un proyecto o un tipo de trabajo trae sus sesiones
    // aunque el título no lo mencione.
    const out: Item[] = rank(
      query,
      visibles,
      (s) => `${sessionLabel(s)} ${folderDe(s)?.name ?? ''} ${s.tags.join(' ')}`,
    ).map((r) => {
      const folder = folderDe(r.item)
      return {
        kind: 'session' as const,
        session: r.item,
        folder,
        tituloPos: fuzzyMatch(query, sessionLabel(r.item))?.positions ?? [],
        folderPos: folder ? (fuzzyMatch(query, folder.name)?.positions ?? []) : [],
        tagPos: r.item.tags.map((t) => fuzzyMatch(query, t)?.positions ?? []),
      }
    })

    // Con un filtro ya elegido no se ofrecen más: la búsqueda quedó adentro
    // de ese, y mostrar otros invitaría a saltar en vez de filtrar.
    if (!filtro) {
      for (const r of rank(query, folders, (f) => f.name)) {
        out.push({ kind: 'folder', folder: r.item, match: r.match })
      }
      // Los tags van alfabéticos y no por uso, igual que el resto del
      // palette: el orden no se mueve solo entre una apertura y la siguiente.
      const tags = allTags(sessions).sort((a, b) => a.name.localeCompare(b.name))
      for (const r of rank(query, tags, (t) => t.name)) {
        out.push({ kind: 'tag', tag: r.item, match: r.match })
      }
    }
    return out
  }, [query, sessions, folders, filtro])

  // Escribir cambia los resultados, así que la selección vuelve arriba: dejarla
  // donde estaba apuntaría a otra cosa de la que el usuario venía mirando.
  useEffect(() => setActive(0), [query, filtro])

  useEffect(() => {
    listRef.current?.querySelector('[data-active="true"]')?.scrollIntoView({ block: 'nearest' })
  }, [active])

  const elegir = (item: Item) => {
    // Un folder o un tag no es un destino: acota la búsqueda y deja seguir
    // escribiendo. La query que sirvió para encontrarlo no sirve para buscar
    // adentro, así que se limpia.
    if (item.kind === 'folder') {
      setFiltro({ kind: 'folder', folder: item.folder })
      setQuery('')
      return
    }
    if (item.kind === 'tag') {
      setFiltro({ kind: 'tag', tag: item.tag.name })
      setQuery('')
      return
    }
    onSelect(item.session.id)
    onClose()
  }

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') {
      e.preventDefault()
      onClose()
      return
    }
    // Backspace con el campo vacío saca el folder, como se borra un chip en
    // cualquier campo de etiquetas.
    if (e.key === 'Backspace' && query === '' && filtro) {
      e.preventDefault()
      setFiltro(null)
      return
    }
    if (e.key === 'ArrowDown' || (e.key === 'n' && e.ctrlKey)) {
      e.preventDefault()
      setActive((i) => (items.length === 0 ? 0 : (i + 1) % items.length))
      return
    }
    if (e.key === 'ArrowUp' || (e.key === 'p' && e.ctrlKey)) {
      e.preventDefault()
      setActive((i) => (items.length === 0 ? 0 : (i - 1 + items.length) % items.length))
      return
    }
    if (e.key === 'Enter') {
      e.preventDefault()
      const hit = items[active]
      if (hit) elegir(hit)
    }
  }

  // Índice donde arranca la sección de folders, para poder dibujar el
  // encabezado sin romper la numeración que usa el teclado.
  //
  // Solo los folders llevan título: son la sección secundaria. Etiquetar
  // también las sesiones sería ruido, porque son lo que uno viene a buscar.
  const primerFolder = items.findIndex((i) => i.kind === 'folder')
  const primerTag = items.findIndex((i) => i.kind === 'tag')
  const itemKey = (item: Item) =>
    item.kind === 'folder'
      ? 'folder' + item.folder.id
      : item.kind === 'tag'
        ? 'tag' + item.tag.name
        : 'session' + item.session.id

  return createPortal(
    <div className="palette-backdrop" onMouseDown={onClose}>
      <div
        className="palette"
        role="dialog"
        aria-modal="true"
        aria-label="Buscar sesiones"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <div className="palette-campo">
          {filtro && (
            <span className="palette-chip">
              {filtro.kind === 'folder' ? <FolderIcon size={12} /> : <TagIcon size={12} />}
              {filtro.kind === 'folder' ? filtro.folder.name : filtro.tag}
              <button
                onClick={() => setFiltro(null)}
                title="Quitar el filtro"
                aria-label="Quitar el filtro"
              >
                <X size={12} />
              </button>
            </span>
          )}
          <input
            className="palette-input"
            autoFocus
            value={query}
            placeholder={
              !filtro
                ? 'Buscar sesión, folder o tag…'
                : filtro.kind === 'folder'
                  ? 'Buscar en este folder…'
                  : 'Buscar con este tag…'
            }
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={onKeyDown}
          />
        </div>

        <ul className="palette-results" ref={listRef}>
          {items.length === 0 && <li className="palette-empty">sin resultados</li>}

          {items.map((item, i) => (
            <li key={itemKey(item)}>
              {i === primerFolder && <div className="palette-seccion">Folders</div>}
              {i === primerTag && <div className="palette-seccion">Tags</div>}
              <div
                data-active={i === active}
                className={
                  'palette-item' +
                  (item.kind === 'session' && item.session.id === selectedId ? ' current' : '')
                }
                // mousedown y no click: el backdrop cierra en mousedown, así
                // que para cuando llegaría el click esta fila ya no existe.
                onMouseDown={() => elegir(item)}
                onMouseEnter={() => setActive(i)}
              >
                {item.kind === 'session' ? (
                  <>
                    <span className="dot" data-status={item.session.pty_status} />
                    <span className="palette-name">
                      <Highlight text={sessionLabel(item.session)} positions={item.tituloPos} />
                    </span>
                    {/* Con un folder ya elegido el dato sobra: todas las filas
                        son de ese folder y repetirlo en cada una es ruido. */}
                    {item.session.tags.map((t, ti) => (
                      <span key={t} className="palette-tag">
                        <Highlight text={t} positions={item.tagPos[ti]} />
                      </span>
                    ))}
                    {item.folder && filtro?.kind !== 'folder' && (
                      <span className="palette-folder">
                        <FolderIcon size={11} />
                        <Highlight text={item.folder.name} positions={item.folderPos} />
                      </span>
                    )}
                    {item.session.id === selectedId && (
                      <span className="palette-hint">actual</span>
                    )}
                  </>
                ) : item.kind === 'tag' ? (
                  <>
                    <TagIcon size={13} className="palette-icono" />
                    <span className="palette-name">
                      <Highlight text={item.tag.name} positions={item.match.positions} />
                    </span>
                    <span className="palette-hint">{item.tag.count} · filtrar</span>
                  </>
                ) : (
                  <>
                    <FolderIcon size={13} className="palette-icono" />
                    <span className="palette-name">
                      <Highlight text={item.folder.name} positions={item.match.positions} />
                    </span>
                    <span className="palette-hint">filtrar</span>
                  </>
                )}
              </div>
            </li>
          ))}
        </ul>
      </div>
    </div>,
    document.body,
  )
}
