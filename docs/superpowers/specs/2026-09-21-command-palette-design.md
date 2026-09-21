# Command palette (fase 1: fuzzy sobre sesiones)

Fecha: 2026-09-21
Estado: aprobado

## Problema

Cambiar de sesión obliga a buscarla con la vista en la sidebar. Con varias
sesiones abiertas eso es lento, y es el problema real detrás de "me pierdo
entre muchas sesiones": es un problema de **acceso**, no de organización.

Esta es la primera de tres fases. Las siguientes agregan folders (proyectos) y
tags (tipo de trabajo), y ambas se enganchan a este mismo palette. El orden es
deliberado: el palette funciona hoy con los títulos que ya existen, y es además
lo que le da utilidad a los tags — un tag sin buscador es una etiqueta
decorativa.

## Alcance de esta fase

Buscar sesiones por título y saltar a una. Nada más: sin folders, sin tags, sin
secciones por tipo de entidad, sin acciones sobre la sesión encontrada.

## El atajo: Cmd+K (Ctrl+K fuera de macOS)

Los candidatos descartados y por qué:

- **Ctrl+P**: lo necesita el shell — es `previous command` en readline, además
  de ser paste en la terminal del usuario.
- **Cmd+P**: imprimir, en Chrome.
- **Ctrl+Space**: `set mark` en emacs/readline, y en macOS cambia el input
  source cuando hay más de un idioma configurado.

`Cmd+K` es el estándar de facto de los command palettes web, `Cmd` no viaja al
pty, y deja `Ctrl+P` y `Ctrl+K` libres para el shell. Chrome lo usa para buscar
desde la omnibox, pero `preventDefault()` gana con la página enfocada.

**Dos detalles que lo hacen funcionar o lo rompen:**

1. Con el foco dentro del terminal, xterm.js se queda con el teclado. El
   listener va en `window` en fase de **captura**, para verlo antes que xterm.
2. Al cerrar hay que **devolver el foco al terminal**. Si no, se cierra el
   palette y no se puede tipear: la peor forma de romper una terminal.

## Componentes

- **`web/src/session.ts`** (nuevo): `sessionLabel(s)`, que decide qué mostrar
  cuando una sesión no tiene título. Hoy vive dentro de `SessionList.tsx` y el
  palette necesita exactamente lo mismo, así que se mueve en vez de duplicarse.
  Se renombra porque `App.tsx` ya tiene una constante `label`.
- **`web/src/fuzzy.ts`** (nuevo): el matcher. Sin dependencias: traer una
  librería serían decenas de KB para ordenar unas pocas decenas de strings.
- **`web/src/CommandPalette.tsx`** (nuevo): el overlay.
- **`web/src/App.tsx`**: estado de apertura, listener global, y el salto a la
  sesión elegida (que ya es `setSelected`).

## El matcher

`fuzzyMatch(query, target)` devuelve `null` si no matchea, o un score con las
posiciones que matchearon para poder resaltarlas.

Es subsequence matching case-insensitive con bonus por inicio de palabra
(después de un separador o en un camelCase) y por caracteres consecutivos. Eso
hace que `idi` encuentre `Iceberg decisions inline` y que `webterm` le gane a un
título donde esas letras aparecen desperdigadas. Los espacios de la query no
tienen que matchear, así se puede buscar `ice athena` salteando el medio.

**El orden de resultados es estable**: a score igual desempata el target más
corto y después el orden alfabético, nunca la actividad reciente. Es la misma
razón por la que la sidebar no se reordena sola — una lista que se mueve
destruye la memoria espacial y hace más difícil encontrar por vista.

Con la query vacía se muestran todas las sesiones en el orden en que ya están.

## Verificación

No hay runner de tests de frontend y no se agrega uno en esta fase. El matcher
es una función pura y se verifica con un script ejecutable contra casos reales
(los títulos de sesiones que existen hoy). El resto es `tsc -b` más prueba
manual en el navegador.
