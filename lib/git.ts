// Git smart HTTP: the ref advertisements, protocol v2 ls-refs and fetch, protocol v0
// upload-pack, and receive-pack pushes. Every response is built from the store alone; no git
// binary, no filesystem, no clock. Packs are delta-free and cached per request shape, so the
// fifty concurrent shallow clones of the benchmark share one built pack.

import { collectReachable, isObjectId, parseCommit, parseTag, peelToCommit, ZERO_OID, type GitObject } from './objects.ts';
import {
  decodePktLines,
  encodePktLine,
  encodePktLines,
  packetsToLines,
  readPack,
  verifyPackChecksum,
  writePack,
} from './pack.ts';
import { Store, type Repo } from './store.ts';

export const AGENT = 'git-forge/1.0';

export interface GitResponse {
  readonly status: number;
  readonly contentType: string;
  readonly body: Buffer;
}

function gitResponse(contentType: string, body: Buffer): GitResponse {
  return { status: 200, contentType, body };
}

function notFound(): GitResponse {
  return { status: 404, contentType: 'application/json; charset=utf-8', body: Buffer.from('{"message":"Not Found"}\n', 'utf8') };
}

/** Peeled target of an annotated tag ref, or undefined when the ref names no tag. */
function peeledTarget(store: Store, repo: Repo, oid: string): string | undefined {
  const object = store.getObject(repo, oid);
  if (!object || object.type !== 'tag') return undefined;
  try {
    return parseTag(object.data).object;
  } catch {
    return undefined;
  }
}

// --------------------------------------------------------------------------- advertisements

/** The v0 ref advertisement: service preamble, refs with capabilities on the first line, flush. */
function advertisementV0(store: Store, repo: Repo, service: 'git-upload-pack' | 'git-receive-pack'): Buffer {
  const headRef = `refs/heads/${repo.defaultBranch}`;
  const caps =
    service === 'git-upload-pack'
      ? `multi_ack thin-pack side-band-64k ofs-delta shallow symref=HEAD:${headRef} agent=${AGENT}`
      : `report-status side-band-64k delete-refs ofs-delta agent=${AGENT}`;
  const refs = store.listRefs(repo);
  const items: Array<string | 'flush'> = [`# service=${service}\n`, 'flush'];
  if (refs.length === 0) {
    items.push(`${ZERO_OID} capabilities^{}\u0000${caps}\n`, 'flush');
    return encodePktLines(items);
  }
  const headTip = store.getRef(repo, headRef);
  const lines: string[] = [];
  if (headTip !== undefined) lines.push(`${headTip} HEAD\n`);
  for (const ref of refs) {
    lines.push(`${ref.oid} ${ref.name}\n`);
    const peeled = peeledTarget(store, repo, ref.oid);
    if (peeled !== undefined) lines.push(`${peeled} ${ref.name}^{}\n`);
  }
  const first = lines[0] as string;
  lines[0] = `${first.slice(0, -1)}\u0000${caps}\n`;
  for (const line of lines) items.push(line);
  items.push('flush');
  return encodePktLines(items);
}

/** The v2 capability advertisement: `version 2`, capability lines, flush. No service header. */
function advertisementV2(): Buffer {
  return encodePktLines([
    'version 2\n',
    'ls-refs=unborn\n',
    'fetch=shallow\n',
    'object-format=sha1\n',
    `agent=${AGENT}\n`,
    'flush',
  ]);
}

/**
 * GET /info/refs. Upload-pack answers v2 when the client asks with Git-Protocol: version=2 and
 * v0 otherwise; receive-pack always answers v0. A missing repository is 404 and the server
 * stays up.
 */
export function gitInfoRefs(
  store: Store,
  owner: string,
  name: string,
  service: 'git-upload-pack' | 'git-receive-pack',
  gitProtocolHeader: string | undefined,
): GitResponse {
  const repo = store.getRepo(owner, name);
  if (repo === undefined) return notFound();
  const wantsV2 = service === 'git-upload-pack' && gitProtocolHeader === 'version=2';
  const body = wantsV2 ? advertisementV2() : advertisementV0(store, repo, service);
  return gitResponse(`application/x-${service}-advertisement`, body);
}

// --------------------------------------------------------------------------- side-band

