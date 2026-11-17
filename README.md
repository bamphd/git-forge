# git-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/bamphd/git-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/bamphd/git-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone, GitHub-compatible git forge: a statically linked Go core serving the whole surface
the contest scores from one in-memory store — git smart HTTP (upload-pack v2, receive-pack v0),
the REST v3 API, the GraphQL v4 API and an MCP endpoint over streamable HTTP. No git binary, no
outside service, no install step: the entrypoint is a thin Node launcher that starts the
committed Go binary and owns the trial lifecycle. Everything is deterministic: no wall clock and
no unseeded randomness reach a response, so a replayed trial answers byte for byte as the live
one did.

## Layout

- `main.ts` — the entry lifecycle: start the Go core, register its ephemeral port with the env,
  poll until the env says done, stop the core, exit 0.
- `forge-core` — the statically linked Linux amd64 Go binary (built from the Go sources below
  with `CGO_ENABLED=0`; no toolchain is needed at grade time). The launcher spawns it in place
  when executable, else from a writable temp copy.
- `main.go` — the core's life: bind an ephemeral port, print it for the launcher, serve until
  SIGTERM/SIGINT.
- `http.go` — the raw TCP HTTP/1.1 transport: head/body parsing, keep-alive, pipelining,
  routing and auth.
- `store.go` — the one store: repositories, git objects and refs, issues, comments, labels and
  pull requests, all in memory.
- `objects.go`, `pack.go`, `git.go` — git smart HTTP: object hashing, pkt-line framing,
  protocol v2 ls-refs/fetch, v0 upload-pack and receive-pack, packfile reading and writing.
- `rest.go` — the REST v3 routes.
- `graphql.go` — the GraphQL v4 endpoint: lexer, parser and executor for the documents the
  suite sends.
- `mcp.go` — the MCP endpoint: server/discover, initialize, tools/list and tools/call.
- `json.go` — the ordered JSON value, parser and serializer shared by every surface.
- `app.ts`, `lib/` — the original TypeScript implementation, kept for reference; the Go core
  is the runtime.

## Run it

```sh
node main.ts
```

Node 22.18 or later strips the TypeScript types on its own; there is nothing to install. The
process needs `MAKEFUN_ENV_URL` (the env's origin) and `MAKEFUN_SHOP_TOKEN` (the bearer token the
suite presents) in its environment, as a trial sets them.

To rebuild the core from source (Go 1.27, standard library only):

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o forge-core .
```

Built for [github-core-clone-01](https://make.fun/bounty/github-core-clone-01) by
[@7billions](https://make.fun/u/7billions) ([bamphd](https://github.com/bamphd)).

---

<sub>Entered on [make.fun](https://make.fun/?ref=3r8va27c&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
