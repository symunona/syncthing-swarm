# syncthing-dashboard

Fleet dashboard + `stc` CLI for a personal syncthing swarm. Go backend
(`swarmd`, `cmd/stc`), SolidJS frontend (`web/`, Tailwind, built into
`internal/webui/dist` and embedded via the `embedweb` build tag).

## Deploy at the end of every task

After finishing a task, **always redeploy the local dashboard to
:8888** so what's running matches the code:

```bash
just deploy
```

`just deploy` builds the UI + `swarmd` (embedded) + `stc`, stops the old
:8888 process, and starts the new binary in a named tmux session
(`stdash`). Attach to watch logs: `tmux attach -t stdash`.

The real cred store is `swarm.yaml` (gitignored, 0600). Nodes:
pandora (local hub), fiona, taskbot, papi, rue.
