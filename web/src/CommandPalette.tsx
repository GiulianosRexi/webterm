import { useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import type { Session } from './api'
import { sessionLabel } from './session'
import { rank } from './fuzzy'

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
        run.match ? (
          <mark key={i}>{run.text}</mark>
        ) : (
          <span key={i}>{run.text}</span>
        ),
      )}
    </>
  )
}

export function CommandPalette({
  sessions,
  selectedId,
  onSelect,
  onClose,
}: {
  sessions: Session[]
  selectedId: string | null
  onSelect: (id: string) => void
  onClose: () => void
}) {
  const [query, setQuery] = useState('')
  const [active, setActive] = useState(0)
  const listRef = useRef<HTMLUListElement>(null)

  const results = useMemo(() => rank(query, sessions, sessionLabel), [query, sessions])

  // Escribir cambia los resultados, así que la selección vuelve arriba: dejarla
  // donde estaba apuntaría a una sesión distinta de la que el usuario venía
  // mirando.
  useEffect(() => setActive(0), [query])

  // Mantiene visible la fila activa cuando se navega con el teclado más allá
  // del alto de la lista.
  useEffect(() => {
    listRef.current?.querySelector('[data-active="true"]')?.scrollIntoView({ block: 'nearest' })
  }, [active])

  const choose = (id: string) => {
    onSelect(id)
    onClose()
  }

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') {
      e.preventDefault()
      onClose()
      return
    }
    if (e.key === 'ArrowDown' || (e.key === 'n' && e.ctrlKey)) {
      e.preventDefault()
      setActive((i) => (results.length === 0 ? 0 : (i + 1) % results.length))
      return
    }
    if (e.key === 'ArrowUp' || (e.key === 'p' && e.ctrlKey)) {
      e.preventDefault()
      setActive((i) => (results.length === 0 ? 0 : (i - 1 + results.length) % results.length))
      return
    }
    if (e.key === 'Enter') {
      e.preventDefault()
      const hit = results[active]
      if (hit) choose(hit.item.id)
    }
  }

  return createPortal(
    <div className="palette-backdrop" onMouseDown={onClose}>
      <div
        className="palette"
        role="dialog"
        aria-modal="true"
        aria-label="Buscar sesiones"
        onMouseDown={(e) => e.stopPropagation()}
      >
        <input
          className="palette-input"
          autoFocus
          value={query}
          placeholder="Buscar sesión…"
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={onKeyDown}
        />

        <ul className="palette-results" ref={listRef}>
          {results.length === 0 && <li className="palette-empty">sin resultados</li>}
          {results.map(({ item, match }, i) => (
            <li
              key={item.id}
              data-active={i === active}
              className={'palette-item' + (item.id === selectedId ? ' current' : '')}
              // mousedown y no click: el backdrop cierra en mousedown, así que
              // para cuando llegaría el click esta fila ya no existe.
              onMouseDown={() => choose(item.id)}
              onMouseEnter={() => setActive(i)}
            >
              <span className="dot" data-status={item.pty_status} />
              <span className="palette-name">
                <Highlight text={sessionLabel(item)} positions={match.positions} />
              </span>
              {item.id === selectedId && <span className="palette-hint">actual</span>}
            </li>
          ))}
        </ul>
      </div>
    </div>,
    document.body,
  )
}
