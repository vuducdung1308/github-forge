# github-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/vuducdung1308/github-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/vuducdung1308/github-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

A standalone, GitHub-compatible git forge with zero dependencies and no git binary. It is written twice from the same design: in Go (`go/`, built into the statically linked binaries under `bin/`, the one that runs) and in Node 22 TypeScript (the fallback for platforms with no binary). One in-memory store serves four surfaces:

- **git smart HTTP** at `/{owner}/{repo}.git`: upload-pack over protocol v2 (`ls-refs` with symrefs, peeling and ref-prefix; `fetch` with shallow/deepen, thin packs and side-band framing), a v0 fallback, and receive-pack v0 with `report-status`, `side-band-64k` and `delete-refs`. Packs are parsed with ofs/ref deltas and written on `node:zlib` and `node:crypto` only. Every ref update must fast-forward.
- **REST v3** under `/api/v3`: repositories (with `Link` pagination), branches, git refs/commits/trees/blobs, contents (create, update with stale-sha 409, delete, directory listings), labels, issues, comments, and pull requests (files, three-way squash merges, 405 on a conflict).
- **GraphQL v4** at `/api/graphql`: a hand-written parser and executor for the documents `gh` and `github-mcp-server` send, with introspection (`__type`, `__schema`), Relay `node(id:)`, cursor connections, mutations, operation batching, and unknown fields reported in `errors`.
- **MCP** at `/mcp`: JSON-RPC 2.0 over Streamable HTTP, revision 2026-07-28 (`server/discover`) and the `initialize` handshake revisions, with 16 tools acting on the same store as REST.

Layout: `main.ts` is the entrypoint; it starts `bin/forge-<platform>-<arch>` when one matches, else `node-forge.ts`. `go/` is the Go forge, standard library only: `main.go` (a raw HTTP/1.1 server, routing, auth and the trial lifecycle), `git.go`, `store.go`, `smarthttp.go`, `rest.go`, `graphql.go` and `mcp.go`. The Node forge is `node-forge.ts` (lifecycle), `app.ts` (routing and auth) and `src/`, which holds `httpd.ts` (a raw HTTP/1.1 server on `node:net`: keep-alive, pipelining, one socket write per response), `git.ts` (objects, packs, deltas, pkt-lines), `store.ts` (repositories, server-side commits, merges, diffs), `smart-http.ts`, `rest.ts`, `graphql.ts` and `mcp.ts`.

## Run it

```sh
# build the static binaries (Go 1.27)
cd go && for a in amd64 arm64; do CGO_ENABLED=0 GOOS=linux GOARCH=$a go build -trimpath -ldflags="-s -w" -o ../bin/forge-linux-$a .; done; cd ..

# standalone on port 7811, accepting the token "tok" (FORGE_IMPL=node forces the Node forge)
FORGE_PORT=7811 MAKEFUN_SHOP_TOKEN=tok node main.ts

# as a trial entry: registers with the env and exits when it says done
MAKEFUN_ENV_URL=http://store:8080 MAKEFUN_SHOP_TOKEN=... node main.ts
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=0d8jtjfq&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
