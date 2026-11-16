// The forge's HTTP front door: one server, one store, four surfaces. Routing is by path:
//
//   GET  /healthz                    the env's boot check, no authentication
//   POST /mcp                        JSON-RPC 2.0 (see lib/mcp.ts)
//   POST /api/graphql                GraphQL (see lib/graphql.ts)
//   *    /api/v3/...                 REST v3 (see lib/rest.ts)
//   *    /{owner}/{repo}.git/...     git smart HTTP (see lib/git.ts)
//
// Every route except /healthz requires the trial's bearer token (an exact match against
// MAKEFUN_SHOP_TOKEN when it is set; any non-empty bearer when it is not, so local runs without
// the variable still work while a missing token is always 401). Every JSON reply carries a
// non-empty X-GitHub-Request-Id and application/json; unknown routes are JSON 404s; a malformed
// JSON body is a 400; a request error never takes the server down.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { gitInfoRefs, gitReceivePack, gitUploadPack } from './lib/git.ts';
import { handleGraphQL } from './lib/graphql.ts';
import { handleMcp } from './lib/mcp.ts';
import { handleRest } from './lib/rest.ts';
import { Store, StoreError } from './lib/store.ts';

/** Bodies larger than this are refused; the suite's packs are far smaller. */
const MAX_BODY_BYTES = 256 * 1024 * 1024;

function jsonError(store: Store, res: ServerResponse, status: number, message: string): void {
  const body = Buffer.from(`${JSON.stringify({ message })}\n`, 'utf8');
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': body.length,
    'x-github-request-id': store.nextRequestId(),
  });
  res.end(body);
}

function sendBuffer(res: ServerResponse, status: number, headers: Record<string, string>, body: Buffer | null): void {
  const allHeaders: Record<string, string | number> = { ...headers };
  if (body !== null) allHeaders['content-length'] = body.length;
  else allHeaders['content-length'] = 0;
  res.writeHead(status, allHeaders);
  res.end(body ?? undefined);
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    let total = 0;
    let tooLarge = false;
    req.on('data', (chunk: Buffer) => {
      if (tooLarge) return;
      total += chunk.length;
      if (total > MAX_BODY_BYTES) {
        tooLarge = true;
        reject(new Error('request body too large'));
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => {
      if (!tooLarge) resolve(Buffer.concat(chunks));
    });
    req.on('error', reject);
  });
}

function authenticated(req: IncomingMessage): boolean {
  const header = req.headers.authorization;
  if (typeof header !== 'string' || !header.startsWith('Bearer ')) return false;
  const token = header.slice('Bearer '.length).trim();
  if (token === '') return false;
  const expected = (process.env.MAKEFUN_SHOP_TOKEN ?? '').trim();
  if (expected === '') return true;
  return token === expected;
}

function splitPath(pathname: string): string[] {
  return pathname
    .split('/')
    .filter((segment) => segment !== '')
    .map((segment) => {
      try {
        return decodeURIComponent(segment);
      } catch {
        return segment;
      }
    });
}

/** Create the forge server. main.ts binds it to an ephemeral port and registers it. */
export function createApp(): Server {
  const store = new Store();
  const server = createServer((req, res) => {
    void handleRequest(store, req, res).catch((err: unknown) => {
      if (!res.headersSent) {
        try {
          jsonError(store, res, 500, 'Internal Server Error');
        } catch {
          // The socket is gone; nothing left to do.
        }
      }
      process.stderr.write(`[forge] request error: ${err instanceof Error ? err.message : String(err)}\n`);
    });
  });
  return server;
}

