# git-forge

A standalone, GitHub-compatible git forge in one Node 22 process, standard library only, no git
binary, no outside service. It serves the whole surface the contest scores from one in-memory
store: git smart HTTP (upload-pack v2, receive-pack v0), the REST v3 API, the GraphQL v4 API and
an MCP endpoint over streamable HTTP. Everything is deterministic: no wall clock and no unseeded
randomness reach a response, so a replayed trial answers byte for byte as the live one did.

## Layout

- `main.ts` — the entry lifecycle: bind an ephemeral port, register it with the env, poll until
  the env says done, exit 0.
- `app.ts` — the HTTP server: routing, auth and the JSON discipline every surface shares.
- `lib/store.ts` — the one store: repositories, git objects and refs, issues, comments, labels
  and pull requests, all in memory.
- `lib/git.ts` — git smart HTTP: pkt-line framing, protocol v2 ls-refs/fetch, v0 upload-pack and
  receive-pack, packfile reading and writing.
- `lib/rest.ts` — the REST v3 routes.
- `lib/graphql.ts` — the GraphQL v4 endpoint: the documents gh and github-mcp-server send.
- `lib/mcp.ts` — the MCP endpoint: server/discover, initialize, tools/list and tools/call.

## Run it

```sh
node main.ts
```

Node 22.18 or later strips the TypeScript types on its own; there is nothing to install. The
process needs `MAKEFUN_ENV_URL` (the env's origin) and `MAKEFUN_SHOP_TOKEN` (the bearer token the
suite presents) in its environment, as a trial sets them.

Built for [github-core-clone-01](https://make.fun/bounty/github-core-clone-01) by
[@7billions](https://make.fun/u/7billions) ([bamphd](https://github.com/bamphd)).
