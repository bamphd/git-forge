// The one store every surface reads and writes: repositories, git objects and refs, issues,
// comments, labels and pull requests, all in memory. A trial is ephemeral and the suite is the
// only client, so nothing is persisted; in-memory maps are the fastest correct store, and every
// id, timestamp and ordering is deterministic (counters and one fixed trial clock), so a
// replayed trial answers exactly as the live one did.
//
// Issues and pull requests share one numbering sequence per repository, as on GitHub: a pull
// request is an item with a head and a base, an issue is an item without them.

import {
  buildCommit,
  buildTree,
  hashObject,
  isObjectId,
  parseCommit,
  parseTree,
  peelToCommit,
  type CommitView,
  type GitObject,
  type GitObjectType,
  type TreeEntry,
} from './objects.ts';

/** The login the trial's bearer token names: the scenario's viewer, fixed by the contest's seed. */
export const VIEWER = Object.freeze({
  id: 1000,
  login: 'adaiversen',
  name: 'ada iversen',
  email: 'adaiversen@example.invalid',
});

/** The scenario's day one, 2025-01-01T00:00:00Z, the fallback when the trial sets no clock. */
const FALLBACK_NOW_SEC = 1_735_689_600;

/** The trial's fixed clock: MAKEFUN_TRIAL_NOW when it parses, else the scenario's epoch. */
function trialNow(): { sec: number; iso: string } {
  const raw = (process.env.MAKEFUN_TRIAL_NOW ?? '').trim();
  if (/^\d+$/.test(raw)) {
    const sec = Number(raw);
    const normalized = sec > 1e12 ? Math.floor(sec / 1000) : sec;
    return { sec: normalized, iso: new Date(normalized * 1000).toISOString() };
  }
  if (raw !== '') {
    const parsed = Date.parse(raw);
    if (Number.isFinite(parsed)) {
      const sec = Math.floor(parsed / 1000);
      return { sec, iso: new Date(sec * 1000).toISOString() };
    }
  }
  return { sec: FALLBACK_NOW_SEC, iso: new Date(FALLBACK_NOW_SEC * 1000).toISOString() };
}

const NOW = trialNow();

/** Opaque GraphQL global ids: prefixed, never the bare database id or number. */
export function globalRepoId(repo: Repo): string {
  return `R_${repo.id}`;
}

export function globalItemId(item: Item): string {
  return `I_${item.id}`;
}

export function globalCommentId(comment: Comment): string {
  return `C_${comment.id}`;
}

/** Unix seconds every server-created commit carries. */
export function trialNowSec(): number {
  return NOW.sec;
}

/** The ISO timestamp every created/updated field carries. */
export function trialNowIso(): string {
  return NOW.iso;
}

/** The identity server-created commits carry when the request names none. */
export function viewerIdentity(): string {
  return `${VIEWER.name} <${VIEWER.email}>`;
}

export interface Label {
  readonly id: number;
  readonly name: string;
  readonly color: string;
  readonly description: string;
}

export interface Comment {
  readonly id: number;
  body: string;
  readonly createdAt: string;
  updatedAt: string;
}

export type ItemKind = 'issue' | 'pull';

export interface Item {
  readonly id: number;
  readonly kind: ItemKind;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: Label[];
  readonly comments: Comment[];
  readonly createdAt: string;
  updatedAt: string;
  closedAt: string | null;
  // Pull-request fields; null on an issue.
  readonly headRef: string | null;
  readonly baseRef: string | null;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
}

export interface Repo {
  readonly id: number;
  readonly owner: string;
  readonly name: string;
  readonly fullName: string;
  description: string;
  isPrivate: boolean;
  readonly defaultBranch: string;
  readonly createdAt: string;
  updatedAt: string;
  pushedAt: string;
  readonly objects: Map<string, GitObject>;
  readonly refs: Map<string, string>;
  readonly labels: Label[];
  readonly items: Item[];
  nextNumber: number;
  /** Bumped on every mutation that can change a cached issue serialization. */
  version: number;
}

