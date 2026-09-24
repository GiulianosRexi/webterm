# Ideas

Cosas que se podrían hacer, sin orden. No es una cola: cada una se puede tomar
cuando convenga, por valor o por ganas. Lo que ya está hecho vive en el README,
y el diseño de cada pieza en `webterm-diseno.md`.

Cuando una idea se vuelve trabajo concreto, su diseño va a
`docs/superpowers/specs/` y acá queda solo el link.

## Organización

- [ ] **Tags por sesión.** El tipo de trabajo —bugfix, consulta,
      implementación, brainstorming—, varios por sesión y transversales al
      folder. Con autocompletado desde los que ya existen, porque sin eso
      aparecen `bugfix`, `bug-fix` y `fix` como tres cosas distintas y el corte
      por tipo deja de servir. Va con tool de MCP que liste los existentes, por
      la misma razón. Es la fase 3 del plan de organización: folders responde
      *de qué proyecto es* y tags *de qué clase de trabajo*.

- [ ] **Buscar sin que importe el orden de las palabras.** Hoy el matcher es
      por subsecuencia: `mejoras web` encuentra, `webterm mejoras` no. Con dos
      campos molesta poco; con tres —título, folder y tags— va a molestar,
      porque nadie recuerda en qué orden están escritos. Se arregla partiendo
      la query en palabras y exigiendo que cada una matchee por separado.

- [ ] **Subfolders.** Solo si aparece la necesidad de dividir un proyecto
      grande. Agregar `parent_id` a `folders` es una migración aditiva más. Ojo:
      el caso que lo justificaría (frontend / backend dentro de un proyecto) lo
      resuelven mejor los tags, y sin el nivel extra.

## UI

- [ ] **Ver el contexto de una sesión.** Claude escribe contexto por MCP
      (`set_context`) y hoy no hay forma de verlo desde la UI: se guarda a
      ciegas. Falta definir la UI —panel al lado del de recursos, algo dentro
      del menú de la sesión, o una vista aparte.

- [ ] **Acciones sobre los links de la terminal.** Reconocer links con una
      regex propia y ofrecer algo según lo que sean: un PR de GitHub, linkearlo
      a la sesión. La acción ya existe —`POST /api/sessions/{id}/resources` y la
      tool `link_pr`—, falta el disparador desde la terminal.

      xterm lo soporta por dos caminos. El barato es pasarle un handler al
      `WebLinksAddon` que ya está cargado (`(event, uri) => …`), que intercepta
      el click pero deja el detector del addon, o sea solo URLs. El completo es
      `term.registerLinkProvider()`, donde el matcher es propio: cada link
      define `activate`, `hover`/`leave`, y decoraciones propias, así que un
      link accionable se puede ver distinto de uno común. Ahí también entran
      cosas que no son URLs —un `ABC-123`, una ruta de archivo, un hash de
      commit.

      Lo que hay que decidir no es técnico sino de interacción: hoy el click
      abre el link y eso no se quiere perder. Opciones: ofrecer las acciones en
      el hover, reservar un modificador, o menú contextual con click derecho.
      Si se hace tooltip DOM, tiene que vivir dentro de `Terminal.element` y
      llevar la clase `xterm-hover`, o el mouse se cae a través y activa otros
      links.

- [ ] **UI multi-terminal (tabs).** Ver más de una sesión a la vez en vez de
      cambiar de una. Era M3.

- [ ] **Dashboard.** Vista de solo lectura sobre las sesiones: agrupar por
      `kanban_status` da un board, `work_status` da las señales de qué necesita
      atención ahora. Los dos campos ya existen en el schema sin usarse. Era
      parte de M7.

## Integraciones

- [ ] **CLI local `webterm`.** Un binario chico que corre *adentro* de una
      sesión y le habla al backend, para que los programas de ahí adentro
      puedan leer y escribir su propio estado. Era M5.

- [ ] **Crear sesiones desde el MCP.** Hoy las ocho tools tocan datos: leen y
      escriben metadata, KV, recursos y folders. Crear es distinto en
      naturaleza —no es escribir una fila, es pedirle al daemon que levante un
      pty—, y por eso la interfaz `Sessions` del MCP ni siquiera expone
      `Create`: existe así para poder probar las tools sin levantar procesos.
      Abrirlo pide decidir quién elige el `cwd`, si la sesión nace dentro de un
      folder, y si Claude puede crear sesiones sin que el usuario lo vea o hace
      falta alguna confirmación. El criterio que rige hoy es que borrar y crear
      son decisiones humanas y la UI ya las tiene.

- [ ] **Hooks de Claude Code.** Que `work_status` se mueva solo —idle,
      trabajando, esperando input, error— en vez de a mano. Era M6.

## Deudas conocidas

- [ ] **Renombrar los identificadores a inglés.** El código de folders y los
      helpers de test están en español. Pasada completa pendiente.

- [ ] **La muerte espontánea de un pty no llega al bus de eventos.** El daemon
      escribe el exit de forma sincrónica, así que el sweep del orquestador
      nunca la encuentra y no publica nada: la lista tarda hasta 60s salvo que
      sea la sesión que se está mirando. Cerrarlo pide un canal
      daemon → orquestador, que obliga a tocar el daemon.

- [ ] **`@xterm/addon-fit` quedó declarado sin uso** desde que el ajuste del
      terminal pasó a medir el render. Sale con un `npm uninstall`.

- [ ] **Chicas, de revisiones anteriores.** `events.New()` acepta un buffer
      inválido en silencio; borrar una sesión viva emite dos eventos (uno por
      el kill interno y otro por el borrado); `useEvents` valida que el frame
      sea JSON pero no su forma; no hay test del publish en `Restart` ni en
      `Sweep`.
