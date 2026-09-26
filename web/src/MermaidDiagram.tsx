import { useEffect, useId, useState } from 'react'

// mermaid pesa bastante más que el resto del bundle y casi ningún contexto
// trae diagramas: se carga recién la primera vez que hace falta y se comparte
// entre todos los bloques.
let mermaidPromise: Promise<typeof import('mermaid').default> | null = null

const loadMermaid = () => {
  mermaidPromise ??= import('mermaid').then(({ default: mermaid }) => {
    mermaid.initialize({ startOnLoad: false, theme: 'dark', securityLevel: 'strict' })
    return mermaid
  })
  return mermaidPromise
}

export function MermaidDiagram({ source }: { source: string }) {
  // useId trae ":" y mermaid lo usa como id de un elemento del DOM.
  const id = `mermaid-${useId().replace(/:/g, '')}`
  const [svg, setSvg] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    loadMermaid()
      .then((mermaid) => mermaid.render(id, source))
      .then(({ svg }) => {
        if (cancelled) return
        setSvg(svg)
        setError(null)
      })
      .catch((err) => {
        if (cancelled) return
        setSvg(null)
        setError(String(err))
      })
    return () => {
      cancelled = true
    }
  }, [id, source])

  // Si el diagrama no parsea mostramos el código tal cual, con el error arriba:
  // es texto que escribió Claude y conviene poder leerlo igual.
  if (error) {
    return (
      <div className="mermaid-error">
        <p className="resource-error">{error}</p>
        <pre>
          <code>{source}</code>
        </pre>
      </div>
    )
  }
  if (!svg) return <pre className="mermaid-loading">cargando diagrama…</pre>
  return <div className="mermaid-diagram" dangerouslySetInnerHTML={{ __html: svg }} />
}
