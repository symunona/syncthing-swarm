# syncthing-swarm task runner. `just` with no args -> serve.

config := "swarm.yaml"

# default: build UI + serve full dashboard (embedded, single process)
default: serve

# build frontend then run swarmd with UI embedded
serve: web
    go run -tags embedweb ./cmd/swarmd -config {{config}}

# `just dev` starts its own backend now; use this when you want it in the
# foreground of this shell instead.

# backend only, no UI (API on listen addr)
run:
    go run ./cmd/swarmd -config {{config}}

# The backend used to be your problem: `just dev` started only vite and assumed
# you had `just run` going in another pane. That bit twice in one evening —
# swarmd reads swarm.yaml once at startup, so a `stc bootstrap` that had just
# added a node (or rewritten fiona's mount) was invisible to a dashboard still
# running the old config, and nothing in the UI hinted the backend was stale.
# Restarting it here makes "did I restart it?" a question you never have to ask.
#
# NB: the doc comment `just --list` shows is the LAST contiguous comment line
# above the recipe, so the rationale lives above this blank line.

# frontend dev server + a FRESH backend on :8888 (tmux 'stdash-dev')
dev:
    # Kill the tmux session, not a `pkill -f 'go run ...'` pattern: the shell
    # running that pkill has the pattern in its OWN command line, so it matches
    # and kills the caller — this recipe took itself out exactly once. The
    # session owns the process group, so killing it takes the child binary too.
    -tmux kill-session -t stdash-dev 2>/dev/null
    # ...and the deployed binary, if `just deploy` is holding :8888. Matched by
    # process NAME (-x), which never matches this shell.
    -pkill -x swarmd 2>/dev/null
    sleep 1
    tmux new-session -d -s stdash-dev "exec go run ./cmd/swarmd -config {{config}}"
    @echo "backend restarted on :8888 in tmux 'stdash-dev' — logs: tmux attach -t stdash-dev"
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

# repair a folder shared before mesh-by-default (or with -pairwise): just remesh <folder>
# widens device membership only on nodes that already have it — never shares it anywhere new.
remesh folder:
    go run ./cmd/stc remesh {{folder}} -config {{config}}

# build everything and (re)start the local dashboard on :8888 in tmux `stdash`.
# run this at the end of a task so :8888 always matches the code.
deploy: build
    -tmux kill-session -t stdash 2>/dev/null
    -pkill -x swarmd 2>/dev/null
    sleep 1
    tmux new-session -d -s stdash "exec ./swarmd -config {{config}}"
    @echo "deployed to :8888 in tmux session 'stdash' — logs: tmux attach -t stdash"

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