export interface FileChange {
  readonly filename: string;
  readonly status: 'added' | 'modified' | 'removed';
  readonly additions: number;
  readonly deletions: number;
}

export class StoreError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = 'StoreError';
    this.status = status;
  }
}

export class Store {
  private readonly repos = new Map<string, Repo>();
  private repoSeq = 1;
  private itemSeq = 1;
  private commentSeq = 1;
  private labelSeq = 1;
  private requestSeq = 1;
  private readonly packCache = new Map<string, Buffer>();
  private readonly fetchCache = new Map<string, Buffer>();
  private readonly issueCache = new Map<string, Buffer>();
  private readonly repoById = new Map<number, Repo>();
  private readonly itemById = new Map<number, { repo: Repo; item: Item }>();
  private readonly commentById = new Map<number, { repo: Repo; item: Item; comment: Comment }>();

  /** Mark a repository changed, invalidating its cached issue serializations. */
  touch(repo: Repo): void {
    repo.version += 1;
    const prefix = `${repo.fullName}|issue|`;
    for (const key of this.issueCache.keys()) {
      if (key.startsWith(prefix)) this.issueCache.delete(key);
    }
  }

  /** A cached GET /issues/{n} body, or null when the repository changed since it was built. */
  cachedIssueBody(repo: Repo, item: Item): Buffer | null {
    return this.issueCache.get(`${repo.fullName}|issue|${item.number}|${repo.version}`) ?? null;
  }

  cacheIssueBody(repo: Repo, item: Item, body: Buffer): void {
    if (this.issueCache.size > 1024) this.issueCache.clear();
    this.issueCache.set(`${repo.fullName}|issue|${item.number}|${repo.version}`, body);
  }

  /** A fresh non-empty request id for a reply; a counter, so replays agree. */
  nextRequestId(): string {
    return `forge-${this.requestSeq++}`;
  }

  /** The repository a global id names, or undefined when the id is not a repository id. */
  repoByGlobalId(id: string): Repo | undefined {
    const match = /^R_(\d+)$/.exec(id);
    return match === null ? undefined : this.repoById.get(Number(match[1]));
  }

  /** The item a global id names, with its repository, or undefined. */
  itemByGlobalId(id: string): { repo: Repo; item: Item } | undefined {
    const match = /^I_(\d+)$/.exec(id);
    return match === null ? undefined : this.itemById.get(Number(match[1]));
  }

  /** The repository an item belongs to, via the global item registry. */
  repoOfItem(item: Item): Repo | undefined {
    return this.itemById.get(item.id)?.repo;
  }

  /** The object a Relay `node(id:)` global id names, whatever its kind. */
  nodeByGlobalId(
    id: string,
  ): { kind: 'repo'; repo: Repo } | { kind: 'item'; repo: Repo; item: Item } | { kind: 'comment'; repo: Repo; item: Item; comment: Comment } | undefined {
    const repo = this.repoByGlobalId(id);
    if (repo !== undefined) return { kind: 'repo', repo };
    const item = this.itemByGlobalId(id);
    if (item !== undefined) return { kind: 'item', ...item };
    const commentMatch = /^C_(\d+)$/.exec(id);
    if (commentMatch !== null) {
      const comment = this.commentById.get(Number(commentMatch[1]));
      if (comment !== undefined) return { kind: 'comment', ...comment };
    }
    return undefined;
  }

  // --------------------------------------------------------------------- repositories

