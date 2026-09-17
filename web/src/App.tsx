import { useCallback, useState } from 'react'
import { TerminalView, type ConnState } from './TerminalView'

const label: Record<ConnState, string> = {
  connecting: 'conectando…',
  open: 'conectado',
  closed: 'desconectado',
  exited: 'sesión terminada',
}

export function App() {
  const [state, setState] = useState<ConnState>('connecting')
  // M1 no tiene persistencia de sesiones: "reiniciar" remonta el componente,
  // lo que cierra el WebSocket y spawnea un pty nuevo.
  const [generation, setGeneration] = useState(0)

  const restart = useCallback(() => setGeneration((g) => g + 1), [])

  return (
    <div className="app">
      <header className="topbar">
        <span className="brand">WebTerm</span>
        <span className="status" data-state={state}>
          <span className="dot" />
          {label[state]}
        </span>
        <span className="spacer" />
        <button onClick={restart}>
          {state === 'open' ? 'Reiniciar sesión' : 'Reconectar'}
        </button>
      </header>
      <TerminalView key={generation} onState={setState} />
    </div>
  )
}
