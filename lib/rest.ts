// The REST v3 surface: repositories, branches, commits, contents, labels, issues, comments
// and pull requests. Every reply is application/json with a non-empty X-GitHub-Request-Id,
// every error carries a string `message`, and unknown routes are JSON 404s. All state lives in
// the one store; this module only translates HTTP into store calls and store objects into the
// GitHub shapes the suite asserts.

import { isObjectId, type GitObjectType } from './objects.ts';
import {
  Store,
  StoreError,
  VIEWER,
  trialNowIso,
  viewerIdentity,
  type Comment,
  type Item,
  type Label,
  type Repo,
} from './store.ts';

export interface RestRequest {
  readonly method: string;
  /** Path segments after `/api/v3`, percent-decoded, without empty segments. */
  readonly segments: string[];
  readonly query: URLSearchParams;
  /** The request's Host header, for absolute pagination links. */
  readonly host: string;
  /** The parsed JSON body, or null when the request carried no body. */
  readonly body: unknown;
}

export interface RestReply {
  readonly status: number;
  readonly headers: Record<string, string>;
  /** Serialized JSON bytes, or null for a bodyless reply (204). */
  readonly body: Buffer | null;
}

const JSON_TYPE = 'application/json; charset=utf-8';

/** The one viewer object: every field is a scenario constant, so one frozen copy serves all. */
const USER_OBJECT: Record<string, unknown> = Object.freeze({
  login: VIEWER.login,
  id: VIEWER.id,
  name: VIEWER.name,
  email: VIEWER.email,
  type: 'User',
  site_admin: false,
  created_at: trialNowIso(),
  updated_at: trialNowIso(),
});

/** Shared empty array for fields that are always empty (assignees, topics): stringify-only. */
const EMPTY_ARRAY: readonly unknown[] = Object.freeze([]);

function jsonReply(store: Store, status: number, value: unknown, extraHeaders: Record<string, string> = {}): RestReply {
  return {
    status,
    headers: { 'content-type': JSON_TYPE, 'x-github-request-id': store.nextRequestId(), ...extraHeaders },
    body: Buffer.from(`${JSON.stringify(value)}\n`, 'utf8'),
  };
}

function errorReply(store: Store, status: number, message: string): RestReply {
  return jsonReply(store, status, { message });
}

function notFound(store: Store): RestReply {
  return errorReply(store, 404, 'Not Found');
}

function userObject(): Record<string, unknown> {
  return USER_OBJECT;
}

function labelObject(label: Label): Record<string, unknown> {
  return { id: label.id, name: label.name, color: label.color, description: label.description, default: false };
}

export function repoObject(store: Store, repo: Repo): Record<string, unknown> {
  const openIssues = repo.items.filter((item) => item.state === 'open').length;
  return {
    id: repo.id,
    name: repo.name,
    full_name: repo.fullName,
    owner: userObject(),
    private: repo.isPrivate,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
    pushed_at: repo.pushedAt,
    has_issues: true,
    has_projects: true,
    has_wiki: true,
    has_pages: false,
    has_downloads: true,
    archived: false,
    disabled: false,
    visibility: repo.isPrivate ? 'private' : 'public',
    forks_count: 0,
    open_issues_count: openIssues,
    watchers_count: 0,
    size: 0,
    language: null,
    topics: EMPTY_ARRAY,
  };
}

function commentObject(comment: Comment): Record<string, unknown> {
  return {
    id: comment.id,
    body: comment.body,
    user: userObject(),
    created_at: comment.createdAt,
    updated_at: comment.updatedAt,
  };
}

export function issueObject(store: Store, repo: Repo, item: Item): Record<string, unknown> {
  const object: Record<string, unknown> = {
    id: item.id,
    number: item.number,
    title: item.title,
    body: item.body,
    state: item.state,
    labels: item.labels.map(labelObject),
    user: userObject(),
    assignees: EMPTY_ARRAY,
    milestone: null,
    locked: false,
    comments: item.comments.length,
    created_at: item.createdAt,
    updated_at: item.updatedAt,
    closed_at: item.closedAt,
  };
  if (item.kind === 'pull') {
    object.pull_request = { merged_at: item.mergedAt };
  }
  void store;
  void repo;
  return object;
}

