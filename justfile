# syncthing-swarm task runner. `just` with no args -> serve.

config := "swarm.yaml"

# default: build UI + serve full dashboard (embedded, single process)
default: serve

# build frontend then run swarmd with UI embedded
serve: web
    go run -tags embedweb ./cmd/swarmd -config {{config}}

# backend only, no UI (API on listen addr) — pair with `just dev`
run:
    go run ./cmd/swarmd -config {{config}}

# frontend dev server: hot reload, proxies /api -> :8888 (run `just run` too)
dev:
    pnpm --dir web run dev

# build frontend into internal/webui/dist
web:
    pnpm --dir web install
    pnpm --dir web run build

# compile self-contained binary ./swarmd (UI embedded) + stc CLI
build: web
    go build -tags embedweb -o swarmd ./cmd/swarmd
    go build -o stc ./cmd/stc
    @echo "built ./swarmd and ./stc"

# run the stc CLI, config injected: just stc list devices
stc *args:
    go run ./cmd/stc {{args}} -config {{config}}

# share a folder from local -> target: just share <folder> <target>
share folder target:
    go run ./cmd/stc share {{folder}} {{target}} -config {{config}}

# stop sharing (no confirm): just unshare <folder> <target>
unshare folder target:
    go run ./cmd/stc unshare {{folder}} {{target}} -config {{config}}

# run go tests
test:
    go test ./...

# copy the example cred store if you have none yet
init:
    test -f {{config}} || cp swarm.example.yaml {{config}}
    @echo "edit {{config}} -> add your nodes + apikeys"

# remove build artifacts
clean:
    rm -f swarmd
    rm -rf internal/webui/dist web/node_modules