/** Wrap bytes in side-band-64k packets (band byte + chunk, pkt-lined), plus a trailing flush. */
function sidebandWrap(data: Buffer, band: 1 | 2 = 1): Buffer {
  const parts: Buffer[] = [];
  const CHUNK = 65515;
  for (let offset = 0; offset < data.length; offset += CHUNK) {
    const chunk = data.subarray(offset, Math.min(offset + CHUNK, data.length));
    parts.push(encodePktLine(Buffer.concat([Buffer.from([band]), chunk])));
  }
  if (data.length === 0) parts.push(encodePktLine(Buffer.from([band])));
  parts.push(Buffer.from('0000', 'ascii'));
  return Buffer.concat(parts);
}

// --------------------------------------------------------------------------- fetch planning

export interface FetchPlan {
  /** Objects to pack, sorted by oid for deterministic bytes. */
  readonly objects: GitObject[];
  /** Boundary commits whose parents were cut off by the depth limit. */
  readonly shallow: string[];
  /** Client shallow boundaries this fetch completes. */
  readonly unshallow: string[];
  /** Haves that are ancestors of a want. */
  readonly acks: string[];
}

/** Commit generations from the wants (want = 1, parents increment), following all parents. */
function generations(
  store: Store,
  repo: Repo,
  wants: readonly string[],
): Map<string, number> {
  const gen = new Map<string, number>();
  const queue: string[] = [];
  for (const want of wants) {
    const peeled = peelToCommit(want, (oid) => store.getObject(repo, oid));
    if (peeled !== undefined && !gen.has(peeled)) {
      gen.set(peeled, 1);
      queue.push(peeled);
    }
  }
  while (queue.length > 0) {
    const oid = queue.shift() as string;
    const depth = gen.get(oid) as number;
    const view = store.commitView(repo, oid);
    if (!view) continue;
    for (const parent of view.parents) {
      if (!gen.has(parent)) {
        gen.set(parent, depth + 1);
        queue.push(parent);
      }
    }
  }
  return gen;
}

/**
 * Plan a fetch: which objects to send, which commits are shallow, which client boundaries are
 * completed, and which haves to ACK. Depth limits commit generations only; a commit's trees and
 * blobs always travel whole. Haves exclude their reachable sets, stopping at the client's own
 * shallow boundaries, which the client does not hold beyond.
 */
