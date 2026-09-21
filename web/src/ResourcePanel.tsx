import { useCallback, useEffect, useState } from 'react'
import { ChevronDown, ChevronRight, X } from 'lucide-react'
import { api, type LinkedResource, type PRState } from './api'
import {
  IconCheck,
  IconClock,
  IconClosed,
  IconComment,
  IconDraft,
  IconMerged,
  IconOpen,
  IconX,
} from './prIcons'

// El refresco en vivo llega por /api/events, pero el bus nunca puede avisar
// que cambió el estado de un PR en GitHub: eso solo se entera preguntando. 30s
// y no más porque el backend cachea las respuestas de GitHub 30s, así que
// bajar el poll no cuesta una sola llamada extra a GitHub.
const POLL_MS = 30000

export function ResourcePanel({
  sessionId,
  reloadKey,
}: {
  sessionId: string
  reloadKey: number
}) {
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

  // Corre al montar y cada vez que App avisa que hubo un evento de recursos
  // para esta sesión.
  useEffect(() => {
    void refresh()
  }, [refresh, reloadKey])

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
        <span className="caret">
          {open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}
        </span>
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
        <div className="titles">
          {pr?.repo && <span className="repo">{pr.repo}</span>}
          <a href={item.ref} target="_blank" rel="noreferrer">
            {pr ? `#${pr.number} ${pr.title}` : item.ref}
          </a>
        </div>
        <button
          className="danger"
          onClick={onUnlink}
          disabled={busy}
          title="Deslinkear"
          aria-label="Deslinkear"
        >
          <X size={14} />
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
      <StateBadge pr={pr} />
      <ReviewBadge decision={pr.review_decision} />
      {pr.checks_state && <ChecksBadge pr={pr} />}
      <CommentsBadge pr={pr} />
    </p>
  )
}

// StateBadge usa la paleta de GitHub para que el color signifique lo mismo que
// allá: violeta mergeado, verde abierto, rojo cerrado, gris borrador.
function StateBadge({ pr }: { pr: PRState }) {
  if (pr.state === 'MERGED') {
    return (
      <span className="badge icon-only" data-state="merged">
        <IconMerged title="Mergeado" />
      </span>
    )
  }
  if (pr.state === 'CLOSED') {
    return (
      <span className="badge icon-only" data-state="closed">
        <IconClosed title="Cerrado sin mergear" />
      </span>
    )
  }
  if (pr.is_draft) {
    return (
      <span className="badge icon-only" data-state="draft">
        <IconDraft title="Borrador" />
      </span>
    )
  }
  const conflictos = pr.mergeable === 'CONFLICTING'
  return (
    <span className="badge icon-only" data-state={conflictos ? 'closed' : 'open'}>
      <IconOpen title={conflictos ? 'Abierto, con conflictos' : 'Abierto'} />
    </span>
  )
}

function ReviewBadge({ decision }: { decision: string }) {
  switch (decision) {
    case 'APPROVED':
      return (
        <span className="badge icon-only" data-state="open">
          <IconCheck title="Aprobado" />
        </span>
      )
    case 'CHANGES_REQUESTED':
      return (
        <span className="badge icon-only" data-state="closed">
          <IconX title="Cambios pedidos" />
        </span>
      )
    case 'REVIEW_REQUIRED':
      return (
        <span className="badge icon-only" data-state="draft">
          <IconClock title="Falta review" />
        </span>
      )
    default:
      return (
        <span className="badge icon-only" data-state="draft">
          <IconClock title="Sin review todavía" />
        </span>
      )
  }
}

// ChecksBadge reproduce el resumen de GitHub ("1 skipped, 1 expected, 28
// successful checks") en vez de colapsarlo en un solo número: no es lo mismo
// que falten checks por correr que que estén salteados, y esa distinción es
// justo lo que uno mira antes de mergear.
//
// Las categorías suman el total, así que el desglose siempre cierra contra el
// número de checks que muestra GitHub.
function ChecksBadge({ pr }: { pr: PRState }) {
  const partes: string[] = []
  const push = (n: number, etiqueta: string) => {
    if (n > 0) partes.push(`${n} ${etiqueta}`)
  }
  // De lo más urgente a lo más inocuo; los exitosos al final, como GitHub.
  push(pr.checks_failing, 'fallando')
  push(pr.checks_pending, 'corriendo')
  push(pr.checks_expected, 'esperando')
  push(pr.checks_cancelled, 'cancelado')
  push(pr.checks_neutral, 'neutral')
  push(pr.checks_other, 'sin clasificar')
  push(pr.checks_skipped, 'salteado')
  push(pr.checks_success, 'ok')

  const estado =
    pr.checks_failing > 0
      ? 'closed'
      : pr.checks_pending > 0 || pr.checks_expected > 0
        ? 'draft'
        : 'open'

  return (
    <span
      className="badge checks"
      data-state={estado}
      title={`${pr.checks_total} checks del último commit`}
    >
      {partes.join(' · ')}
    </span>
  )
}

function CommentsBadge({ pr }: { pr: PRState }) {
  if (pr.unresolved_count === 0) return null
  const n = `${pr.unresolved_count}${pr.threads_truncated ? '+' : ''}`
  return (
    <span className="badge" data-state="closed" title={`${n} comments sin resolver`}>
      <IconComment title="Comments sin resolver" />
      {n}
    </span>
  )
}

// Freshness marca la edad del dato. Un check en verde de hace veinte minutos
// mostrado como actual es peor que no mostrar nada.
function Freshness({ at }: { at: number }) {
  const secs = Math.max(0, Math.round((Date.now() - at) / 1000))
  if (secs < 60) return null
  const txt = secs < 3600 ? `hace ${Math.floor(secs / 60)} min` : `hace ${Math.floor(secs / 3600)} h`
  return <p className="stale">{txt}</p>
}
