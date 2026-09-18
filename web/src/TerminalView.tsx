import { useEffect, useRef } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebLinksAddon } from '@xterm/addon-web-links'
import { WebglAddon } from '@xterm/addon-webgl'

export type ConnState = 'connecting' | 'open' | 'readonly' | 'closed' | 'exited'

// Protocolo con el backend:
//   browser -> server : binario = input crudo | texto JSON = control (resize)
//   server -> browser : binario = replay y output vivo | texto JSON = eventos
type ServerMsg =
  | { type: 'attached'; session: { pty_status: string } }
  | { type: 'ready' }
  | { type: 'exit'; code: number; reason: string }
  | { type: 'error'; error: string }

const theme = {
  background: '#11131a',
  foreground: '#d6dae4',
  cursor: '#5ac8a8',
  selectionBackground: '#2c3446',
}

export function TerminalView({
  sessionId,
  onState,
}: {
  sessionId: string
  onState: (s: ConnState) => void
}) {
  const hostRef = useRef<HTMLDivElement>(null)
  // Guardamos el callback en un ref para que el effect corra una sola vez
  // por sesión y no se reconecte en cada render del padre.
  const onStateRef = useRef(onState)
  onStateRef.current = onState

  useEffect(() => {
    const host = hostRef.current
    if (!host) return

    const term = new Terminal({
      theme,
      fontFamily: '"SF Mono", Menlo, ui-monospace, monospace',
      fontSize: 13,
      cursorBlink: true,
      allowProposedApi: true,
      scrollback: 10000,
      macOptionIsMeta: true,
      // Dibuja los bloques (█ ▀ ▄) y el box drawing de forma procedural en vez
      // de con los glyphs de la fuente, que dejan gaps entre filas. Solo tiene
      // efecto con el renderer WebGL, de ahí loadWebgl() más abajo.
      customGlyphs: true,
      rescaleOverlappingGlyphs: true,
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.loadAddon(new WebLinksAddon())
    term.open(host)
    loadWebgl(term)
    fit.fit()

    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:'
    const ws = new WebSocket(
      `${proto}//${location.host}/ws/terminal?session_id=${encodeURIComponent(sessionId)}`,
    )
    ws.binaryType = 'arraybuffer'

    const encoder = new TextEncoder()
    let exited = false
    // Una sesión ya terminada se attachea igual: llega el historial y nada
    // más, así que la mostramos de solo lectura.
    let live = true

    const sendResize = () => {
      if (ws.readyState !== WebSocket.OPEN) return
      ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }))
    }

    onStateRef.current('connecting')

    ws.onmessage = (ev) => {
      if (typeof ev.data !== 'string') {
        term.write(new Uint8Array(ev.data as ArrayBuffer))
        return
      }
      let msg: ServerMsg
      try {
        msg = JSON.parse(ev.data) as ServerMsg
      } catch {
        return
      }
      switch (msg.type) {
        case 'attached':
          live = msg.session.pty_status === 'running'
          break
        case 'ready':
          // El replay ya está escrito; recién acá sabemos si hay pty del otro
          // lado al que mandarle nuestro tamaño.
          onStateRef.current(live ? 'open' : 'readonly')
          if (live) {
            fit.fit()
            sendResize()
            term.focus()
          }
          break
        case 'exit':
          exited = true
          live = false
          onStateRef.current('exited')
          term.write(
            `\r\n\x1b[90m— el proceso terminó (${msg.reason}, código ${msg.code}) —\x1b[0m\r\n`,
          )
          break
        case 'error':
          term.write(`\r\n\x1b[31m— ${msg.error} —\x1b[0m\r\n`)
          break
      }
    }

    ws.onclose = () => {
      if (!exited) onStateRef.current('closed')
    }

    const dataSub = term.onData((data) => {
      if (live && ws.readyState === WebSocket.OPEN) ws.send(encoder.encode(data))
    })
    const resizeSub = term.onResize(() => {
      if (live) sendResize()
    })

    // El fit real depende del layout, así que lo reintentamos en cada cambio
    // de tamaño del contenedor (ventana, sidebar, zoom del browser).
    let raf = 0
    const observer = new ResizeObserver(() => {
      cancelAnimationFrame(raf)
      raf = requestAnimationFrame(() => fit.fit())
    })
    observer.observe(host)

    return () => {
      cancelAnimationFrame(raf)
      observer.disconnect()
      dataSub.dispose()
      resizeSub.dispose()
      ws.onclose = null
      ws.close()
      term.dispose()
    }
  }, [sessionId])

  return <div className="terminal-host" ref={hostRef} />
}

// loadWebgl activa el renderer WebGL, necesario para que customGlyphs tenga
// efecto (el renderer DOM siempre usa la fuente). Si no hay WebGL disponible
// seguimos con el DOM renderer: se ve peor, pero funciona.
function loadWebgl(term: Terminal) {
  try {
    const addon = new WebglAddon()
    // Al perder el contexto (GPU reset, tab dormida) disposeamos el addon:
    // xterm vuelve solo al renderer DOM en vez de quedarse en negro.
    addon.onContextLoss(() => addon.dispose())
    term.loadAddon(addon)
  } catch (err) {
    console.warn('WebTerm: sin renderer WebGL, se usa el DOM renderer', err)
  }
}