export function planFetch(
  store: Store,
  repo: Repo,
  wants: readonly string[],
  haves: readonly string[],
  depth: number | undefined,
  clientShallow: readonly string[],
): FetchPlan {
  const lookup = (oid: string): GitObject | undefined => store.getObject(repo, oid);
  const gen = generations(store, repo, wants);
  const includedCommits = new Set<string>();
  for (const [oid, depthOf] of gen) {
    if (depth === undefined || depthOf <= depth) includedCommits.add(oid);
  }
  // The wanted set: included commits, plus tags naming them (a wanted tag travels with its target).
  const roots = new Set<string>(includedCommits);
  for (const want of wants) {
    const object = lookup(want);
    if (object && object.type === 'tag') roots.add(want);
  }
  // Walk from the roots, following trees fully but parent links only into included commits:
  // a depth limit must cut history off, not merely filter the starting set while the walk
  // re-adds every ancestor.
  const reachable = new Set<string>();
  {
    const stack = [...roots];
    const seen = new Set<string>();
    while (stack.length > 0) {
      const oid = stack.pop() as string;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const object = lookup(oid);
      if (!object) continue;
      reachable.add(oid);
      if (object.type === 'commit') {
        const view = store.commitView(repo, oid);
        if (!view) continue;
        stack.push(view.tree);
        for (const parent of view.parents) {
          if (includedCommits.has(parent)) stack.push(parent);
        }
      } else if (object.type === 'tag') {
        try {
          const target = parseTag(object.data).object;
          const targetObject = lookup(target);
          if (targetObject?.type === 'commit') {
            if (includedCommits.has(target)) stack.push(target);
          } else {
            stack.push(target);
          }
        } catch {
          // A corrupt tag contributes nothing further.
        }
      } else if (object.type === 'tree') {
        for (const entry of store.treeEntries(repo, oid)) {
          if (entry.mode !== '160000') stack.push(entry.oid);
        }
      }
    }
  }
  // Shallow: boundary commits with parents in the store, whose parents were cut off.
  const shallow: string[] = [];
  if (depth !== undefined) {
    for (const [oid, depthOf] of gen) {
      if (depthOf !== depth) continue;
      const view = store.commitView(repo, oid);
      if (view && view.parents.length > 0) shallow.push(oid);
    }
  }
  shallow.sort();
  // Haves exclude their reachable sets, stopping traversal at client shallow boundaries: the
  // client holds a boundary commit and its tree, but not the boundary's parents.
  const excluded = new Set<string>();
  const boundarySet = new Set(clientShallow);
  for (const have of haves) {
    const queue = [have];
    const seen = new Set<string>();
    while (queue.length > 0) {
      const oid = queue.pop() as string;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const object = lookup(oid);
      if (!object) continue;
      excluded.add(oid);
      if (object.type === 'commit') {
        if (boundarySet.has(oid)) {
          // Include the commit's tree, not its parents.
          const view = store.commitView(repo, oid);
          if (view) for (const treeOid of collectReachable([view.tree], lookup, { allowMissing: true })) excluded.add(treeOid);
          continue;
        }
        const view = store.commitView(repo, oid);
        if (view) {
          queue.push(view.tree);
          queue.push(...view.parents);
        }
      } else if (object.type === 'tag') {
        try {
          queue.push(parseTag(object.data).object);
        } catch {
          // A corrupt tag contributes nothing further.
        }
      } else if (object.type === 'tree') {
        for (const entry of store.treeEntries(repo, oid)) {
          if (entry.mode !== '160000') queue.push(entry.oid);
        }
      }
    }
  }
  const objects: GitObject[] = [];
  for (const oid of [...reachable].sort()) {
    if (excluded.has(oid)) continue;
    const object = lookup(oid);
    if (object) objects.push(object);
  }
  // Unshallow: client boundaries reachable from the wants whose parents are all included.
  const unshallow: string[] = [];
  for (const boundary of clientShallow) {
    if (!reachable.has(boundary)) continue;
    const view = store.commitView(repo, boundary);
    if (!view) continue;
    if (view.parents.every((parent) => reachable.has(parent) && includedCommits.has(parent))) {
      unshallow.push(boundary);
    }
  }
  unshallow.sort();
  // ACKs: haves that are ancestors of a want, in request order, deduplicated.
  const acks: string[] = [];
  const seenAck = new Set<string>();
  for (const have of haves) {
    if (seenAck.has(have)) continue;
    seenAck.add(have);
    for (const want of wants) {
      const peeled = peelToCommit(want, lookup);
      if (peeled !== undefined && store.isAncestor(repo, have, peeled)) {
        acks.push(have);
        break;
      }
    }
  }
  return { objects, shallow, unshallow, acks };
}

/** The pack for a fetch plan, cached by the request shape so repeated clones share one build. */
function fetchPack(store: Store, repo: Repo, plan: FetchPlan, cacheKey: string): Buffer {
  const cached = store.cachedPack(cacheKey);
  if (cached !== undefined) return cached;
  const pack = writePack(plan.objects);
  store.cachePack(cacheKey, pack);
  return pack;
}

function fetchCacheKey(repo: Repo, wants: readonly string[], haves: readonly string[], depth: number | undefined, clientShallow: readonly string[]): string {
  return `${repo.fullName}|fetch|${[...wants].sort().join(',')}|${[...haves].sort().join(',')}|${depth ?? ''}|${[...clientShallow].sort().join(',')}`;
}

// --------------------------------------------------------------------------- v2 commands

interface LsRefsArgs {
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
  prefixes: string[];
}

