.PHONY: dev web build run test clean

# backend only (API on :8888), no embedded UI
run:
	go run ./cmd/swarmd

# frontend dev server (hot reload, proxies /api -> :8888)
dev:
	npm --prefix web run dev

# build frontend into internal/webui/dist
web:
	npm --prefix web install
	npm --prefix web run build

# single self-contained binary with UI embedded
build: web
	go build -tags embedweb -o swarmd ./cmd/swarmd
	@echo "built ./swarmd"

test:
	go test ./...

clean:
	rm -f swarmd
	rm -rf internal/webui/dist web/node_modules
