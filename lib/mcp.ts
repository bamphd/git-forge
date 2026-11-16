// The MCP surface: JSON-RPC 2.0 over Streamable HTTP at POST /mcp, answering plain JSON (never
// SSE). Both protocol eras the suite accepts are served unconditionally: the modern 2026-07-28
// `server/discover` (identity, capabilities and supported versions in one answer, no handshake,
// no session) and the legacy `initialize` + `notifications/initialized` handshake. Every request
// is stateless and answered from the one store, so MCP, REST, git and GraphQL always agree.
//
// Wire rules, as the suite asserts them: `tools/list` answers the same six tools byte-for-byte on
// every call (a single static list, one page, no cursor); an unknown tool is JSON-RPC -32602; a
// body that is not JSON is HTTP 400 with JSON-RPC -32700; a tools/call missing required arguments
// is JSON-RPC -32602; notifications (no id) are accepted with HTTP 202 and no body. Tool results
// carry both a JSON text block and structuredContent holding the same REST-shaped object, so the
// suite can read them either way. Authentication is the app layer's job.

import { Store, StoreError } from './store.ts';
import { issueObject, pullObject, repoObject } from './rest.ts';

export interface McpReply {
  readonly status: number;
  readonly headers: Record<string, string>;
  readonly body: Buffer | null;
}

const SERVER_NAME = 'git-forge';
const SERVER_VERSION = '1.0.0';
const SUPPORTED_VERSIONS = ['2026-07-28', '2025-11-25'];

const JSON_RPC_PARSE_ERROR = -32700;
const JSON_RPC_INVALID_REQUEST = -32600;
const JSON_RPC_METHOD_NOT_FOUND = -32601;
const JSON_RPC_INVALID_PARAMS = -32602;

// --------------------------------------------------------------------------- tools

interface ToolDefinition {
  readonly name: string;
  readonly description: string;
  readonly properties: Record<string, Record<string, unknown>>;
  readonly required: string[];
}

const TOOLS: readonly ToolDefinition[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    properties: {
      name: { type: 'string', description: 'The repository name.' },
      description: { type: 'string', description: 'The repository description.' },
      private: { type: 'boolean', description: 'Whether the repository is private.' },
    },
    required: ['name'],
  },
  {
    name: 'get_repo',
    description: 'Read one repository by owner and name.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
    },
    required: ['owner', 'repo'],
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      title: { type: 'string', description: 'The issue title.' },
      body: { type: 'string', description: 'The issue body.' },
    },
    required: ['owner', 'repo', 'title'],
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      state: { type: 'string', enum: ['open', 'closed', 'all'], description: 'Filter by issue state.' },
    },
    required: ['owner', 'repo'],
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch into a base branch.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      title: { type: 'string', description: 'The pull request title.' },
      body: { type: 'string', description: 'The pull request body.' },
      head: { type: 'string', description: 'The head branch name.' },
      base: { type: 'string', description: 'The base branch name.' },
    },
    required: ['owner', 'repo', 'title', 'head', 'base'],
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge a pull request into its base branch.',
    properties: {
      owner: { type: 'string', description: 'The repository owner login.' },
      repo: { type: 'string', description: 'The repository name.' },
      pull_number: { type: 'integer', description: 'The pull request number.' },
    },
    required: ['owner', 'repo', 'pull_number'],
  },
];

/** The one tools/list payload, serialized identically on every call. */
const TOOLS_LIST_RESULT = {
  tools: TOOLS.map((tool) => ({
    name: tool.name,
    description: tool.description,
    inputSchema: { type: 'object', properties: tool.properties, required: tool.required, additionalProperties: false },
  })),
};

// --------------------------------------------------------------------------- replies

function jsonReply(store: Store, status: number, value: unknown): McpReply {
  return {
    status,
    headers: { 'content-type': 'application/json; charset=utf-8', 'x-github-request-id': store.nextRequestId() },
    body: Buffer.from(`${JSON.stringify(value)}\n`, 'utf8'),
  };
}

function rpcResult(store: Store, id: unknown, result: unknown): McpReply {
  return jsonReply(store, 200, { jsonrpc: '2.0', id, result });
}