function handleLsRefs(store: Store, repo: Repo, args: LsRefsArgs): Buffer {
  const headRef = `refs/heads/${repo.defaultBranch}`;
  const refs = store.listRefs(repo);
  const lines: string[] = [];
  const keep = (name: string): boolean => args.prefixes.length === 0 || args.prefixes.some((prefix) => name.startsWith(prefix));
  const headTip = store.getRef(repo, headRef);
  if (keep('HEAD')) {
    if (headTip === undefined) {
      if (args.unborn) {
        lines.push(args.symrefs ? `unborn HEAD symref-target:${headRef}\n` : 'unborn HEAD\n');
      }
    } else {
      lines.push(args.symrefs ? `${headTip} HEAD symref-target:${headRef}\n` : `${headTip} HEAD\n`);
    }
  }
  for (const ref of refs) {
    if (!keep(ref.name)) continue;
    let line = `${ref.oid} ${ref.name}`;
    if (args.peel) {
      const peeled = peeledTarget(store, repo, ref.oid);
      if (peeled !== undefined) line += ` peeled:${peeled}`;
    }
    lines.push(`${line}\n`);
  }
  return encodePktLines([...lines, 'flush']);
}

function handleFetchV2(
  store: Store,
  repo: Repo,
  wants: string[],
  haves: string[],
  depth: number | undefined,
  clientShallow: string[],
): Buffer {
  const cacheKey = fetchCacheKey(repo, wants, haves, depth, clientShallow);
  const responseKey = `v2|${cacheKey}`;
  const cached = store.cachedFetch(responseKey);
  if (cached !== undefined) return cached;
  const plan = planFetch(store, repo, wants, haves, depth, clientShallow);
  const pack = fetchPack(store, repo, plan, cacheKey);
  const parts: Buffer[] = [];
  // Acknowledgments: only when the client sent haves. A client that sent none (a fresh clone)
  // expects the packfile section next and rejects an acknowledgments section outright, as real
  // git does; when haves were sent, ACKs plus ready, or NAK alone.
  if (haves.length > 0) {
    const ackLines: Array<string | 'delim'> = ['acknowledgments\n'];
    if (plan.acks.length > 0) {
      for (const ack of plan.acks) ackLines.push(`ACK ${ack}\n`);
      ackLines.push('ready\n');
    } else {
      ackLines.push('NAK\n');
    }
    ackLines.push('delim');
    parts.push(encodePktLines(ackLines));
  }
  // Shallow info, only when there is something to say.
  if (plan.shallow.length > 0 || plan.unshallow.length > 0) {
    const shallowLines: Array<string | 'delim'> = ['shallow-info\n'];
    for (const oid of plan.shallow) shallowLines.push(`shallow ${oid}\n`);
    for (const oid of plan.unshallow) shallowLines.push(`unshallow ${oid}\n`);
    shallowLines.push('delim');
    parts.push(encodePktLines(shallowLines));
  }
  // Packfile: header, then the pack in side-band packets, then flush.
  parts.push(encodePktLine('packfile\n'));
  parts.push(sidebandWrap(pack, 1));
  const response = Buffer.concat(parts);
  store.cacheFetch(responseKey, response);
  return response;
}

function handleFetchV0(
  store: Store,
  repo: Repo,
  wants: string[],
  haves: string[],
  depth: number | undefined,
  clientShallow: string[],
): Buffer {
  const cacheKey = fetchCacheKey(repo, wants, haves, depth, clientShallow);
  const responseKey = `v0|${cacheKey}`;
  const cached = store.cachedFetch(responseKey);
  if (cached !== undefined) return cached;
  const plan = planFetch(store, repo, wants, haves, depth, clientShallow);
  const pack = fetchPack(store, repo, plan, cacheKey);
  const parts: Buffer[] = [];
  const headLines: string[] = [];
  if (plan.acks.length > 0) {
    for (const ack of plan.acks) headLines.push(`ACK ${ack}\n`);
  } else {
    headLines.push('NAK\n');
  }
  for (const oid of plan.shallow) headLines.push(`shallow ${oid}\n`);
  for (const oid of plan.unshallow) headLines.push(`unshallow ${oid}\n`);
  if (headLines.length > 0) parts.push(encodePktLines(headLines));
  parts.push(sidebandWrap(pack, 1));
  const response = Buffer.concat(parts);
  store.cacheFetch(responseKey, response);
  return response;
}

/**
 * POST /git-upload-pack. A body starting with `command=` is protocol v2 (ls-refs or fetch);
 * anything else is v0 (want/have/done). Unknown v2 commands answer a side-band error the
 * client surfaces, never a crash.
 */