  createRepo(name: string, description: string, isPrivate: boolean): Repo {
    const key = `${VIEWER.login}/${name}`;
    if (this.repos.has(key)) throw new StoreError(422, `Repository ${name} already exists`);
    const repo: Repo = {
      id: this.repoSeq++,
      owner: VIEWER.login,
      name,
      fullName: key,
      description,
      isPrivate,
      defaultBranch: 'main',
      createdAt: NOW.iso,
      updatedAt: NOW.iso,
      pushedAt: NOW.iso,
      objects: new Map(),
      refs: new Map(),
      labels: [],
      items: [],
      nextNumber: 1,
      version: 0,
    };
    this.repoById.set(repo.id, repo);
    this.repos.set(key, repo);
    return repo;
  }

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(`${owner}/${name}`);
  }

  deleteRepo(repo: Repo): void {
    this.repos.delete(repo.fullName);
    this.repoById.delete(repo.id);
    for (const item of repo.items) {
      this.itemById.delete(item.id);
      for (const comment of item.comments) this.commentById.delete(comment.id);
    }
    this.invalidatePackCache(repo);
  }

  /** Every repository, in creation order. */
  listRepos(): Repo[] {
    return [...this.repos.values()].sort((a, b) => a.id - b.id);
  }

  // --------------------------------------------------------------------- git objects

  putObject(repo: Repo, type: GitObjectType, data: Uint8Array): string {
    const body = Buffer.isBuffer(data) ? data : Buffer.from(data);
    const oid = hashObject(type, body);
    if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data: body });
    return oid;
  }

  getObject(repo: Repo, oid: string): GitObject | undefined {
    return repo.objects.get(oid);
  }

  putObjects(repo: Repo, objects: Iterable<GitObject>): void {
    for (const object of objects) {
      const oid = hashObject(object.type, object.data);
      if (!repo.objects.has(oid)) repo.objects.set(oid, object);
    }
    this.invalidatePackCache(repo);
  }

  getRef(repo: Repo, refname: string): string | undefined {
    return repo.refs.get(refname);
  }

  setRef(repo: Repo, refname: string, oid: string): void {
    repo.refs.set(refname, oid);
    repo.pushedAt = NOW.iso;
    this.invalidatePackCache(repo);
  }

  deleteRef(repo: Repo, refname: string): boolean {
    const deleted = repo.refs.delete(refname);
    if (deleted) this.invalidatePackCache(repo);
    return deleted;
  }

  /** Every ref, sorted by name, for a deterministic advertisement. */
  listRefs(repo: Repo): Array<{ name: string; oid: string }> {
    return [...repo.refs.entries()].sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0)).map(([name, oid]) => ({ name, oid }));
  }

  /**
   * Resolve a ref spelling to a commit id: a full ref, a short branch or tag name, HEAD, or a
   * bare object id (peeled to its commit when it names a tag). Undefined when nothing matches.
   */
  resolveRef(repo: Repo, ref: string): string | undefined {
    const trimmed = ref.trim();
    if (trimmed === '') return undefined;
    if (trimmed === 'HEAD') {
      const tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
      return tip === undefined ? undefined : (peelToCommit(tip, (oid) => repo.objects.get(oid)) ?? undefined);
    }
    const candidates = [
      trimmed,
      `refs/${trimmed}`,
      `refs/heads/${trimmed}`,
      `refs/tags/${trimmed}`,
    ];
    for (const candidate of candidates) {
      const oid = repo.refs.get(candidate);
      if (oid !== undefined) return peelToCommit(oid, (oid2) => repo.objects.get(oid2)) ?? undefined;
    }
    if (isObjectId(trimmed)) return peelToCommit(trimmed, (oid) => repo.objects.get(oid)) ?? undefined;
    return undefined;
  }

  /** Resolve a ref spelling to the object id it names, without peeling tags. */
  resolveOid(repo: Repo, ref: string): string | undefined {
    const trimmed = ref.trim();
    if (trimmed === 'HEAD') return repo.refs.get(`refs/heads/${repo.defaultBranch}`);
    const candidates = [trimmed, `refs/${trimmed}`, `refs/heads/${trimmed}`, `refs/tags/${trimmed}`];
    for (const candidate of candidates) {
      const oid = repo.refs.get(candidate);
      if (oid !== undefined) return oid;
    }
    if (isObjectId(trimmed) && repo.objects.has(trimmed)) return trimmed;
    return undefined;
  }

  commitView(repo: Repo, oid: string): CommitView | undefined {
    const object = repo.objects.get(oid);
    if (!object || object.type !== 'commit') return undefined;
    try {
      return parseCommit(object.data);
    } catch {
      return undefined;
    }
  }

  /** First-parent history from a tip, newest first, cycle-safe. */
  commitHistory(repo: Repo, tip: string, limit = 1000): string[] {
    const out: string[] = [];
    const seen = new Set<string>();
    let current: string | undefined = tip;
    while (current !== undefined && out.length < limit && !seen.has(current)) {
      seen.add(current);
      out.push(current);
      current = this.commitView(repo, current)?.parents[0];
    }
    return out;
  }

  /** True when `ancestor` is reachable from `descendant` (or the two are equal). */
  isAncestor(repo: Repo, ancestor: string, descendant: string): boolean {
    if (ancestor === descendant) return true;
    const seen = new Set<string>();
    const queue = [descendant];
    while (queue.length > 0) {
      const oid = queue.pop() as string;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const view = this.commitView(repo, oid);
      if (!view) continue;
      for (const parent of view.parents) {
        if (parent === ancestor) return true;
        queue.push(parent);
      }
    }
    return false;
  }

  /** A common ancestor of two tips, found by breadth-first search from the second. */
  mergeBase(repo: Repo, a: string, b: string): string | undefined {
    const ancestorsA = new Set<string>();
    const queueA = [a];
    while (queueA.length > 0) {
      const oid = queueA.shift() as string;
      if (ancestorsA.has(oid)) continue;
      ancestorsA.add(oid);
      const view = this.commitView(repo, oid);
      if (view) queueA.push(...view.parents);
    }
    const seen = new Set<string>();
    const queueB = [b];
    while (queueB.length > 0) {
      const oid = queueB.shift() as string;
      if (seen.has(oid)) continue;
      seen.add(oid);
      if (ancestorsA.has(oid)) return oid;
      const view = this.commitView(repo, oid);
      if (view) queueB.push(...view.parents);
    }
    return undefined;
  }

  createCommit(
    repo: Repo,
    fields: { tree: string; parents: readonly string[]; message: string; author: string; committer: string },
  ): string {
    const data = buildCommit({ ...fields, timestamp: NOW.sec });
    return this.putObject(repo, 'commit', data);
  }

  // --------------------------------------------------------------------- trees

  treeEntries(repo: Repo, treeOid: string): TreeEntry[] {
    const object = repo.objects.get(treeOid);
    if (!object || object.type !== 'tree') return [];
    try {
      return parseTree(object.data);
    } catch {
      return [];
    }
  }

  /**
   * Walk a path from a commit: the blob or tree it names, or undefined. A trailing slash names
   * the directory itself; an empty path names the root tree.
   */
  readPath(
    repo: Repo,
    commitOid: string,
    path: string,
  ): { type: 'blob' | 'tree'; oid: string; mode: string; name: string } | undefined {
    const view = this.commitView(repo, commitOid);
    if (!view) return undefined;
    const segments = path.split('/').filter((segment) => segment !== '');
    let treeOid = view.tree;
    if (segments.length === 0) return { type: 'tree', oid: treeOid, mode: '40000', name: '' };
    for (let i = 0; i < segments.length; i += 1) {
      const segment = segments[i] as string;
      const entry = this.treeEntries(repo, treeOid).find((candidate) => candidate.name === segment);
      if (!entry) return undefined;
      const last = i === segments.length - 1;
      if (entry.mode === '40000') {
        if (last) return { type: 'tree', oid: entry.oid, mode: entry.mode, name: entry.name };
        treeOid = entry.oid;
      } else {
        if (!last) return undefined;
        return { type: 'blob', oid: entry.oid, mode: entry.mode, name: entry.name };
      }
    }
    return undefined;
  }

  /** Every file in a commit, path -> blob, in sorted-path order. */
  flatTree(repo: Repo, commitOid: string): Map<string, { oid: string; mode: string }> {
    const out = new Map<string, { oid: string; mode: string }>();
    const view = this.commitView(repo, commitOid);
    if (!view) return out;
    const walk = (treeOid: string, prefix: string): void => {
      for (const entry of this.treeEntries(repo, treeOid)) {
        const path = prefix === '' ? entry.name : `${prefix}/${entry.name}`;
        if (entry.mode === '40000') walk(entry.oid, path);
        else if (entry.mode !== '160000') out.set(path, { oid: entry.oid, mode: entry.mode });
      }
    };
    walk(view.tree, '');
    return new Map([...out.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1)));
  }

  /** Build (and store) the nested trees for a flat file map; returns the root tree id. */
  buildTreeFromFlat(repo: Repo, files: Map<string, { oid: string; mode: string }>): string {
    interface DirNode {
      readonly files: Array<{ name: string; oid: string; mode: string }>;
      readonly dirs: Map<string, DirNode>;
    }
    const root: DirNode = { files: [], dirs: new Map() };
    for (const [path, file] of files) {
      const segments = path.split('/').filter((segment) => segment !== '');
      let node = root;
      for (let i = 0; i < segments.length - 1; i += 1) {
        const segment = segments[i] as string;
        let child = node.dirs.get(segment);
        if (!child) {
          child = { files: [], dirs: new Map() };
          node.dirs.set(segment, child);
        }
        node = child;
      }
      node.files.push({ name: segments[segments.length - 1] as string, oid: file.oid, mode: file.mode });
    }
    const build = (node: DirNode): string => {
      const entries: TreeEntry[] = [...node.files];
      for (const [name, child] of [...node.dirs.entries()].sort((a, b) => (a[0] < b[0] ? -1 : 1))) {
        entries.push({ mode: '40000', name, oid: build(child) });
      }
      return this.putObject(repo, 'tree', buildTree(entries));
    };
    return build(root);
  }

  // --------------------------------------------------------------------- labels/issues/pulls

  createLabel(repo: Repo, name: string, color: string, description = ''): Label {
    if (repo.labels.some((label) => label.name === name)) {
      throw new StoreError(422, `Label ${name} already exists`);
    }
    const label: Label = { id: this.labelSeq++, name, color, description };
    repo.labels.push(label);
    this.touch(repo);
    return label;
  }

  getLabel(repo: Repo, name: string): Label | undefined {
    return repo.labels.find((label) => label.name === name);
  }

  createItem(
    repo: Repo,
    kind: ItemKind,
    fields: { title: string; body: string; labelNames: readonly string[]; headRef?: string; baseRef?: string },
  ): Item {
    const labels: Label[] = [];
    for (const name of fields.labelNames ?? []) {
      const label = this.getLabel(repo, name);
      if (!label) throw new StoreError(422, `Label ${name} does not exist`);
      labels.push(label);
    }
    const item: Item = {
      id: this.itemSeq++,
      kind,
      number: repo.nextNumber++,
      title: fields.title,
      body: fields.body,
      state: 'open',
      labels,
      comments: [],
      createdAt: NOW.iso,
      updatedAt: NOW.iso,
      closedAt: null,
      headRef: fields.headRef ?? null,
      baseRef: fields.baseRef ?? null,
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
    };
    repo.items.push(item);
    this.itemById.set(item.id, { repo, item });
    this.touch(repo);
    return item;
  }

  getItem(repo: Repo, number: number): Item | undefined {
    return repo.items.find((item) => item.number === number);
  }

  listItems(repo: Repo): Item[] {
    return [...repo.items].sort((a, b) => a.number - b.number);
  }

  addComment(repo: Repo, item: Item, body: string): Comment {
    void repo;
    const comment: Comment = { id: this.commentSeq++, body, createdAt: NOW.iso, updatedAt: NOW.iso };
    item.comments.push(comment);
    this.commentById.set(comment.id, { repo, item, comment });
    item.updatedAt = NOW.iso;
    this.touch(repo);
    return comment;
  }

  // --------------------------------------------------------------------- merge + diff

  /**
   * The three-way merge of `head` onto `base`: the merged file map, or the conflicting paths.
   * A path conflicts when both sides changed it from the merge base to different contents.
   */
  threeWayMerge(
    repo: Repo,
    baseTip: string,
    headTip: string,
    mergeBaseOid: string | undefined,
  ): { merged: Map<string, { oid: string; mode: string }> } | { conflicts: string[] } {
    const baseFiles = this.flatTree(repo, baseTip);
    const headFiles = this.flatTree(repo, headTip);
    const ancestorFiles = mergeBaseOid === undefined ? new Map<string, { oid: string; mode: string }>() : this.flatTree(repo, mergeBaseOid);
    const paths = new Set([...baseFiles.keys(), ...headFiles.keys(), ...ancestorFiles.keys()]);
    const conflicts: string[] = [];
    const merged = new Map<string, { oid: string; mode: string }>();
    for (const path of paths) {
      const base = baseFiles.get(path);
      const head = headFiles.get(path);
      const ancestor = ancestorFiles.get(path);
      const baseChanged = (base?.oid ?? null) !== (ancestor?.oid ?? null);
      const headChanged = (head?.oid ?? null) !== (ancestor?.oid ?? null);
      if (baseChanged && headChanged && (base?.oid ?? null) !== (head?.oid ?? null)) {
        conflicts.push(path);
        continue;
      }
      const winner = headChanged ? head : base;
      if (winner) merged.set(path, winner);
    }
    if (conflicts.length > 0) return { conflicts: conflicts.sort() };
    return { merged };
  }

  /** True when the pull request would merge cleanly right now. */
  isMergeable(repo: Repo, item: Item): boolean {
    if (item.kind !== 'pull' || item.state !== 'open' || !item.headRef || !item.baseRef) return false;
    const baseTip = this.resolveRef(repo, item.baseRef);
    const headTip = this.resolveRef(repo, item.headRef);
    if (baseTip === undefined || headTip === undefined) return false;
    if (baseTip === headTip) return true;
    const base = this.mergeBase(repo, baseTip, headTip);
    return !('conflicts' in this.threeWayMerge(repo, baseTip, headTip, base));
  }

  /**
   * Squash-merge a pull request: one new commit on the base branch carrying the merged tree.
   * Returns the merge commit's sha, or null when the merge conflicts (nothing changes).
   */
  mergePullRequest(repo: Repo, item: Item, message: string): string | null {
    if (item.kind !== 'pull' || item.state !== 'open' || !item.headRef || !item.baseRef) return null;
    const baseTip = this.resolveRef(repo, item.baseRef);
    const headTip = this.resolveRef(repo, item.headRef);
    if (baseTip === undefined || headTip === undefined) return null;
    if (baseTip === headTip || this.isAncestor(repo, headTip, baseTip)) {
      item.state = 'closed';
      item.merged = true;
      item.mergedAt = NOW.iso;
      item.closedAt = NOW.iso;
      item.mergeCommitSha = baseTip;
      item.updatedAt = NOW.iso;
      this.touch(repo);
      return baseTip;
    }
    const base = this.mergeBase(repo, baseTip, headTip);
    const merged = this.threeWayMerge(repo, baseTip, headTip, base);
    if ('conflicts' in merged) return null;
    const tree = this.buildTreeFromFlat(repo, merged.merged);
    const sha = this.createCommit(repo, {
      tree,
      parents: [baseTip],
      message,
      author: viewerIdentity(),
      committer: viewerIdentity(),
    });
    this.setRef(repo, `refs/heads/${item.baseRef}`, sha);
    item.state = 'closed';
    item.merged = true;
    item.mergedAt = NOW.iso;
    item.closedAt = NOW.iso;
    item.mergeCommitSha = sha;
    item.updatedAt = NOW.iso;
    this.touch(repo);
    return sha;
  }

  /** The file changes between two commits, with LCS line counts. */
  diffFiles(repo: Repo, baseOid: string | undefined, headOid: string): FileChange[] {
    const baseFiles = baseOid === undefined ? new Map<string, { oid: string; mode: string }>() : this.flatTree(repo, baseOid);
    const headFiles = this.flatTree(repo, headOid);
    const paths = new Set([...baseFiles.keys(), ...headFiles.keys()]);
    const changes: FileChange[] = [];
    for (const path of [...paths].sort()) {
      const base = baseFiles.get(path);
      const head = headFiles.get(path);
      if (base && head && base.oid === head.oid) continue;
      const baseText = base ? (this.getObject(repo, base.oid)?.data.toString('utf8') ?? '') : null;
      const headText = head ? (this.getObject(repo, head.oid)?.data.toString('utf8') ?? '') : null;
      if (base === undefined && headText !== null) {
        changes.push({ filename: path, status: 'added', additions: countLines(headText), deletions: 0 });
      } else if (head === undefined && baseText !== null) {
        changes.push({ filename: path, status: 'removed', additions: 0, deletions: countLines(baseText) });
      } else if (baseText !== null && headText !== null) {
        const { additions, deletions } = diffLines(baseText, headText);
        changes.push({ filename: path, status: 'modified', additions, deletions });
      }
    }
    return changes;
  }

  // --------------------------------------------------------------------- pack cache

  cachedPack(key: string): Buffer | undefined {
    return this.packCache.get(key);
  }

  cachePack(key: string, pack: Buffer): void {
    if (this.packCache.size > 64) this.packCache.clear();
    this.packCache.set(key, pack);
  }

  /** A cached whole fetch response (acknowledgments + shallow-info + packfile sections). */
  cachedFetch(key: string): Buffer | undefined {
    return this.fetchCache.get(key);
  }

  cacheFetch(key: string, response: Buffer): void {
    if (this.fetchCache.size > 64) this.fetchCache.clear();
    this.fetchCache.set(key, response);
  }

  invalidatePackCache(repo: Repo): void {
    const prefix = `${repo.fullName}|`;
    for (const key of this.packCache.keys()) {
      if (key.startsWith(prefix)) this.packCache.delete(key);
    }
    for (const key of this.fetchCache.keys()) {
      if (key.startsWith(prefix)) this.fetchCache.delete(key);
    }
  }
}

function countLines(text: string): number {
  if (text === '') return 0;
  const lines = text.split('\n');
  return text.endsWith('\n') ? lines.length - 1 : lines.length;
}

/** LCS line diff: added and removed line counts between two texts. */
function diffLines(before: string, after: string): { additions: number; deletions: number } {
  const a = before === '' ? [] : before.split('\n');
  const b = after === '' ? [] : after.split('\n');
  if (before.endsWith('\n')) a.pop();
  if (after.endsWith('\n')) b.pop();
  const m = a.length;
  const n = b.length;
  let prev = new Array<number>(n + 1).fill(0);
  let curr = new Array<number>(n + 1).fill(0);
  for (let i = 1; i <= m; i += 1) {
    for (let j = 1; j <= n; j += 1) {
      curr[j] = a[i - 1] === b[j - 1] ? (prev[j - 1] as number) + 1 : Math.max(prev[j] as number, curr[j - 1] as number);
    }
    const swap = prev;
    prev = curr;
    curr = swap;
  }
  const lcs = prev[n] as number;
  return { additions: n - lcs, deletions: m - lcs };
}
