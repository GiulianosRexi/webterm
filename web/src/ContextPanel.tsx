import { useCallback, useEffect, useState } from 'react'
import Markdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { Check, Pencil, Plus, Trash2, X } from 'lucide-react'
import { api } from './api'

type Entry = { key: string; value: string }

const toEntries = (kv: Record<string, string>): Entry[] =>
  Object.entries(kv)
    .map(([key, value]) => ({ key, value }))
    .sort((a, b) => a.key.localeCompare(b.key))

export function ContextPanel({
  sessionId,
  reloadKey,
  markdown,
  onCount,
}: {
  sessionId: string
  reloadKey: number
  markdown: boolean
  onCount: (n: number) => void
}) {
  const [entries, setEntries] = useState<Entry[]>([])
  const [loaded, setLoaded] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  const [editingKey, setEditingKey] = useState<string | null>(null)
  const [adding, setAdding] = useState(false)

  const refresh = useCallback(async () => {
    try {
      const kv = await api.kv.list(sessionId)
      setEntries(toEntries(kv))
      setError(null)
    } catch (err) {
      setError(String(err))
    } finally {
      setLoaded(true)
    }
  }, [sessionId])

  // Corre al montar y cada vez que App avisa un evento de contexto de esta
  // sesión (lo escribe Claude por MCP o lo editamos nosotros desde otra pestaña).
  useEffect(() => {
    void refresh()
  }, [refresh, reloadKey])

  // El contador del tab lo mantiene el panel: es el único que tiene los datos.
  useEffect(() => {
    onCount(entries.length)
  }, [entries.length, onCount])

  // run envuelve las escrituras: una a la vez, error visible y refresco contra
  // el server en vez de confiar en el estado local. El evento SSE que dispara la
  // escritura también refresca, pero no esperamos a que llegue.
  const run = useCallback(
    async (fn: () => Promise<unknown>) => {
      setBusy(true)
      try {
        await fn()
        setError(null)
        await refresh()
      } catch (err) {
        setError(String(err))
      } finally {
        setBusy(false)
      }
    },
    [refresh],
  )

  const remove = (key: string) => run(() => api.kv.remove(sessionId, key))

  const save = (originalKey: string, next: Entry) =>
    run(async () => {
      // Renombrar una clave es escribir la nueva y borrar la vieja: el store no
      // tiene un rename, y la clave es parte de la identidad de la fila.
      await api.kv.set(sessionId, next.key, next.value)
      if (originalKey && originalKey !== next.key) {
        await api.kv.remove(sessionId, originalKey)
      }
    })

  return (
    <div className="tab-body context-tab">
      {error && <p className="resource-error">{error}</p>}
      {loaded && entries.length === 0 && !adding && (
        <p className="empty">esta sesión no tiene contexto todavía</p>
      )}

      {entries.map((entry) =>
        editingKey === entry.key ? (
          <ContextEditor
            key={entry.key}
            initial={entry}
            busy={busy}
            onCancel={() => setEditingKey(null)}
            onSave={(next) => {
              void save(entry.key, next).then(() => setEditingKey(null))
            }}
          />
        ) : (
          <ContextCard
            key={entry.key}
            entry={entry}
            markdown={markdown}
            busy={busy}
            onEdit={() => setEditingKey(entry.key)}
            onDelete={() => void remove(entry.key)}
          />
        ),
      )}

      {adding ? (
        <ContextEditor
          initial={{ key: '', value: '' }}
          busy={busy}
          onCancel={() => setAdding(false)}
          onSave={(next) => {
            void save('', next).then(() => setAdding(false))
          }}
        />
      ) : (
        <button className="context-add" onClick={() => setAdding(true)} disabled={busy}>
          <Plus size={14} /> agregar clave
        </button>
      )}
    </div>
  )
}

function ContextCard({
  entry,
  markdown,
  busy,
  onEdit,
  onDelete,
}: {
  entry: Entry
  markdown: boolean
  busy: boolean
  onEdit: () => void
  onDelete: () => void
}) {
  return (
    <article className="context-card">
      <header>
        <span className="context-key">{entry.key}</span>
        <span className="context-actions">
          <button onClick={onEdit} disabled={busy} title="Editar" aria-label="Editar">
            <Pencil size={13} />
          </button>
          <button
            className="danger"
            onClick={onDelete}
            disabled={busy}
            title="Borrar"
            aria-label="Borrar"
          >
            <Trash2 size={13} />
          </button>
        </span>
      </header>
      {markdown ? (
        <div className="context-value markdown">
          <Markdown remarkPlugins={[remarkGfm]}>{entry.value}</Markdown>
        </div>
      ) : (
        <pre className="context-value">{entry.value}</pre>
      )}
    </article>
  )
}

function ContextEditor({
  initial,
  busy,
  onCancel,
  onSave,
}: {
  initial: Entry
  busy: boolean
  onCancel: () => void
  onSave: (next: Entry) => void
}) {
  const [key, setKey] = useState(initial.key)
  const [value, setValue] = useState(initial.value)
  const canSave = key.trim().length > 0 && !busy

  return (
    <article className="context-card editing">
      <header>
        <input
          className="context-key-input"
          value={key}
          placeholder="clave"
          onChange={(e) => setKey(e.target.value)}
        />
        <span className="context-actions">
          <button
            onClick={() => canSave && onSave({ key: key.trim(), value })}
            disabled={!canSave}
            title="Guardar"
            aria-label="Guardar"
          >
            <Check size={13} />
          </button>
          <button onClick={onCancel} disabled={busy} title="Cancelar" aria-label="Cancelar">
            <X size={13} />
          </button>
        </span>
      </header>
      <textarea
        className="context-value-input"
        value={value}
        placeholder="valor"
        rows={Math.min(12, Math.max(3, value.split('\n').length))}
        onChange={(e) => setValue(e.target.value)}
      />
    </article>
  )
}