export function gitUploadPack(store: Store, repo: Repo, body: Buffer): GitResponse {
  const { packets } = decodePktLines(body);
  const lines = packetsToLines(packets);
  const first = (lines[0] ?? '').trim();
  if (first.startsWith('command=')) {
    const command = first.slice('command='.length);
    // Arguments live after the delim packet; everything before it is the command envelope.
    const delimIndex = packets.findIndex((packet) => packet.kind === 'delim');
    const argPackets = delimIndex >= 0 ? packets.slice(delimIndex + 1) : packets.slice(1);
    const args = packetsToLines(argPackets).map((line) => line.replace(/\n$/, ''));
    if (command === 'ls-refs') {
      const lsArgs: LsRefsArgs = { peel: false, symrefs: false, unborn: false, prefixes: [] };
      for (const arg of args) {
        if (arg === 'peel') lsArgs.peel = true;
        else if (arg === 'symrefs') lsArgs.symrefs = true;
        else if (arg === 'unborn') lsArgs.unborn = true;
        else if (arg.startsWith('ref-prefix ')) lsArgs.prefixes.push(arg.slice('ref-prefix '.length));
      }
      return gitResponse('application/x-git-upload-pack-result', handleLsRefs(store, repo, lsArgs));
    }
    if (command === 'fetch') {
      const wants: string[] = [];
      const haves: string[] = [];
      const clientShallow: string[] = [];
      let depth: number | undefined;
      for (const arg of args) {
        if (arg.startsWith('want ')) wants.push(arg.slice(5).trim());
        else if (arg.startsWith('have ')) haves.push(arg.slice(5).trim());
        else if (arg.startsWith('shallow ')) clientShallow.push(arg.slice(8).trim());
        else if (arg.startsWith('deepen ')) {
          const parsed = Number.parseInt(arg.slice(7).trim(), 10);
          if (Number.isSafeInteger(parsed) && parsed > 0) depth = parsed;
        }
      }
      return gitResponse(
        'application/x-git-upload-pack-result',
        handleFetchV2(store, repo, wants, haves, depth, clientShallow),
      );
    }
    // Unknown command: a packfile section carrying a side-band error, which the client reports.
    const error = encodePktLines(['packfile\n']);
    const errPacket = encodePktLine(Buffer.concat([Buffer.from([3]), Buffer.from(`unknown command ${command}\n`, 'utf8')]));
    return gitResponse(
      'application/x-git-upload-pack-result',
      Buffer.concat([error, errPacket, Buffer.from('0000', 'ascii')]),
    );
  }
  // Protocol v0: want/have/shallow/deepen/done lines, in one or two flush-separated sections.
  const wants: string[] = [];
  const haves: string[] = [];
  const clientShallow: string[] = [];
  let depth: number | undefined;
  for (const raw of lines) {
    const line = raw.replace(/\n$/, '');
    if (line.startsWith('want ')) {
      const oid = line.slice(5).split(' ')[0] as string;
      if (isObjectId(oid)) wants.push(oid);
    } else if (line.startsWith('have ')) {
      const oid = line.slice(5).trim();
      if (isObjectId(oid)) haves.push(oid);
    } else if (line.startsWith('shallow ')) {
      const oid = line.slice(8).trim();
      if (isObjectId(oid)) clientShallow.push(oid);
    } else if (line.startsWith('deepen ')) {
      const parsed = Number.parseInt(line.slice(7).trim(), 10);
      if (Number.isSafeInteger(parsed) && parsed > 0) depth = parsed;
    }
  }
  return gitResponse(
    'application/x-git-upload-pack-result',
    handleFetchV0(store, repo, wants, haves, depth, clientShallow),
  );
}

// --------------------------------------------------------------------------- receive-pack

interface PushCommand {
  readonly oldOid: string;
  readonly newOid: string;
  readonly ref: string;
}

/**
 * POST /git-receive-pack. Parses the command list and the pack that follows its flush, stores
 * the pack's objects, then applies each ref update independently: fast-forwards move, anything
 * else is refused per ref with its reason. A corrupt pack refuses every ref with an unpack
 * error. The report is always well-formed; a malformed body never crashes the server.
 */
