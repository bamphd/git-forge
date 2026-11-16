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
//
// The transport is a raw TCP server speaking HTTP/1.1 directly (see the parser below): the
// benchmark workloads are thousands of small requests, and skipping the http module's stream
// machinery, per-request objects and header re-serialization is worth a fifth of the wall
// time. The parser is deliberately strict where the wire format is unambiguous (request line,
// header shape, content-length) and lenient where clients vary (LF-only line endings, folded
// whitespace), and it implements the framing the clients use: content-length bodies, chunked
// bodies, keep-alive, pipelining and Expect: 100-continue. Dispatch itself is synchronous:
// once a request's bytes are complete, its reply is built and written before the next request
// on the socket is read, so responses always leave in request order.

import { createServer, type Server, type Socket } from 'node:net';
import { gitInfoRefs, gitReceivePack, gitUploadPack } from './lib/git.ts';
import { handleGraphQL } from './lib/graphql.ts';
import { handleMcp } from './lib/mcp.ts';
import { handleRest } from './lib/rest.ts';
import { Store, StoreError } from './lib/store.ts';

/** Bodies larger than this are refused; the suite's packs are far smaller. */
const MAX_BODY_BYTES = 256 * 1024 * 1024;
/** Request head (line + headers) larger than this is refused, as the http module would. */
const MAX_HEAD_BYTES = 16 * 1024;
/** The interim answer to `Expect: 100-continue`, sent as soon as the head is parsed. */
const CONTINUE_100 = Buffer.from('HTTP/1.1 100 Continue\r\n\r\n', 'ascii');
const EMPTY_BODY = Buffer.alloc(0);
const HEALTHZ_BODY = Buffer.from('{"ok":true}\n', 'utf8');

const REASONS: Readonly<Record<number, string>> = {
  200: 'OK',
  201: 'Created',
  204: 'No Content',
  400: 'Bad Request',
  401: 'Unauthorized',
  404: 'Not Found',
  405: 'Method Not Allowed',
  409: 'Conflict',
  422: 'Unprocessable Entity',
  431: 'Request Header Fields Too Large',
  500: 'Internal Server Error',
};

// --------------------------------------------------------------------------- replies

interface Reply {
  readonly status: number;
  /** Header lines in wire order, without the trailing CRLF; content-length is appended. */
  readonly headers: string[];
  readonly body: Buffer;
}

function jsonErrorReply(store: Store, status: number, message: string): Reply {
  return {
    status,
    headers: [
      'content-type: application/json; charset=utf-8',
      `x-github-request-id: ${store.nextRequestId()}`,
    ],
    body: Buffer.from(`${JSON.stringify({ message })}\n`, 'utf8'),
  };
}

function toReply(status: number, headers: Record<string, string>, body: Buffer | null): Reply {
  const lines: string[] = [];
  for (const [name, value] of Object.entries(headers)) lines.push(`${name}: ${value}`);
  return { status, headers: lines, body: body ?? EMPTY_BODY };
}

/** Serialize one reply: status line, headers, content-length, blank line, body. */
function serializeReply(reply: Reply, close: boolean, headOnly: boolean): Buffer {
  const head =
    `HTTP/1.1 ${reply.status} ${REASONS[reply.status] ?? ''}\r\n` +
    reply.headers.map((line) => `${line}\r\n`).join('') +
    `content-length: ${reply.body.length}\r\n` +
    (close ? 'connection: close\r\n' : '') +
    '\r\n';
  const headBuffer = Buffer.from(head, 'ascii');
  if (headOnly) return headBuffer;
  const out = Buffer.allocUnsafe(headBuffer.length + reply.body.length);
  headBuffer.copy(out, 0);
  reply.body.copy(out, headBuffer.length);
  return out;
}

// --------------------------------------------------------------------------- request parsing

/** Shared empty query: no route mutates it (pagination copies before it sets). */
const EMPTY_PARAMS = new URLSearchParams();

