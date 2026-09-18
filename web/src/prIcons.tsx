// Iconos de la card de PR.
//
// Son dibujos propios y no los octicons de GitHub: a 13px los glifos reales de
// pull request quedan ilegibles. La familia es toda circular para que se lean
// como variantes de un mismo estado, y cada uno lleva su title para que el
// significado no dependa de reconocer la forma.

type IconProps = { title: string }

function Svg({ title, children }: IconProps & { children: React.ReactNode }) {
  return (
    <svg className="icon" viewBox="0 0 16 16" aria-label={title} role="img">
      <title>{title}</title>
      {children}
    </svg>
  )
}

// Abierto: círculo con punto, el símbolo clásico de "activo".
export function IconOpen({ title }: IconProps) {
  return (
    <Svg title={title}>
      <circle cx="8" cy="8" r="5.5" fill="none" stroke="currentColor" strokeWidth="1.6" />
      <circle cx="8" cy="8" r="2" fill="currentColor" />
    </Svg>
  )
}

// Mergeado: una rama que se curva y entra en el nodo principal.
export function IconMerged({ title }: IconProps) {
  return (
    <Svg title={title}>
      <path
        d="M4 13V7a3 3 0 0 1 3-3h3"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeLinecap="round"
      />
      <circle cx="4" cy="13" r="1.9" fill="currentColor" />
      <circle cx="11.5" cy="4" r="1.9" fill="currentColor" />
    </Svg>
  )
}

// Cerrado sin mergear: círculo tachado.
export function IconClosed({ title }: IconProps) {
  return (
    <Svg title={title}>
      <circle cx="8" cy="8" r="5.5" fill="none" stroke="currentColor" strokeWidth="1.6" />
      <path
        d="M5.8 5.8l4.4 4.4M10.2 5.8l-4.4 4.4"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeLinecap="round"
      />
    </Svg>
  )
}

// Borrador: el mismo círculo pero punteado, que es como se lee "todavía no".
export function IconDraft({ title }: IconProps) {
  return (
    <Svg title={title}>
      <circle
        cx="8"
        cy="8"
        r="5.5"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeDasharray="2.4 2.2"
      />
    </Svg>
  )
}

// Aprobado.
export function IconCheck({ title }: IconProps) {
  return (
    <Svg title={title}>
      <path
        d="M3 8.6l3.6 3.6L13 4.2"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </Svg>
  )
}

// Cambios pedidos. Sin círculo, para no confundirse con el PR cerrado.
export function IconX({ title }: IconProps) {
  return (
    <Svg title={title}>
      <path
        d="M4 4l8 8M12 4l-8 8"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        strokeLinecap="round"
      />
    </Svg>
  )
}

// Falta review: un reloj, o sea "esperando a alguien".
export function IconClock({ title }: IconProps) {
  return (
    <Svg title={title}>
      <circle cx="8" cy="8" r="5.8" fill="none" stroke="currentColor" strokeWidth="1.6" />
      <path
        d="M8 4.8V8.3l2.4 1.5"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.6"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </Svg>
  )
}

// Comentarios sin resolver.
export function IconComment({ title }: IconProps) {
  return (
    <Svg title={title}>
      <path
        d="M2.2 4.2a1.4 1.4 0 0 1 1.4-1.4h8.8a1.4 1.4 0 0 1 1.4 1.4v5.2a1.4 1.4 0 0 1-1.4 1.4H7l-3.3 2.6v-2.6a1.4 1.4 0 0 1-1.5-1.4z"
        fill="none"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinejoin="round"
      />
    </Svg>
  )
}