async function handleRequest(store: Store, req: IncomingMessage, res: ServerResponse): Promise<void> {
  const url = new URL(req.url ?? '/', 'http://forge.invalid');
  const segments = splitPath(url.pathname);
  const method = (req.method ?? 'GET').toUpperCase();

  // The boot check: no authentication, always up.
  if (segments.length === 1 && segments[0] === 'healthz') {
    if (method !== 'GET') {
      jsonError(store, res, 404, 'Not Found');
      return;
    }
    const body = Buffer.from('{"ok":true}\n', 'utf8');
    sendBuffer(res, 200, { 'content-type': 'application/json; charset=utf-8', 'x-github-request-id': store.nextRequestId() }, body);
    return;
  }

  // Every other surface requires the bearer token, checked before any body work.
  if (!authenticated(req)) {
    jsonError(store, res, 401, 'Bad credentials');
    return;
  }

  const body = await readBody(req);

  try {
    // MCP: the raw text goes to the handler, which owns the -32700 parse error.
    if (segments.length === 1 && segments[0] === 'mcp') {
      if (method !== 'POST') {
        jsonError(store, res, 405, 'Method Not Allowed');
        return;
      }
      const reply = handleMcp(store, body.toString('utf8'));
      sendBuffer(res, reply.status, reply.headers, reply.body);
      return;
    }

    // GraphQL: one JSON object or array per POST.
    if (segments.length === 2 && segments[0] === 'api' && segments[1] === 'graphql') {
      if (method !== 'POST') {
        jsonError(store, res, 405, 'Method Not Allowed');
        return;
      }
      let parsed: unknown = null;
      if (body.length > 0) {
        try {
          parsed = JSON.parse(body.toString('utf8')) as unknown;
        } catch {
          jsonError(store, res, 400, 'Problems parsing JSON');
          return;
        }
      }
      const reply = handleGraphQL(store, parsed);
      sendBuffer(res, reply.status, reply.headers, reply.body);
      return;
    }

    // REST v3.
    if (segments.length >= 1 && segments[0] === 'api' && segments[1] === 'v3') {
      let parsed: unknown = null;
      if (body.length > 0) {
        try {
          parsed = JSON.parse(body.toString('utf8')) as unknown;
        } catch {
          jsonError(store, res, 400, 'Problems parsing JSON');
          return;
        }
      }
      const reply = handleRest(store, {
        method,
        segments: segments.slice(2),
        query: url.searchParams,
        host: req.headers.host ?? 'localhost',
        body: parsed,
      });
      sendBuffer(res, reply.status, reply.headers, reply.body);
      return;
    }

    // Git smart HTTP: /{owner}/{repo}.git/info/refs (GET) and /{owner}/{repo}.git/git-{upload,receive}-pack (POST).
    if (segments.length >= 3 && segments[1]?.endsWith('.git') === true) {
      const owner = segments[0] as string;
      const name = (segments[1] as string).slice(0, -'.git'.length);
      const repo = store.getRepo(owner, name);
      if (repo === undefined) {
        jsonError(store, res, 404, 'Not Found');
        return;
      }
      if (segments.length === 4 && segments[2] === 'info' && segments[3] === 'refs' && method === 'GET') {
        const service = url.searchParams.get('service') ?? '';
        const reply = gitInfoRefs(store, owner, name, service, req.headers['git-protocol'] ?? undefined);
        sendBuffer(res, reply.status, { 'content-type': reply.contentType, 'x-github-request-id': store.nextRequestId() }, reply.body);
        return;
      }
      if (segments.length === 3 && method === 'POST' && (segments[2] === 'git-upload-pack' || segments[2] === 'git-receive-pack')) {
        const reply = segments[2] === 'git-upload-pack' ? gitUploadPack(store, repo, body) : gitReceivePack(store, repo, body);
        sendBuffer(res, reply.status, { 'content-type': reply.contentType, 'x-github-request-id': store.nextRequestId() }, reply.body);
        return;
      }
      jsonError(store, res, 404, 'Not Found');
      return;
    }

    jsonError(store, res, 404, 'Not Found');
  } catch (err) {
    if (err instanceof StoreError) {
      jsonError(store, res, err.status, err.message);
      return;
    }
    throw err;
  }
}