/**
 * Split a request target into decoded path segments and a query, without a WHATWG URL parse:
 * the suite's targets are origin-form paths, so a manual split is exact and far cheaper. An
 * absolute-form target has its scheme and authority stripped; a fragment is dropped, as a URL
 * parse would. Segments are percent-decoded only when they contain a '%'.
 */
function parseRequestTarget(target: string): { segments: string[]; query: URLSearchParams } {
  let path = target;
  if (path.startsWith('http://') || path.startsWith('https://')) {
    const slash = path.indexOf('/', path.indexOf('://') + 3);
    path = slash < 0 ? '/' : path.slice(slash);
  }
  let end = path.length;
  const hash = path.indexOf('#');
  if (hash >= 0) end = hash;
  const question = path.indexOf('?');
  const hasQuery = question >= 0 && question < end;
  const pathname = hasQuery ? path.slice(0, question) : path.slice(0, end);
  const segments: string[] = [];
  for (const segment of pathname.split('/')) {
    if (segment === '') continue;
    if (segment.includes('%')) {
      try {
        segments.push(decodeURIComponent(segment));
      } catch {
        segments.push(segment);
      }
    } else {
      segments.push(segment);
    }
  }
  const query = hasQuery && question + 1 < end ? new URLSearchParams(path.slice(question + 1, end)) : EMPTY_PARAMS;
  return { segments, query };
}

interface RequestHead {
  readonly method: string;
  readonly target: string;
  readonly version: '1.1' | '1.0';
  readonly headers: Map<string, string>;
  /** Bytes consumed by the head, including its blank line. */
  readonly consumed: number;
  /** Whether the client waits for `100 Continue` before sending the body. */
  readonly expectsContinue: boolean;
}

type HeadParse = { status: 'need-more' } | { status: 'bad' } | { status: 'ok'; head: RequestHead };

/** Find the end of a request head: the blank line, CRLF or LF terminated. */
function findHeadEnd(buffer: Buffer): number {
  const crlf = buffer.indexOf('\r\n\r\n');
  const lf = buffer.indexOf('\n\n');
  if (crlf < 0) return lf < 0 ? -1 : lf + 2;
  if (lf < 0) return crlf + 4;
  return lf + 2 < crlf + 4 ? lf + 2 : crlf + 4;
}

function parseHead(buffer: Buffer): HeadParse {
  const end = findHeadEnd(buffer);
  if (end < 0) {
    if (buffer.length > MAX_HEAD_BYTES) return { status: 'bad' };
    return { status: 'need-more' };
  }
  if (end > MAX_HEAD_BYTES + 4) return { status: 'bad' };
  const text = buffer.subarray(0, end).toString('ascii');
  const lines = text.split(/\r\n|\n/);
  const requestLine = lines[0] ?? '';
  const parts = requestLine.split(' ');
  if (parts.length !== 3) return { status: 'bad' };
  const method = parts[0] as string;
  const target = parts[1] as string;
  const versionRaw = parts[2] as string;
  if (method === '' || target === '') return { status: 'bad' };
  if (versionRaw !== 'HTTP/1.1' && versionRaw !== 'HTTP/1.0') return { status: 'bad' };
  const headers = new Map<string, string>();
  for (let i = 1; i < lines.length; i += 1) {
    const line = lines[i] as string;
    if (line === '') continue;
    const colon = line.indexOf(':');
    if (colon <= 0) return { status: 'bad' };
    const name = line.slice(0, colon).trim().toLowerCase();
    if (name === '' || /[\s\x00-\x1f\x7f]/.test(name)) return { status: 'bad' };
    const value = line.slice(colon + 1).trim();
    const previous = headers.get(name);
    headers.set(name, previous === undefined ? value : `${previous}, ${value}`);
  }
  const expect = headers.get('expect') ?? '';
  return {
    status: 'ok',
    head: {
      method,
      target,
      version: versionRaw === 'HTTP/1.1' ? '1.1' : '1.0',
      headers,
      consumed: end,
      expectsContinue: expect.toLowerCase().split(',').some((token) => token.trim() === '100-continue'),
    },
  };
}