function rpcError(store: Store, id: unknown, code: number, message: string, status = 200): McpReply {
  return jsonReply(store, status, { jsonrpc: '2.0', id, error: { code, message } });
}

function toolResult(payload: unknown): Record<string, unknown> {
  return { content: [{ type: 'text', text: JSON.stringify(payload) }], structuredContent: payload, isError: false };
}

function toolError(message: string): Record<string, unknown> {
  return { content: [{ type: 'text', text: message }], isError: true };
}

// --------------------------------------------------------------------------- arguments

class InvalidParams extends Error {}

function asRecord(value: unknown): Record<string, unknown> | null {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? (value as Record<string, unknown>) : null;
}

function requiredString(args: Record<string, unknown>, field: string): string {
  const value = args[field];
  if (typeof value !== 'string' || value === '') throw new InvalidParams(`Missing required argument: ${field}`);
  return value;
}

function optionalString(args: Record<string, unknown>, field: string, fallback: string): string {
  const value = args[field];
  return typeof value === 'string' ? value : fallback;
}

function optionalBoolean(args: Record<string, unknown>, field: string, fallback: boolean): boolean {
  const value = args[field];
  return typeof value === 'boolean' ? value : fallback;
}

function requiredInt(args: Record<string, unknown>, field: string): number {
  const value = args[field];
  if (typeof value === 'number' && Number.isSafeInteger(value)) return value;
  if (typeof value === 'string' && /^\d+$/.test(value)) return Number.parseInt(value, 10);
  throw new InvalidParams(`Missing required argument: ${field}`);
}

/** Owner + repo from the arguments, accepting a `full_name` ("owner/repo") fallback. */
function repoRef(args: Record<string, unknown>): { owner: string; name: string } {
  const fullName = args.full_name;
  if ((args.owner === undefined || args.repo === undefined) && typeof fullName === 'string' && fullName.includes('/')) {
    const [owner, name] = fullName.split('/', 2);
    if (owner !== '' && name !== '') return { owner: owner as string, name: name as string };
  }
  return { owner: requiredString(args, 'owner'), name: requiredString(args, 'repo') };
}

// --------------------------------------------------------------------------- tools/call

function callTool(store: Store, name: string, args: Record<string, unknown>): Record<string, unknown> {
  switch (name) {
    case 'create_repo': {
      const repoName = requiredString(args, 'name');
      try {
        const repo = store.createRepo(repoName, optionalString(args, 'description', ''), optionalBoolean(args, 'private', false));
        return toolResult(repoObject(store, repo));
      } catch (err) {
        if (err instanceof StoreError) return toolError(err.message);
        throw err;
      }
    }
    case 'get_repo': {
      const { owner, name } = repoRef(args);
      const repo = store.getRepo(owner, name);
      if (repo === undefined) return toolError(`Repository ${owner}/${name} not found`);
      return toolResult(repoObject(store, repo));
    }
    case 'create_issue': {
      const { owner, name } = repoRef(args);
      const repo = store.getRepo(owner, name);
      if (repo === undefined) return toolError(`Repository ${owner}/${name} not found`);
      const title = requiredString(args, 'title');
      const item = store.createItem(repo, 'issue', { title, body: optionalString(args, 'body', '') });
      return toolResult(issueObject(store, repo, item));
    }
    case 'list_issues': {
      const { owner, name } = repoRef(args);
      const repo = store.getRepo(owner, name);
      if (repo === undefined) return toolError(`Repository ${owner}/${name} not found`);
      const state = optionalString(args, 'state', 'open');
      let items = store.listItems(repo);
      if (state !== 'all') items = items.filter((item) => item.state === state);
      items = [...items].sort((a, b) => b.number - a.number);
      return toolResult(items.map((item) => issueObject(store, repo, item)));
    }
    case 'create_pull_request': {
      const { owner, name } = repoRef(args);
      const repo = store.getRepo(owner, name);
      if (repo === undefined) return toolError(`Repository ${owner}/${name} not found`);
      const title = requiredString(args, 'title');
      const head = requiredString(args, 'head');
      const base = requiredString(args, 'base');
      if (store.resolveRef(repo, head) === undefined) return toolError(`Head branch ${head} does not exist`);
      if (store.resolveRef(repo, base) === undefined) return toolError(`Base branch ${base} does not exist`);
      const item = store.createItem(repo, 'pull', { title, body: optionalString(args, 'body', ''), headRef: head, baseRef: base });
      return toolResult(pullObject(store, repo, item));
    }
    case 'merge_pull_request': {
      const { owner, name } = repoRef(args);
      const repo = store.getRepo(owner, name);
      if (repo === undefined) return toolError(`Repository ${owner}/${name} not found`);
      const number = requiredInt(args, 'pull_number');
      const item = store.getItem(repo, number);
      if (item === undefined || item.kind !== 'pull') return toolError(`Pull request #${number} not found`);
      if (item.state !== 'open') return toolError(`Pull request #${number} is already closed`);
      const sha = store.mergePullRequest(repo, item, `${item.title} (#${item.number})`);
      if (sha === null) return toolError(`Pull request #${number} cannot be merged due to conflicts`);
      return toolResult({ sha, merged: true, message: 'Pull request successfully merged' });
    }
    default:
      throw new InvalidParams(`Unknown tool: ${name}`);
  }
}