export function pullObject(store: Store, repo: Repo, item: Item): Record<string, unknown> {
  const headSha = item.headRef === null ? null : store.resolveRef(repo, item.headRef);
  const baseSha = item.baseRef === null ? null : store.resolveRef(repo, item.baseRef);
  const object = issueObject(store, repo, item);
  object.head = {
    label: `${repo.owner}:${item.headRef ?? ''}`,
    ref: item.headRef ?? '',
    sha: headSha ?? '0'.repeat(40),
    repo: repoObject(store, repo),
    user: userObject(),
  };
  object.base = {
    label: `${repo.owner}:${item.baseRef ?? ''}`,
    ref: item.baseRef ?? '',
    sha: baseSha ?? '0'.repeat(40),
    repo: repoObject(store, repo),
    user: userObject(),
  };
  object.merged = item.merged;
  object.mergeable = store.isMergeable(repo, item);
  object.merge_commit_sha = item.mergeCommitSha;
  object.merged_at = item.mergedAt;
  object.draft = false;
  return object;
}

/** Parse a commit's `Name <email> ts tz` identity line into GitHub author fields. */
function identityFields(identity: string): { name: string; email: string; date: string } {
  const match = /^(.*?) <([^>]*)> (\d+) ([+-]\d{4})$/.exec(identity.trim());
  if (!match) return { name: identity, email: '', date: trialNowIso() };
  const epoch = Number(match[3]);
  return {
    name: match[1] as string,
    email: match[2] as string,
    date: new Date(epoch * 1000).toISOString(),
  };
}

function commitObject(store: Store, repo: Repo, sha: string): Record<string, unknown> | null {
  const view = store.commitView(repo, sha);
  if (!view) return null;
  const author = identityFields(view.author);
  const committer = identityFields(view.committer);
  return {
    sha,
    commit: {
      message: view.message,
      author,
      committer,
      tree: { sha: view.tree },
    },
    author: userObject(),
    committer: userObject(),
    parents: view.parents.map((parent) => ({ sha: parent })),
  };
}

function refObject(store: Store, repo: Repo, refname: string, oid: string): Record<string, unknown> {
  const object = store.getObject(repo, oid);
  const type: GitObjectType = object?.type ?? 'commit';
  return { ref: refname, object: { sha: oid, type } };
}

// --------------------------------------------------------------------------- pagination

interface Page<T> {
  readonly items: T[];
  readonly link: string | null;
}

function paginate<T>(items: readonly T[], query: URLSearchParams, host: string, path: string): Page<T> {
  const rawPerPage = Number.parseInt(query.get('per_page') ?? '', 10);
  const perPage = Number.isSafeInteger(rawPerPage) && rawPerPage >= 1 && rawPerPage <= 100 ? rawPerPage : 30;
  const rawPage = Number.parseInt(query.get('page') ?? '', 10);
  const page = Number.isSafeInteger(rawPage) && rawPage >= 1 ? rawPage : 1;
  const totalPages = Math.max(1, Math.ceil(items.length / perPage));
  const start = (page - 1) * perPage;
  const slice = items.slice(start, start + perPage);
  if (totalPages <= 1) return { items: slice, link: null };
  const build = (target: number): string => {
    const params = new URLSearchParams(query);
    params.set('page', String(target));
    params.set('per_page', String(perPage));
    return `http://${host}${path}?${params.toString()}`;
  };
  const links: string[] = [`<${build(1)}>; rel="first"`, `<${build(totalPages)}>; rel="last"`];
  if (page > 1) links.push(`<${build(page - 1)}>; rel="prev"`);
  if (page < totalPages) links.push(`<${build(page + 1)}>; rel="next"`);
  return { items: slice, link: links.join(', ') };
}

// --------------------------------------------------------------------------- body helpers

function bodyObject(body: unknown): Record<string, unknown> {
  if (body === null || body === undefined) return {};
  if (typeof body === 'object' && !Array.isArray(body)) return body as Record<string, unknown>;
  throw new StoreError(400, 'Problems parsing JSON');
}

function requiredString(body: Record<string, unknown>, field: string): string {
  const value = body[field];
  if (typeof value !== 'string' || value === '') throw new StoreError(422, `${field} is required`);
  return value;
}

function optionalString(body: Record<string, unknown>, field: string, fallback: string): string {
  const value = body[field];
  return typeof value === 'string' ? value : fallback;
}

function optionalBoolean(body: Record<string, unknown>, field: string, fallback: boolean): boolean {
  const value = body[field];
  return typeof value === 'boolean' ? value : fallback;
}