type BodyParse =
  | { status: 'need-more' }
  | { status: 'bad' }
  | { status: 'too-large' }
  | { status: 'ok'; body: Buffer; consumed: number };

/** Parse a content-length from the head: one consistent value, or null when absent. */
function contentLengthOf(head: RequestHead): { status: 'ok'; length: number | null } | { status: 'bad' } {
  const raw = head.headers.get('content-length');
  if (raw === undefined) return { status: 'ok', length: null };
  const values = raw.split(',').map((part) => part.trim());
  let length: number | null = null;
  for (const value of values) {
    if (!/^\d+$/.test(value)) return { status: 'bad' };
    const parsed = Number(value);
    if (!Number.isSafeInteger(parsed) || parsed < 0) return { status: 'bad' };
    if (length === null) length = parsed;
    else if (length !== parsed) return { status: 'bad' };
  }
  return { status: 'ok', length };
}

function parseBody(buffer: Buffer, head: RequestHead): BodyParse {
  const transferEncoding = head.headers.get('transfer-encoding') ?? '';
  const chunked = transferEncoding
    .toLowerCase()
    .split(',')
    .some((token) => token.trim() === 'chunked');
  if (chunked) return parseChunkedBody(buffer);
  const lengthResult = contentLengthOf(head);
  if (lengthResult.status === 'bad') return { status: 'bad' };
  const length = lengthResult.length ?? 0;
  if (length > MAX_BODY_BYTES) return { status: 'too-large' };
  if (buffer.length < length) return { status: 'need-more' };
  return { status: 'ok', body: buffer.subarray(0, length), consumed: length };
}

/** Parse a chunked body: size lines, chunks, the zero chunk, trailers. Incomplete -> need-more. */
function parseChunkedBody(buffer: Buffer): BodyParse {
  const chunks: Buffer[] = [];
  let total = 0;
  let offset = 0;
  for (;;) {
    const lineEnd = buffer.indexOf('\r\n', offset);
    const lfEnd = buffer.indexOf('\n', offset);
    const eol = lineEnd >= 0 && (lfEnd < 0 || lineEnd <= lfEnd) ? lineEnd : lfEnd;
    if (eol < 0) return { status: 'need-more' };
    const eolLength = buffer[eol] === 0x0d ? 2 : 1;
    const sizeText = buffer.subarray(offset, eol).toString('ascii').split(';', 1)[0]?.trim() ?? '';
    if (!/^[0-9a-fA-F]+$/.test(sizeText)) return { status: 'bad' };
    const size = Number.parseInt(sizeText, 16);
    if (!Number.isSafeInteger(size) || size < 0) return { status: 'bad' };
    offset = eol + eolLength;
    if (size === 0) {
      // Trailers end at the first blank line; their content is ignored.
      for (;;) {
        const trailerEnd = buffer.indexOf('\r\n', offset);
        const trailerLf = buffer.indexOf('\n', offset);
        const trailerEol = trailerEnd >= 0 && (trailerLf < 0 || trailerEnd <= trailerLf) ? trailerEnd : trailerLf;
        if (trailerEol < 0) return { status: 'need-more' };
        const trailerLength = buffer[trailerEol] === 0x0d ? 2 : 1;
        if (trailerEol === offset) return { status: 'ok', body: Buffer.concat(chunks, total), consumed: offset + trailerLength };
        offset = trailerEol + trailerLength;
      }
    }
    total += size;
    if (total > MAX_BODY_BYTES) return { status: 'too-large' };
    if (buffer.length < offset + size + 2) return { status: 'need-more' };
    chunks.push(buffer.subarray(offset, offset + size));
    offset += size;
    if (buffer[offset] !== 0x0d || buffer[offset + 1] !== 0x0a) {
      // A chunk must end CRLF; a lone LF is tolerated the way the head parser tolerates it.
      if (buffer[offset] !== 0x0a) return { status: 'bad' };
      offset += 1;
    } else {
      offset += 2;
    }
  }
}

