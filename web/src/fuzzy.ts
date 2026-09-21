// Matcher de subsecuencias con scoring, para el command palette.
//
// No usa una librería a propósito: fuse.js y compañía pesan decenas de KB y
// están pensadas para miles de documentos. Acá se ordenan unas pocas decenas
// de títulos, y lo único que hace falta es que "idi" encuentre "Iceberg
// decisions inline" — o sea, premiar los inicios de palabra.

export interface FuzzyMatch {
  score: number
  // Índices del target que matchearon, para poder resaltarlos.
  positions: number[]
}

// Un char arranca palabra si está al principio, si viene después de un
// separador, o si es una mayúscula precedida de minúscula (camelCase).
const SEPARATORS = new Set([' ', '-', '_', '/', '.', ':', '(', ')', '[', ']', '→'])

function startsWord(target: string, i: number): boolean {
  if (i === 0) return true
  const prev = target[i - 1]
  if (SEPARATORS.has(prev)) return true
  return prev === prev.toLowerCase() && target[i] !== target[i].toLowerCase()
}

// Pesos. La diferencia entre ellos es lo que decide el orden de los resultados:
// un inicio de palabra vale bastante más que una letra suelta en el medio, y
// dos letras pegadas valen más que dos separadas.
const BASE = 1
const WORD_START_BONUS = 10
const CONSECUTIVE_BONUS = 6
const LEADING_PENALTY = 2 // por saltear el principio del target, con tope
const MAX_LEADING_PENALTY = 10

/**
 * fuzzyMatch busca query dentro de target como subsecuencia, sin distinguir
 * mayúsculas. Devuelve null si no está.
 *
 * Los espacios de la query se ignoran: así "ice athena" encuentra un título
 * donde entre esas dos palabras hay varias más, que es como uno escribe cuando
 * recuerda el principio y el final pero no el medio.
 */
export function fuzzyMatch(query: string, target: string): FuzzyMatch | null {
  const chars = [...query.trim().toLowerCase()].filter((c) => c !== ' ')
  if (chars.length === 0) return { score: 0, positions: [] }

  const lower = target.toLowerCase()

  // Se prueba un match desde cada lugar donde aparece la primera letra y gana
  // el mejor. Tomar siempre la primera ocurrencia sería más rápido pero elige
  // mal: buscando "ice" en "combined → Iceberg" engancharía la i de
  // "combined", y el resaltado marcaría letras sueltas en vez de la palabra
  // que el usuario tenía en la cabeza. Los títulos son cortos, así que probar
  // todos los arranques no se nota.
  let best: FuzzyMatch | null = null
  for (let start = lower.indexOf(chars[0]); start !== -1; start = lower.indexOf(chars[0], start + 1)) {
    const candidate = matchFrom(chars, target, lower, start)
    if (candidate && (best === null || candidate.score > best.score)) best = candidate
  }
  return best
}

// matchFrom hace el match codicioso anclando la primera letra en `start`.
function matchFrom(
  chars: string[],
  target: string,
  lower: string,
  start: number,
): FuzzyMatch | null {
  const positions: number[] = []
  let score = 0
  let cursor = start
  let lastMatch = -1

  for (const qc of chars) {
    const found = lower.indexOf(qc, cursor)
    if (found === -1) return null

    score += BASE
    if (startsWord(target, found)) score += WORD_START_BONUS
    if (found === lastMatch + 1) score += CONSECUTIVE_BONUS
    if (positions.length === 0) {
      score -= Math.min(found * LEADING_PENALTY, MAX_LEADING_PENALTY)
    }

    positions.push(found)
    lastMatch = found
    cursor = found + 1
  }

  return { score, positions }
}

export interface Ranked<T> {
  item: T
  match: FuzzyMatch
}

/**
 * rank filtra y ordena una lista por qué tan bien matchea la query.
 *
 * El orden es estable a propósito: a igual score gana el target más corto y
 * después el alfabético, nunca la actividad reciente. Una lista que se
 * reordena sola arruina la memoria espacial —uno aprende dónde está algo y el
 * sistema se lo mueve—, y es la misma razón por la que la sidebar tampoco se
 * ordena por última actividad.
 *
 * Con la query vacía devuelve todo en el orden en que venía.
 */
export function rank<T>(query: string, items: T[], key: (item: T) => string): Ranked<T>[] {
  if (!query.trim()) {
    return items.map((item) => ({ item, match: { score: 0, positions: [] } }))
  }

  const out: Ranked<T>[] = []
  for (const item of items) {
    const match = fuzzyMatch(query, key(item))
    if (match) out.push({ item, match })
  }

  return out.sort((a, b) => {
    if (b.match.score !== a.match.score) return b.match.score - a.match.score
    const ka = key(a.item)
    const kb = key(b.item)
    if (ka.length !== kb.length) return ka.length - kb.length
    return ka.localeCompare(kb)
  })
}
