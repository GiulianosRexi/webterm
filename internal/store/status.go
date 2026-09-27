package store

import "slices"

// KanbanStatuses son los estados de trabajo de una sesión, en el orden en que
// avanza el trabajo. La API, el MCP y la UI validan y ordenan contra esta
// lista.
//
// "todo" es el valor de siempre para "todavía no arrancó": es el DEFAULT de la
// columna y lo que escribe el daemon al crear una sesión, así que renombrarlo
// obligaría a tocar el daemon. La columna es un TEXT sin CHECK, por eso sumar
// estados no pide migración.
var KanbanStatuses = []string{"todo", "in_progress", "blocked", "in_review", "needs_testing", "done"}

// ValidKanbanStatus dice si un valor es uno de KanbanStatuses.
func ValidKanbanStatus(s string) bool {
	return slices.Contains(KanbanStatuses, s)
}