/** Whether the connection closes after this request's reply (1.0 default, or `close` asked). */
function wantsClose(head: RequestHead): boolean {
  const connection = head.headers.get('connection') ?? '';
  const tokens = connection.toLowerCase().split(',').map((token) => token.trim());
  if (tokens.includes('close')) return true;
  if (head.version === '1.0') return !tokens.includes('keep-alive');
  return false;
}

// --------------------------------------------------------------------------- dispatch

function authenticated(headers: Map<string, string>): boolean {
  const header = headers.get('authorization');
  if (header === undefined || !header.startsWith('Bearer ')) return false;
  const token = header.slice('Bearer '.length).trim();
  if (token === '') return false;
  const expected = (process.env.MAKEFUN_SHOP_TOKEN ?? '').trim();
  if (expected === '') return true;
  return token === expected;
}

function dispatch(store: Store, head: RequestHead, body: Buffer): Reply {
  const { segments, query } = parseRequestTarget(head.target);
  const method = head.method.toUpperCase();

  // The boot check: no authentication, always up.
  if (segments.length === 1 && segments[0] === 'healthz') {
    if (method !== 'GET') return jsonErrorReply(store, 404, 'Not Found');
    return {
      status: 200,
      headers: [
        'content-type: application/json; charset=utf-8',
        `x-github-request-id: ${store.nextRequestId()}`,
      ],
      body: HEALTHZ_BODY,
    };
  }

  // Every other surface requires the bearer token, checked before any body work.
  if (!authenticated(head.headers)) return jsonErrorReply(store, 401, 'Bad credentials');

  try {
    // MCP: the raw text goes to the handler, which owns the -32700 parse error.
    if (segments.length === 1 && segments[0] === 'mcp') {
      if (method !== 'POST') return jsonErrorReply(store, 405, 'Method Not Allowed');
      const reply = handleMcp(store, body.toString('utf8'));
      return toReply(reply.status, reply.headers, reply.body);
    }

    // GraphQL: one JSON object or array per POST.
    if (segments.length === 2 && segments[0] === 'api' && segments[1] === 'graphql') {
      if (method !== 'POST') return jsonErrorReply(store, 405, 'Method Not Allowed');
      let parsed: unknown = null;
      if (body.length > 0) {
        try {
          parsed = JSON.parse(body.toString('utf8')) as unknown;
        } catch {
          return jsonErrorReply(store, 400, 'Problems parsing JSON');
        }
      }
      const reply = handleGraphQL(store, parsed);
      return toReply(reply.status, reply.headers, reply.body);
    }

    // REST v3.
    if (segments.length >= 1 && segments[0] === 'api' && segments[1] === 'v3') {
      let parsed: unknown = null;
      if (body.length > 0) {
        try {
          parsed = JSON.parse(body.toString('utf8')) as unknown;
        } catch {
          return jsonErrorReply(store, 400, 'Problems parsing JSON');
        }
      }
      const reply = handleRest(store, {
        method,
        segments: segments.slice(2),
        query,
        host: head.headers.get('host') ?? 'localhost',
        body: parsed,
      });
      return toReply(reply.status, reply.headers, reply.body);
    }

    // Git smart HTTP: /{owner}/{repo}.git/info/refs (GET) and /{owner}/{repo}.git/git-{upload,receive}-pack (POST).
    if (segments.length >= 3 && segments[1]?.endsWith('.git') === true) {
      const owner = segments[0] as string;
      const name = (segments[1] as string).slice(0, -'.git'.length);
      const repo = store.getRepo(owner, name);
      if (repo === undefined) return jsonErrorReply(store, 404, 'Not Found');
      if (segments.length === 4 && segments[2] === 'info' && segments[3] === 'refs' && method === 'GET') {
        const service = query.get('service') ?? '';
        const reply = gitInfoRefs(store, owner, name, service, head.headers.get('git-protocol'));
        return toReply(reply.status, { 'content-type': reply.contentType, 'x-github-request-id': store.nextRequestId() }, reply.body);
      }
      if (segments.length === 3 && method === 'POST' && (segments[2] === 'git-upload-pack' || segments[2] === 'git-receive-pack')) {
        const reply = segments[2] === 'git-upload-pack' ? gitUploadPack(store, repo, body) : gitReceivePack(store, repo, body);
        return toReply(reply.status, { 'content-type': reply.contentType, 'x-github-request-id': store.nextRequestId() }, reply.body);
      }
      return jsonErrorReply(store, 404, 'Not Found');
    }

    return jsonErrorReply(store, 404, 'Not Found');
  } catch (err) {
    if (err instanceof StoreError) return jsonErrorReply(store, err.status, err.message);
    throw err;
  }
}

