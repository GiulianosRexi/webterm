import { useCallback, useEffect, useState } from 'react'
import { api, type LinkedResource, type PRState } from './api'

// Cada cuánto se refresca mientras el panel está abierto. El backend cachea
// 30 s, así que este polling no se traduce uno a uno en llamadas a GitHub.
const POLL_MS = 15000

export function ResourcePanel({ sessionId }: { sessionId: string }) {
  const [open, setOpen] = useState(false)
  const [items, setItems] = useState<LinkedResource[]>([])
  const [draft, setDraft] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  const refresh = useCallback(async () => {
    try {
      setItems(await api.resources.list(sessionId))
    } catch (err) {
      setError(String(err))
    }
  }, [sessionId])

  // La lista se pide una vez al montar para poder mostrar el contador; el
  // refresco periódico corre solo con el panel abierto, que es lo que evita
  // consultar GitHub por sesiones que nadie está mirando.
  useEffect(() => {
    void refresh()
  }, [refresh])

  useEffect(() => {
    if (!open) return
    const t = setInterval(() => void refresh(), POLL_MS)
    return () => clearInterval(t)
  }, [open, refresh])

  const link = async () => {
    const ref = draft.trim()
    if (!ref) return
    setBusy(true)
    try {
      await api.resources.link(sessionId, ref)
      setDraft('')
      setError(null)
      await refresh()
    } catch (err) {
      setError(String(err))
    } finally {
      setBusy(false)
    }
  }

  const unlink = async (id: number) => {
    setBusy(true)
    try {
      await api.resources.unlink(sessionId, id)
      setError(null)
      await refresh()
    } catch (err) {
      setError(String(err))
    } finally {
      setBusy(false)
    }
  }

  return (
    <section className="resources">
      <button className="resources-head" onClick={() => setOpen((o) => !o)}>
        <span className="caret">{open ? '▾' : '▸'}</span>
        Linkeado
        {items.length > 0 && <span className="count">{items.length}</span>}
      </button>

      {open && (
        <div className="resources-body">
          <div className="link-form">
            <input
              value={draft}
              placeholder="URL de un PR de GitHub"
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') void link()
              }}
            />
            <button onClick={() => void link()} disabled={busy || !draft.trim()}>
              Linkear
            </button>
          </div>

          {error && <p className="resource-error">{error}</p>}
          {items.length === 0 && <p className="empty">nada linkeado todavía</p>}

          {items.map((it) => (
            <ResourceCard key={it.id} item={it} onUnlink={() => void unlink(it.id)} busy={busy} />
          ))}
        </div>
      )}
    </section>
  )
}

function ResourceCard({
  item,
  onUnlink,
  busy,
}: {
  item: LinkedResource
  onUnlink: () => void
  busy: boolean
}) {
  const pr = item.snapshot?.pr
  return (
    <article className="resource-card">
      <header>
        <a href={item.ref} target="_blank" rel="noreferrer">
          {pr ? `#${pr.number} ${pr.title}` : item.ref}
        </a>
        <button className="danger" onClick={onUnlink} disabled={busy} title="Deslinkear">
          ✕
        </button>
      </header>

      {item.snapshot?.error && <p className="resource-error">{item.snapshot.error}</p>}
      {pr && <PRBadges pr={pr} />}
      {item.snapshot && <Freshness at={item.snapshot.fetched_at} />}
    </article>
  )
}

function PRBadges({ pr }: { pr: PRState }) {
  return (
    <p className="badges">
      <span className="badge" data-value={prState(pr)}>
        {prStateLabel(pr)}
      </span>
      <span className="badge" data-value={pr.review_decision || 'NONE'}>
        {reviewLabel(pr.review_decision)}
      </span>
      {pr.checks_state && (
        <span className="badge" data-value={checksValue(pr)}>
          {checksLabel(pr)}
        </span>
      )}
      <span className="badge" data-value={pr.unresolved_count > 0 ? 'threads' : 'clean'}>
        {pr.unresolved_count === 0
          ? 'sin comments pendientes'
          : `${pr.unresolved_count}${pr.threads_truncated ? '+' : ''} sin resolver`}
      </span>
    </p>
  )
}

function prState(pr: PRState): string {
  return pr.is_draft && pr.state === 'OPEN' ? 'DRAFT' : pr.state
}

function prStateLabel(pr: PRState): string {
  switch (prState(pr)) {
    case 'DRAFT':
      return 'borrador'
    case 'OPEN':
      return pr.mergeable === 'CONFLICTING' ? 'abierto · conflictos' : 'abierto'
    case 'MERGED':
      return 'mergeado'
    default:
      return 'cerrado'
  }
}

function reviewLabel(decision: string): string {
  switch (decision) {
    case 'APPROVED':
      return 'aprobado'
    case 'CHANGES_REQUESTED':
      return 'cambios pedidos'
    case 'REVIEW_REQUIRED':
      return 'falta review'
    default:
      return 'sin review'
  }
}

// checksValue prioriza lo que contamos nosotros por sobre el rollup: si hay
// checks fallando se pinta en rojo aunque el rollup diga otra cosa.
function checksValue(pr: PRState): string {
  if (pr.checks_failing > 0) return 'FAILURE'
  if (pr.checks_state === 'PENDING') return 'PENDING'
  return pr.checks_state
}

function checksLabel(pr: PRState): string {
  if (pr.checks_failing > 0) return `${pr.checks_failing} de ${pr.checks_total} fallando`
  if (pr.checks_state === 'PENDING') return 'checks corriendo'
  return `${pr.checks_total} checks ok`
}

// Freshness marca la edad del dato. Un check en verde de hace veinte minutos
// mostrado como actual es peor que no mostrar nada.
function Freshness({ at }: { at: number }) {
  const secs = Math.max(0, Math.round((Date.now() - at) / 1000))
  if (secs < 60) return null
  const txt = secs < 3600 ? `hace ${Math.floor(secs / 60)} min` : `hace ${Math.floor(secs / 3600)} h`
  return <p className="stale">{txt}</p>
}
