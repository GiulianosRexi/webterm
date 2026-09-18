SHELL := /bin/bash
PORT ?= 7788
ADDR ?= 127.0.0.1:$(PORT)

.PHONY: run run-lan build build-web dev test clean

## run: buildea el frontend y levanta el backend sirviendo web/dist
run: build-web
	go run ./cmd/webterm -addr $(ADDR)

## run-lan: igual que run pero accesible desde la red local (genera token)
run-lan: build-web
	go run ./cmd/webterm -addr 0.0.0.0:$(PORT)

## build: binario en bin/webterm + frontend
build: build-web
	go build -o bin/webterm ./cmd/webterm

## build-web: build de producción del frontend
build-web: web/node_modules
	npm --prefix web run build

web/node_modules: web/package.json
	npm --prefix web install
	@touch web/node_modules

## dev: backend + Vite con HMR (frontend en http://localhost:5173)
dev: web/node_modules
	go run ./cmd/webterm -addr $(ADDR) & \
	npm --prefix web run dev; \
	kill %1

## test: tests del backend con el detector de carreras
test:
	go test ./... -race

clean:
	rm -rf bin web/dist