// --------------------------------------------------------------------------- connections

/** Create the forge server. main.ts binds it to an ephemeral port and registers it. */
export function createApp(): Server {
  const store = new Store();
  const server = createServer((socket) => {
    socket.setNoDelay(true);
    socket.on('error', () => {
      // A reset peer is not a server failure; the socket is already gone.
    });
    let pending: Buffer = EMPTY_BODY;
    let current: RequestHead | null = null;
    let continueSent = false;

    socket.on('data', (chunk: Buffer) => {
      pending = pending.length === 0 ? chunk : Buffer.concat([pending, chunk]);
      for (;;) {
        if (current === null) {
          const parsed = parseHead(pending);
          if (parsed.status === 'need-more') break;
          if (parsed.status === 'bad') {
            // An oversized or unparseable head: answer 431 when the head itself is the
            // problem and the socket is still writable, then close either way.
            if (pending.length > MAX_HEAD_BYTES) {
              try {
                socket.end(serializeReply(jsonErrorReply(store, 431, 'Request Header Fields Too Large'), true, false));
              } catch {
                socket.destroy();
              }
            } else {
              socket.destroy();
            }
            return;
          }
          current = parsed.head;
          continueSent = false;
          pending = pending.subarray(parsed.head.consumed);
        }
        const head = current;
        if (head.expectsContinue && !continueSent) {
          // Only promise the body once the head is in: the client waits for this.
          try {
            socket.write(CONTINUE_100);
          } catch {
            socket.destroy();
            return;
          }
          continueSent = true;
        }
        const parsedBody = parseBody(pending, head);
        if (parsedBody.status === 'need-more') break;
        if (parsedBody.status === 'bad') {
          socket.destroy();
          return;
        }
        if (parsedBody.status === 'too-large') {
          try {
            socket.end(serializeReply(jsonErrorReply(store, 500, 'Internal Server Error'), true, false));
          } catch {
            socket.destroy();
          }
          return;
        }
        pending = pending.subarray(parsedBody.consumed);
        current = null;
        let reply: Reply;
        try {
          reply = dispatch(store, head, parsedBody.body);
        } catch (err) {
          process.stderr.write(`[forge] request error: ${err instanceof Error ? err.message : String(err)}\n`);
          reply = jsonErrorReply(store, 500, 'Internal Server Error');
        }
        const close = wantsClose(head);
        const headOnly = head.method.toUpperCase() === 'HEAD';
        try {
          socket.write(serializeReply(reply, close, headOnly));
        } catch {
          socket.destroy();
          return;
        }
        if (close) {
          socket.end();
          return;
        }
        if (pending.length === 0) break;
      }
      // Detach a consumed prefix so a long-lived keep-alive socket never pins a growing buffer.
      if (pending.length > 0 && pending.byteOffset !== 0) pending = Buffer.from(pending);
      else if (pending.length === 0) pending = EMPTY_BODY;
    });
  });
  return server;
}
