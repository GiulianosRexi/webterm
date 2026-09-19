import { useEffect, useRef } from 'react'

// Los eventos dicen qué cambió, nunca el objeto: el cliente va a buscarlo por
// REST. Ver docs/superpowers/specs/2026-09-19-canal-de-eventos-design.md
export type EventKind =
  | 'resource.added'
  | 'resource.removed'
  | 'session.created'
  | 'session.updated'
  | 'session.deleted'

export interface ServerEvent {
  seq: number
  kind: EventKind
  session_id: string
}

// EventSource entrega por nombre de evento: lo que no se escucha explícitamente
// no llega a onmessage, porque los frames van con event: <kind>.
const KINDS: EventKind[] = [
  'resource.added',
  'resource.removed',
  'session.created',
  'session.updated',
  'session.deleted',
]

/**
 * useEvents abre el stream y llama a onEvent por cada cambio.
 *
 * onEvent recibe null cuando hay que resincronizar todo: al abrir el stream
 * (incluidas las reconexiones, que EventSource maneja solo) y cuando se detecta
 * un salto en seq, que es como se ve desde acá un evento que el server nos
 * salteó por venir lentos. El server no guarda historial, así que pedir "lo que
 * falta" no es una opción: se refetchea entero.
 */
export function useEvents(onEvent: (ev: ServerEvent | null) => void) {
  // El callback va en un ref para que el efecto corra una sola vez y no
  // reabra el stream en cada render del que lo usa.
  const cb = useRef(onEvent)
  cb.current = onEvent

  useEffect(() => {
    // El browser limita a ~6 conexiones HTTP/1.1 concurrentes por origen, y
    // esto es por perfil, no por pestaña: cada pestaña abierta deja un
    // EventSource permanente en ese pool, que fetch (api.ts) también usa. A
    // partir de la sexta pestaña no queda ningún slot libre y toda la UI se
    // congela sin error, sin que el stream ni el poll de 60s puedan
    // recuperarla. Por eso el stream se cierra cuando la pestaña queda oculta
    // y se reabre al volver a primer plano: así el límite es por pestañas
    // VISIBLES, no por pestañas abiertas. Una pestaña en segundo plano se
    // conforma con el poll de 60s. No hace falta lógica extra al reabrir:
    // el server manda un resync apenas se conecta, que es exactamente el
    // refetch completo que una pestaña que vuelve necesita.
    let cleanup: (() => void) | null = null

    const open = () => {
      const es = new EventSource('/api/events')
      let lastSeq = 0

      const onResync = () => {
        lastSeq = 0
        cb.current(null)
      }

      const onKind = (e: MessageEvent) => {
        let ev: ServerEvent
        try {
          ev = JSON.parse(e.data) as ServerEvent
        } catch {
          return
        }
        if (lastSeq !== 0 && ev.seq !== lastSeq + 1) cb.current(null)
        else cb.current(ev)
        lastSeq = ev.seq
      }

      es.addEventListener('resync', onResync)
      for (const k of KINDS) es.addEventListener(k, onKind)

      return () => {
        es.removeEventListener('resync', onResync)
        for (const k of KINDS) es.removeEventListener(k, onKind)
        es.close()
      }
    }

    if (document.visibilityState === 'visible') cleanup = open()

    const onVisibility = () => {
      if (document.visibilityState === 'hidden') {
        cleanup?.()
        cleanup = null
      } else {
        cleanup ??= open()
      }
    }
    document.addEventListener('visibilitychange', onVisibility)

    return () => {
      document.removeEventListener('visibilitychange', onVisibility)
      cleanup?.()
      cleanup = null
    }
  }, [])
}