// --------------------------------------------------------------------------- entry point

/**
 * Handle one POST /mcp body, given as raw text so a body that is not JSON can answer the
 * MCP parse error (HTTP 400, JSON-RPC -32700) instead of the generic JSON 400.
 */
export function handleMcp(store: Store, rawBody: string): McpReply {
  let parsed: unknown;
  try {
    parsed = JSON.parse(rawBody) as unknown;
  } catch {
    return rpcError(store, null, JSON_RPC_PARSE_ERROR, 'Parse error', 400);
  }
  const request = asRecord(parsed);
  if (request === null) {
    return rpcError(store, null, JSON_RPC_INVALID_REQUEST, 'Invalid Request');
  }
  const id = request.id === undefined ? undefined : request.id;
  const method = request.method;
  const isNotification = id === undefined;

  // Notifications (no id) are accepted silently: 202, no body.
  if (isNotification) {
    return { status: 202, headers: { 'x-github-request-id': store.nextRequestId() }, body: null };
  }
  if (typeof method !== 'string' || method === '') {
    return rpcError(store, id, JSON_RPC_INVALID_REQUEST, 'Invalid Request');
  }

  if (method === 'server/discover') {
    return rpcResult(store, id, {
      supportedVersions: [...SUPPORTED_VERSIONS],
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
      _meta: { 'io.modelcontextprotocol/serverInfo': { name: SERVER_NAME, version: SERVER_VERSION } },
    });
  }
  if (method === 'initialize') {
    const params = asRecord(request.params) ?? {};
    const offered = typeof params.protocolVersion === 'string' ? params.protocolVersion : null;
    const chosen = offered !== null && SUPPORTED_VERSIONS.includes(offered) ? offered : '2025-11-25';
    return rpcResult(store, id, {
      protocolVersion: chosen,
      capabilities: { tools: {} },
      serverInfo: { name: SERVER_NAME, version: SERVER_VERSION },
    });
  }
  if (method === 'ping') {
    return rpcResult(store, id, {});
  }
  if (method === 'tools/list') {
    return rpcResult(store, id, TOOLS_LIST_RESULT);
  }
  if (method === 'tools/call') {
    const params = asRecord(request.params);
    if (params === null || typeof params.name !== 'string') {
      return rpcError(store, id, JSON_RPC_INVALID_PARAMS, 'tools/call requires a tool name');
    }
    const args = asRecord(params.arguments) ?? {};
    try {
      return rpcResult(store, id, callTool(store, params.name, args));
    } catch (err) {
      if (err instanceof InvalidParams) return rpcError(store, id, JSON_RPC_INVALID_PARAMS, err.message);
      throw err;
    }
  }
  return rpcError(store, id, JSON_RPC_METHOD_NOT_FOUND, `Method not found: ${method}`);
}