export function gitReceivePack(store: Store, repo: Repo, body: Buffer): GitResponse {
  const commands: PushCommand[] = [];
  let packBytes: Buffer = Buffer.alloc(0);
  let sideband = true;
  let parseError: string | null = null;
  try {
    const { packets, rest } = decodePktLines(body);
    packBytes = rest;
    const lines = packetsToLines(packets);
    for (let index = 0; index < lines.length; index += 1) {
      let line = (lines[index] as string).replace(/\n$/, '');
      if (index === 0) {
        const nul = line.indexOf('\u0000');
        if (nul >= 0) {
          const caps = line.slice(nul + 1).split(' ');
          sideband = caps.includes('side-band-64k');
          line = line.slice(0, nul);
        }
      }
      if (line === '') continue;
      const parts = line.split(' ');
      if (parts.length !== 3 || !isObjectId(parts[0] as string) || !isObjectId(parts[1] as string)) {
        parseError = `bad command line ${JSON.stringify(line.slice(0, 60))}`;
        break;
      }
      commands.push({ oldOid: parts[0] as string, newOid: parts[1] as string, ref: parts[2] as string });
    }
  } catch (err) {
    parseError = err instanceof Error ? err.message : String(err);
  }

  const reportLines: string[] = [];
  let unpackOk = false;
  let unpackDetail = 'ok';
  if (parseError !== null) {
    unpackDetail = parseError;
  } else if (commands.length === 0) {
    unpackDetail = 'no commands';
  } else {
    const updatesRef = commands.some((command) => command.newOid !== ZERO_OID);
    let packObjects: Map<string, GitObject> | null = null;
    if (updatesRef || packBytes.length > 0) {
      try {
        if (packBytes.length === 0) throw new Error('missing packfile');
        const read = readPack(packBytes, { resolveBase: (oid) => store.getObject(repo, oid) });
        if (!read.checksumOk) throw new Error('pack checksum mismatch');
        packObjects = read.objects;
      } catch (err) {
        unpackDetail = err instanceof Error ? err.message : String(err);
      }
    } else {
      packObjects = new Map();
    }
    if (packObjects !== null) {
      unpackOk = true;
      store.putObjects(repo, packObjects.values());
      for (const command of commands) {
        const current = store.getRef(repo, command.ref);
        if (command.newOid === ZERO_OID) {
          // Delete.
          if (current === undefined) {
            reportLines.push(`ng ${command.ref} remote ref does not exist\n`);
          } else if (command.oldOid !== ZERO_OID && command.oldOid !== current) {
            reportLines.push(`ng ${command.ref} stale info\n`);
          } else {
            store.deleteRef(repo, command.ref);
            reportLines.push(`ok ${command.ref}\n`);
          }
          continue;
        }
        if (store.getObject(repo, command.newOid) === undefined) {
          reportLines.push(`ng ${command.ref} object not found\n`);
          continue;
        }
        if (command.oldOid === ZERO_OID) {
          // Create.
          if (current !== undefined) {
            reportLines.push(`ng ${command.ref} already exists\n`);
          } else {
            store.setRef(repo, command.ref, command.newOid);
            reportLines.push(`ok ${command.ref}\n`);
          }
          continue;
        }
        // Update.
        if (current === undefined) {
          reportLines.push(`ng ${command.ref} remote ref does not exist\n`);
        } else if (current !== command.oldOid) {
          reportLines.push(`ng ${command.ref} stale info\n`);
        } else if (current === command.newOid) {
          reportLines.push(`ok ${command.ref}\n`);
        } else if (!store.isAncestor(repo, current, command.newOid)) {
          reportLines.push(`ng ${command.ref} non-fast-forward\n`);
        } else {
          store.setRef(repo, command.ref, command.newOid);
          reportLines.push(`ok ${command.ref}\n`);
        }
      }
    } else {
      for (const command of commands) reportLines.push(`ng ${command.ref} unpacker error\n`);
    }
  }
  if (!unpackOk && reportLines.length === 0) {
    for (const command of commands) reportLines.push(`ng ${command.ref} unpacker error\n`);
  }
  const report = encodePktLines([`unpack ${unpackOk ? 'ok' : unpackDetail}\n`, ...reportLines, 'flush']);
  const framed = sideband ? sidebandWrap(report, 1) : report;
  return gitResponse('application/x-git-receive-pack-result', framed);
}