function stringArray(body: Record<string, unknown>, field: string): string[] {
  const value = body[field];
  if (value === undefined) return [];
  if (!Array.isArray(value) || value.some((entry) => typeof entry !== 'string')) {
    throw new StoreError(422, `${field} must be an array of strings`);
  }
  return [...(value as string[])];
}

// --------------------------------------------------------------------------- contents

/**
 * Apply a contents PUT/DELETE: build the new tree, commit it on the branch, move the branch.
 * Returns the new commit's sha and the blob's sha (null when the file was deleted).
 */
function applyContentChange(
  store: Store,
  repo: Repo,
  branch: string,
  path: string,
  message: string,
  change: { blob: Buffer } | { delete: true },
  identity: { author: string; committer: string },
): { commitSha: string; blobSha: string | null } {
  const refname = `refs/heads/${branch}`;
  const tip = store.getRef(repo, refname);
  const files = tip === undefined ? new Map<string, { oid: string; mode: string }>() : store.flatTree(repo, tip);
  let blobSha: string | null = null;
  if ('delete' in change) {
    files.delete(path);
  } else {
    blobSha = store.putObject(repo, 'blob', change.blob);
    files.set(path, { oid: blobSha, mode: '100644' });
  }
  const tree = store.buildTreeFromFlat(repo, files);
  const commitSha = store.createCommit(repo, {
    tree,
    parents: tip === undefined ? [] : [tip],
    message,
    author: identity.author,
    committer: identity.committer,
  });
  store.setRef(repo, refname, commitSha);
  repo.updatedAt = trialNowIso();
  return { commitSha, blobSha };
}

function identityFrom(body: Record<string, unknown>, field: string): string | undefined {
  const value = body[field];
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return undefined;
  const record = value as Record<string, unknown>;
  const name = record.name;
  const email = record.email;
  if (typeof name !== 'string' || typeof email !== 'string') return undefined;
  return `${name} <${email}>`;
}

// --------------------------------------------------------------------------- router

/**
 * Route one authenticated REST request. The app layer has already checked the bearer token and
 * parsed the JSON body (a malformed body never reaches here: it is a 400 there). StoreErrors
 * carry their HTTP status; anything else is a 500 the app layer turns into a JSON error.
 */
