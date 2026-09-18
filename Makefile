SHELL := /bin/bash
PORT ?= 7788
ADDR ?= 127.0.0.1:$(PORT)

.PHONY: run run-lan build build-go build-web dev test clean daemon-status daemon-restart daemon-stop

## run: buildea el frontend y levanta el backend sirviendo web/dist
run: build
	./bin/webterm -addr $(ADDR)

## run-lan: igual que run pero accesible desde la red local (genera token)
run-lan: build
	./bin/webterm -addr 0.0.0.0:$(PORT)

## build: binario en bin/webterm + frontend
build: build-web build-go

## build-go: solo el binario, sin tocar el frontend
build-go:
	go build -o bin/webterm ./cmd/webterm

## build-web: build de producción del frontend
build-web: web/node_modules
	npm --prefix web run build

web/node_modules: web/package.json
	npm --prefix web install
	@touch web/node_modules

## dev: backend + Vite con HMR (frontend en http://localhost:5173)
dev: build web/node_modules
	./bin/webterm -addr $(ADDR) & \
	npm --prefix web run dev; \
	kill %1

## test: tests del backend con el detector de carreras
test:
	go test ./... -race

clean:
	rm -rf bin web/dist

# Los targets del daemon dependen de build-go y no de build: preguntar el pid
# del daemon —o pedirle que se apague— no tiene por qué recompilar React.

## daemon-status: qué daemon está corriendo y cuántas sesiones tiene
daemon-status: build-go
	./bin/webterm daemon status

## daemon-restart: reinicia el daemon. MATA LAS SESIONES VIVAS (pide confirmación).
daemon-restart: build-go
	./bin/webterm daemon restart

## daemon-stop: detiene el daemon. MATA LAS SESIONES VIVAS (pide confirmación).
daemon-stop: build-go
	./bin/webterm daemon stop
