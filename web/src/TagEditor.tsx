import { useMemo, useState } from 'react'
import { Plus, Tag, X } from 'lucide-react'
import { normalizeTag, type TagCount } from './session'
import { rank } from './fuzzy'

type Option = { kind: 'existing'; name: string; count: number } | { kind: 'create'; name: string }

// Cuántas sugerencias se muestran. Más que esto deja de ser autocompletado y
// pasa a ser una lista para leer.
const MAX_SUGGESTIONS = 6

/**
 * TagEditor muestra los tags de la sesión abierta y deja agregar y sacar.
 *
 * El autocompletado es la mitad de la feature, no un adorno: sin él aparecen
 * "bugfix", "bug-fix" y "fix" como tres tags distintos y filtrar por tipo de
 * trabajo deja de servir. Por eso las sugerencias salen de los tags que ya
 * existen, y crear uno nuevo es una opción explícita al final de la lista.
 */
export function TagEditor({
  tags,
  known,
  onChange,
}: {
  tags: string[]
  known: TagCount[]
  onChange: (tags: string[]) => void
}) {
  // null: cerrado. string: el texto que se está escribiendo.
  const [draft, setDraft] = useState<string | null>(null)
  const [active, setActive] = useState(0)

  // Las opciones son los tags existentes que matchean y, al final, crear lo
  // que se escribió si todavía no existe. Crear va último a propósito: la
  // primera opción —la que elige Enter— es siempre reusar, y estrenar un tag
  // pide bajar hasta él. Es lo que hace que "bug" + Enter termine en "bugfix".
  const options = useMemo<Option[]>(() => {
    if (draft === null) return []
    const typed = normalizeTag(draft)
    const candidates = known.filter((t) => !tags.includes(t.name))
    const out: Option[] = rank(typed, candidates, (t) => t.name)
      .slice(0, MAX_SUGGESTIONS)
      .map((r) => ({ kind: 'existing', name: r.item.name, count: r.item.count }))
    if (typed && !tags.includes(typed) && !known.some((t) => t.name === typed)) {
      out.push({ kind: 'create', name: typed })
    }
    return out
  }, [draft, known, tags])

  const add = (raw: string) => {
    const tag = normalizeTag(raw)
    if (tag && !tags.includes(tag)) onChange([...tags, tag].sort())
    setDraft('')
    setActive(0)
  }

  const close = () => {
    setDraft(null)
    setActive(0)
  }

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'Escape') {
      e.preventDefault()
      close()
      return
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault()
      setActive((i) => (options.length ? (i + 1) % options.length : 0))
      return
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault()
      setActive((i) => (options.length ? (i - 1 + options.length) % options.length : 0))
      return
    }
    // Backspace con el campo vacío saca el último tag, como en cualquier
    // campo de etiquetas.
    if (e.key === 'Backspace' && draft === '' && tags.length > 0) {
      e.preventDefault()
      onChange(tags.slice(0, -1))
      return
    }
    if (e.key === 'Enter' || e.key === 'Tab') {
      if (!draft?.trim()) {
        if (e.key === 'Enter') close()
        return
      }
      e.preventDefault()
      const picked = options[active]
      if (picked) add(picked.name)
    }
  }

  return (
    <span className="tag-editor">
      {tags.map((t) => (
        <span key={t} className="tag-chip">
          {t}
          <button
            onClick={() => onChange(tags.filter((x) => x !== t))}
            title={`Sacar ${t}`}
            aria-label={`Sacar ${t}`}
          >
            <X size={11} />
          </button>
        </span>
      ))}

      {draft === null ? (
        <button
          className="tag-add"
          onClick={() => setDraft('')}
          title="Agregar un tag: qué clase de trabajo es esta sesión"
          aria-label="Agregar un tag"
        >
          {tags.length === 0 ? (
            <>
              <Tag size={12} /> tag
            </>
          ) : (
            <Plus size={12} />
          )}
        </button>
      ) : (
        <span className="tag-input-wrap">
          <input
            className="tag-input"
            autoFocus
            value={draft}
            placeholder="bugfix, consulta…"
            onChange={(e) => {
              setDraft(e.target.value)
              setActive(0)
            }}
            onKeyDown={onKeyDown}
            onBlur={close}
          />
          {options.length > 0 && (
            <ul className="tag-suggestions" role="listbox">
              {options.map((s, i) => (
                <li
                  key={s.kind + s.name}
                  className={s.kind === 'create' ? 'create' : undefined}
                  role="option"
                  aria-selected={i === active}
                  data-active={i === active}
                  // mousedown y no click: el blur del input cierra la lista
                  // antes de que llegue el click.
                  onMouseDown={(e) => {
                    e.preventDefault()
                    add(s.name)
                  }}
                  onMouseEnter={() => setActive(i)}
                >
                  {s.kind === 'create' ? (
                    <span>
                      crear <b>{s.name}</b>
                    </span>
                  ) : (
                    <>
                      <span>{s.name}</span>
                      <span className="tag-count">{s.count}</span>
                    </>
                  )}
                </li>
              ))}
            </ul>
          )}
        </span>
      )}
    </span>
  )
}