export function handleRest(store: Store, request: RestRequest): RestReply {
  const [first, second, third, ...rest] = request.segments;
  const method = request.method;

  try {
    // Root and meta.
    if (request.segments.length === 0) {
      if (method !== 'GET') return notFound(store);
      return jsonReply(store, 200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, { 'x-oauth-scopes': 'repo, read:org' });
    }
    if (first === 'meta' && request.segments.length === 1) {
      if (method !== 'GET') return notFound(store);
      return jsonReply(store, 200, { installed_version: '1.0.0' });
    }
    if (first === 'user' && request.segments.length === 1) {
      if (method !== 'GET') return notFound(store);
      return jsonReply(store, 200, userObject());
    }
    if (first === 'user' && second === 'repos' && request.segments.length === 2) {
      if (method === 'GET') {
        const page = paginate(store.listRepos(), request.query, request.host, '/api/v3/user/repos');
        return jsonReply(store, 200, page.items.map((repo) => repoObject(store, repo)), page.link === null ? {} : { link: page.link });
      }
      if (method === 'POST') {
        const body = bodyObject(request.body);
        const name = requiredString(body, 'name');
        const repo = store.createRepo(name, optionalString(body, 'description', ''), optionalBoolean(body, 'private', false));
        return jsonReply(store, 201, repoObject(store, repo));
      }
      return notFound(store);
    }

    // Repository-scoped routes.
    if (first === 'repos' && second !== undefined && third !== undefined) {
      const owner = second;
      const name = third;
      const repo = store.getRepo(owner, name);
      if (repo === undefined) return notFound(store);
      const tail = rest;

      if (tail.length === 0) {
        if (method === 'GET') return jsonReply(store, 200, repoObject(store, repo));
        if (method === 'PATCH') {
          const body = bodyObject(request.body);
          if (typeof body.description === 'string') repo.description = body.description;
          if (typeof body.private === 'boolean') repo.isPrivate = body.private;
          repo.updatedAt = trialNowIso();
          return jsonReply(store, 200, repoObject(store, repo));
        }
        if (method === 'DELETE') {
          store.deleteRepo(repo);
          return { status: 204, headers: { 'x-github-request-id': store.nextRequestId() }, body: null };
        }
        return notFound(store);
      }

      const [resource, ...resourceTail] = tail;

      if (resource === 'branches') {
        if (method !== 'GET') return notFound(store);
        if (resourceTail.length === 0) {
          const branches = store
            .listRefs(repo)
            .filter((ref) => ref.name.startsWith('refs/heads/'))
            .map((ref) => ({ name: ref.name.slice('refs/heads/'.length), commit: { sha: ref.oid }, protected: false }))
            .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
          return jsonReply(store, 200, branches);
        }
        const branch = resourceTail.join('/');
        const oid = store.getRef(repo, `refs/heads/${branch}`);
        if (oid === undefined) return notFound(store);
        return jsonReply(store, 200, { name: branch, commit: { sha: oid }, protected: false });
      }

      if (resource === 'commits') {
        if (method !== 'GET' || resourceTail.length !== 0) return notFound(store);
        const ref = request.query.get('sha') ?? repo.defaultBranch;
        const tip = store.resolveRef(repo, ref);
        if (tip === undefined) return notFound(store);
        const commits = store.commitHistory(repo, tip).map((sha) => commitObject(store, repo, sha)).filter((entry) => entry !== null);
        return jsonReply(store, 200, commits);
      }

      if (resource === 'git' && resourceTail[0] === 'refs') {
        const refTail = resourceTail.slice(1);
        if (method === 'POST' && refTail.length === 0) {
          const body = bodyObject(request.body);
          const ref = requiredString(body, 'ref');
          const sha = requiredString(body, 'sha');
          if (!ref.startsWith('refs/')) throw new StoreError(422, 'ref must start with refs/');
          if (!isObjectId(sha) || store.getObject(repo, sha) === undefined) throw new StoreError(422, 'object does not exist');
          if (store.getRef(repo, ref) !== undefined) throw new StoreError(422, 'Reference already exists');
          store.setRef(repo, ref, sha);
          return jsonReply(store, 201, refObject(store, repo, ref, sha));
        }
        if (method === 'GET') {
          if (refTail.length === 0) {
            return jsonReply(store, 200, store.listRefs(repo).map((ref) => refObject(store, repo, ref.name, ref.oid)));
          }
          const wanted = `refs/${refTail.join('/')}`;
          const exact = store.getRef(repo, wanted);
          if (exact !== undefined) return jsonReply(store, 200, refObject(store, repo, wanted, exact));
          const prefixed = store.listRefs(repo).filter((ref) => ref.name.startsWith(`${wanted}/`) || ref.name === wanted);
          if (prefixed.length === 0) return notFound(store);
          return jsonReply(store, 200, prefixed.map((ref) => refObject(store, repo, ref.name, ref.oid)));
        }
        return notFound(store);
      }

      if (resource === 'contents') {
        const path = resourceTail.join('/');
        if (method === 'GET') {
          const ref = request.query.get('ref') ?? repo.defaultBranch;
          const tip = store.resolveRef(repo, ref);
          if (tip === undefined) return notFound(store);
          const found = path === '' ? { type: 'tree' as const, oid: store.commitView(repo, tip)?.tree ?? '', mode: '40000', name: '' } : store.readPath(repo, tip, path);
          if (found === undefined) return notFound(store);
          if (found.type === 'tree') {
            const entries = store.treeEntries(repo, found.oid).map((entry) => {
              const entryPath = path === '' ? entry.name : `${path}/${entry.name}`;
              const size = entry.mode === '40000' ? 0 : (store.getObject(repo, entry.oid)?.data.length ?? 0);
              return { type: entry.mode === '40000' ? 'dir' : 'file', name: entry.name, path: entryPath, sha: entry.oid, size };
            });
            return jsonReply(store, 200, entries);
          }
          const blob = store.getObject(repo, found.oid);
          if (blob === undefined) return notFound(store);
          return jsonReply(store, 200, {
            type: 'file',
            encoding: 'base64',
            size: blob.data.length,
            name: found.name,
            path,
            content: blob.data.toString('base64'),
            sha: found.oid,
          });
        }
        if (method === 'PUT' || method === 'DELETE') {
          if (path === '') return notFound(store);
          const body = bodyObject(request.body);
          const message = requiredString(body, 'message');
          const branch = optionalString(body, 'branch', repo.defaultBranch);
          const sha = typeof body.sha === 'string' ? body.sha : undefined;
          const tip = store.resolveRef(repo, branch);
          const current = tip === undefined ? undefined : store.readPath(repo, tip, path);
          if (method === 'DELETE') {
            if (current === undefined || current.type !== 'blob') return notFound(store);
            if (sha !== undefined && sha !== current.oid) throw new StoreError(409, 'The content SHA is stale');
            const identity = identityFrom(body, 'committer') ?? viewerIdentity();
            const { commitSha } = applyContentChange(store, repo, branch, path, message, { delete: true }, { author: identityFrom(body, 'author') ?? identity, committer: identity });
            return jsonReply(store, 200, { commit: { sha: commitSha } });
          }
          const content = body.content;
          if (typeof content !== 'string') throw new StoreError(422, 'content is required');
          let blob: Buffer;
          try {
            blob = Buffer.from(content.replace(/\s+/g, ''), 'base64');
          } catch {
            throw new StoreError(422, 'content is not valid base64');
          }
          const exists = current !== undefined && current.type === 'blob';
          if (exists && sha !== current.oid) throw new StoreError(409, 'The content SHA is stale');
          const identity = identityFrom(body, 'committer') ?? viewerIdentity();
          const { commitSha, blobSha } = applyContentChange(
            store,
            repo,
            branch,
            path,
            message,
            { blob },
            { author: identityFrom(body, 'author') ?? identity, committer: identity },
          );
          return jsonReply(store, exists ? 200 : 201, {
            content: { name: path.split('/').pop() ?? path, path, sha: blobSha, size: blob.length, encoding: 'base64' },
            commit: { sha: commitSha },
          });
        }
        return notFound(store);
      }

      if (resource === 'labels') {
        if (resourceTail.length !== 0) return notFound(store);
        if (method === 'GET') return jsonReply(store, 200, repo.labels.map(labelObject));
        if (method === 'POST') {
          const body = bodyObject(request.body);
          const label = store.createLabel(repo, requiredString(body, 'name'), optionalString(body, 'color', 'ededed'), optionalString(body, 'description', ''));
          return jsonReply(store, 201, labelObject(label));
        }
        return notFound(store);
      }

      if (resource === 'issues') {
        if (resourceTail.length === 0) {
          if (method === 'GET') {
            const state = request.query.get('state') ?? 'open';
            const direction = request.query.get('direction') ?? 'desc';
            let items = store.listItems(repo);
            if (state !== 'all') items = items.filter((item) => item.state === state);
            items = [...items].sort((a, b) => (direction === 'asc' ? a.number - b.number : b.number - a.number));
            const page = paginate(items, request.query, request.host, `/api/v3/repos/${owner}/${name}/issues`);
            return jsonReply(store, 200, page.items.map((item) => issueObject(store, repo, item)), page.link === null ? {} : { link: page.link });
          }
          if (method === 'POST') {
            const body = bodyObject(request.body);
            const item = store.createItem(repo, 'issue', {
              title: requiredString(body, 'title'),
              body: optionalString(body, 'body', ''),
              labelNames: stringArray(body, 'labels'),
            });
            return jsonReply(store, 201, issueObject(store, repo, item));
          }
          return notFound(store);
        }
        const number = Number(resourceTail[0]);
        if (!Number.isSafeInteger(number) || number < 1) return notFound(store);
        const item = store.getItem(repo, number);
        if (item === undefined) return notFound(store);
        if (resourceTail.length === 1) {
          if (method === 'GET') {
            // The read workload hammers this route: serve a cached serialization when the
            // repository has not changed since it was built.
            const cached = store.cachedIssueBody(repo, item);
            if (cached !== null) {
              return {
                status: 200,
                headers: { 'content-type': JSON_TYPE, 'x-github-request-id': store.nextRequestId() },
                body: cached,
              };
            }
            const reply = jsonReply(store, 200, issueObject(store, repo, item));
            if (reply.body !== null) store.cacheIssueBody(repo, item, reply.body);
            return reply;
          }
          if (method === 'PATCH') {
            const body = bodyObject(request.body);
            if (typeof body.title === 'string') item.title = body.title;
            if (typeof body.body === 'string') item.body = body.body;
            if (body.state === 'open' || body.state === 'closed') {
              item.state = body.state;
              item.closedAt = body.state === 'closed' ? trialNowIso() : null;
            }
            if (body.labels !== undefined) {
              const labels: Label[] = [];
              for (const name of stringArray(body, 'labels')) {
                const label = store.getLabel(repo, name);
                if (!label) throw new StoreError(422, `Label ${name} does not exist`);
                labels.push(label);
              }
              item.labels = labels;
            }
            item.updatedAt = trialNowIso();
            store.touch(repo);
            return jsonReply(store, 200, issueObject(store, repo, item));
          }
          return notFound(store);
        }
        if (resourceTail[1] === 'comments' && resourceTail.length === 2) {
          if (method === 'GET') return jsonReply(store, 200, item.comments.map(commentObject));
          if (method === 'POST') {
            const body = bodyObject(request.body);
            const comment = store.addComment(repo, item, requiredString(body, 'body'));
            return jsonReply(store, 201, commentObject(comment));
          }
          return notFound(store);
        }
        return notFound(store);
      }

      if (resource === 'pulls') {
        if (resourceTail.length === 0) {
          if (method === 'GET') {
            const state = request.query.get('state') ?? 'open';
            let items = store.listItems(repo).filter((item) => item.kind === 'pull');
            if (state !== 'all') items = items.filter((item) => item.state === state);
            items = [...items].sort((a, b) => b.number - a.number);
            const page = paginate(items, request.query, request.host, `/api/v3/repos/${owner}/${name}/pulls`);
            return jsonReply(store, 200, page.items.map((item) => pullObject(store, repo, item)), page.link === null ? {} : { link: page.link });
          }
          if (method === 'POST') {
            const body = bodyObject(request.body);
            const head = requiredString(body, 'head');
            const base = requiredString(body, 'base');
            if (store.resolveRef(repo, head) === undefined) throw new StoreError(422, `Head branch ${head} does not exist`);
            if (store.resolveRef(repo, base) === undefined) throw new StoreError(422, `Base branch ${base} does not exist`);
            const item = store.createItem(repo, 'pull', {
              title: requiredString(body, 'title'),
              body: optionalString(body, 'body', ''),
              labelNames: stringArray(body, 'labels'),
              headRef: head,
              baseRef: base,
            });
            return jsonReply(store, 201, pullObject(store, repo, item));
          }
          return notFound(store);
        }
        const number = Number(resourceTail[0]);
        if (!Number.isSafeInteger(number) || number < 1) return notFound(store);
        const item = store.getItem(repo, number);
        if (item === undefined || item.kind !== 'pull') return notFound(store);
        if (resourceTail.length === 1) {
          if (method === 'GET') return jsonReply(store, 200, pullObject(store, repo, item));
          return notFound(store);
        }
        if (resourceTail[1] === 'files' && resourceTail.length === 2) {
          if (method !== 'GET') return notFound(store);
          const baseTip = store.resolveRef(repo, item.baseRef as string);
          const headTip = store.resolveRef(repo, item.headRef as string);
          if (baseTip === undefined || headTip === undefined) return notFound(store);
          const base = store.mergeBase(repo, baseTip, headTip);
          const files = store.diffFiles(repo, base, headTip);
          return jsonReply(
            store,
            200,
            files.map((file) => ({
              filename: file.filename,
              status: file.status,
              additions: file.additions,
              deletions: file.deletions,
              changes: file.additions + file.deletions,
            })),
          );
        }
        if (resourceTail[1] === 'merge' && resourceTail.length === 2) {
          if (method !== 'PUT') return notFound(store);
          if (item.state !== 'open') throw new StoreError(405, 'Pull request is already closed');
          const body = bodyObject(request.body);
          const title = optionalString(body, 'commit_title', `${item.title} (#${item.number})`);
          const extra = optionalString(body, 'commit_message', '');
          const message = extra === '' || extra === title ? title : `${title}\n\n${extra}`;
          const sha = store.mergePullRequest(repo, item, message);
          if (sha === null) throw new StoreError(405, 'Pull request cannot be merged due to conflicts');
          return jsonReply(store, 200, { sha, merged: true, message: 'Pull request successfully merged' });
        }
        return notFound(store);
      }

      return notFound(store);
    }

    return notFound(store);
  } catch (err) {
    if (err instanceof StoreError) return errorReply(store, err.status, err.message);
    throw err;
  }
}
