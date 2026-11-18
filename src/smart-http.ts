// Git smart HTTP: info/refs, upload-pack (protocol v2, with a v0 fallback) and receive-pack (v0).

import {
  DELIM,
  FLUSH,
  type GitObj,
  type Packet,
  ZERO_OID,
  isOid,
  lineOf,
  parseTag,
  pkt,
  readPack,
  readPackets,
  sideband,
  writePack,
} from './git.ts';
import { type Repo, isAncestor } from './store.ts';

export interface GitReply {
  status: number;
  type: string;
  body: Buffer;
}

const AGENT = 'agent=forge/1.0';

function text(status: number, message: string): GitReply {
  return { status, type: 'text/plain; charset=utf-8', body: Buffer.from(`${message}\n`) };
}

/** Refs in advertisement order: sorted by name. */
function sortedRefs(repo: Repo): Array<[string, string]> {
  return [...repo.refs.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
}

function headTarget(repo: Repo): string {
  return `refs/heads/${repo.defaultBranch}`;
}

export function infoRefs(repo: Repo, service: string, v2: boolean): GitReply {
  if (service === 'git-upload-pack' && v2) {
    const body = Buffer.concat([
      pkt('version 2\n'),
      pkt(`${AGENT}\n`),
      pkt('ls-refs\n'),
      pkt('fetch=shallow\n'),
      pkt('server-option\n'),
      pkt('object-format=sha1\n'),
      FLUSH,
    ]);
    return { status: 200, type: 'application/x-git-upload-pack-advertisement', body };
  }
  const parts: Buffer[] = [pkt(`# service=${service}\n`), FLUSH];
  const caps =
    service === 'git-receive-pack'
      ? `report-status delete-refs side-band-64k quiet ofs-delta ${AGENT} object-format=sha1`
      : `multi_ack_detailed side-band-64k thin-pack ofs-delta shallow no-progress include-tag allow-tip-sha1-in-want allow-reachable-sha1-in-want symref=HEAD:${headTarget(repo)} ${AGENT} object-format=sha1`;
  const refs = sortedRefs(repo);
  const lines: string[] = [];
  const head = repo.refs.get(headTarget(repo));
  if (service === 'git-upload-pack' && head !== undefined) lines.push(`${head} HEAD`);
  for (const [name, oid] of refs) {
    lines.push(`${oid} ${name}`);
    if (service === 'git-upload-pack' && name.startsWith('refs/tags/')) {
      const peeled = repo.peel(oid);
      if (peeled !== oid) lines.push(`${peeled} ${name}^{}`);
    }
  }
  if (lines.length === 0) lines.push(`${ZERO_OID} capabilities^{}`);
  lines.forEach((line, index) => parts.push(pkt(index === 0 ? `${line}\0${caps}\n` : `${line}\n`)));
  parts.push(FLUSH);
  return { status: 200, type: `application/x-${service}-advertisement`, body: Buffer.concat(parts) };
}

// ------------------------------------------------------------------ upload-pack

export function uploadPack(repo: Repo, body: Buffer): GitReply {
  let packets: Packet[];
  try {
    packets = readPackets(body, 0, false).packets;
  } catch (err) {
    return text(400, `bad request: ${(err as Error).message}`);
  }
  const first = packets[0] !== undefined ? lineOf(packets[0]) : null;
  if (first !== null && first.startsWith('command=')) return uploadPackV2(repo, first.slice(8), packets);
  return uploadPackV0(repo, packets);
}

function uploadPackV2(repo: Repo, command: string, packets: Packet[]): GitReply {
  const delim = packets.findIndex((packet) => packet.kind === 'delim');
  const args: string[] = [];
  for (const packet of delim < 0 ? [] : packets.slice(delim + 1)) {
    const line = lineOf(packet);
    if (line !== null) args.push(line);
  }
  if (command === 'ls-refs') return lsRefs(repo, args);
  if (command === 'fetch') return fetchV2(repo, args);
  return text(400, `unknown command ${command}`);
}

function lsRefs(repo: Repo, args: string[]): GitReply {
  const symrefs = args.includes('symrefs');
  const peel = args.includes('peel');
  const unborn = args.includes('unborn');
  const prefixes = args.filter((arg) => arg.startsWith('ref-prefix ')).map((arg) => arg.slice(11));
  const keep = (name: string): boolean => prefixes.length === 0 || prefixes.some((prefix) => name.startsWith(prefix));
  const parts: Buffer[] = [];
  const target = headTarget(repo);
  const head = repo.refs.get(target);
  if (keep('HEAD')) {
    if (head !== undefined) parts.push(pkt(`${head} HEAD${symrefs ? ` symref-target:${target}` : ''}\n`));
    else if (unborn && symrefs) parts.push(pkt(`unborn HEAD symref-target:${target}\n`));
  }
  for (const [name, oid] of sortedRefs(repo)) {
    if (!keep(name)) continue;
    let line = `${oid} ${name}`;
    if (peel && name.startsWith('refs/tags/')) {
      const peeled = repo.peel(oid);
      if (peeled !== oid) line += ` peeled:${peeled}`;
    }
    parts.push(pkt(`${line}\n`));
  }
  parts.push(FLUSH);
  return { status: 200, type: 'application/x-git-upload-pack-result', body: Buffer.concat(parts) };
}

interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
  done: boolean;
}

function parseFetchArgs(lines: string[]): FetchArgs | string {
  const args: FetchArgs = { wants: [], haves: [], shallow: [], depth: null, done: false };
  for (const raw of lines) {
    const space = raw.indexOf(' ');
    const word = space < 0 ? raw : raw.slice(0, space);
    const value = space < 0 ? '' : raw.slice(space + 1).split(' ')[0] ?? '';
    if (word === 'want') {
      if (!isOid(value)) return `bad want ${value}`;
      args.wants.push(value);
    } else if (word === 'have') {
      if (isOid(value)) args.haves.push(value);
    } else if (word === 'shallow') {
      if (isOid(value)) args.shallow.push(value);
    } else if (word === 'deepen') {
      const depth = Number(value);
      if (!Number.isInteger(depth) || depth < 1) return `bad deepen ${value}`;
      args.depth = depth;
    } else if (word === 'done') {
      args.done = true;
    }
  }
  return args;
}

interface PackPlan {
  objects: GitObj[];
  shallow: string[];
  unshallow: string[];
  common: string[];
}

/** Choose the objects a fetch sends, and the shallow boundary when it deepens. */
function planFetch(repo: Repo, args: FetchArgs): PackPlan | string {
  const clientShallow = new Set(args.shallow);
  // What the client holds: every have's closure, cut at its shallow commits.
  const held = new Set<string>();
  const common: string[] = [];
  const holdTree = (oid: string): void => {
    if (held.has(oid)) return;
    held.add(oid);
    const obj = repo.get(oid);
    if (obj === undefined || obj.type !== 'tree') return;
    for (const entry of repo.tree(oid)) if (entry.mode !== '160000') holdTree(entry.oid);
  };
  const stack: string[] = [];
  for (const have of args.haves) {
    if (repo.get(have) === undefined) continue;
    common.push(have);
    stack.push(have);
  }
  while (stack.length > 0) {
    const oid = stack.pop() as string;
    if (held.has(oid)) continue;
    const obj = repo.get(oid);
    if (obj === undefined) continue;
    held.add(oid);
    if (obj.type === 'tag') {
      stack.push(parseTag(obj.data).object);
    } else if (obj.type === 'commit') {
      const commit = repo.commit(oid);
      if (commit === null) continue;
      holdTree(commit.tree);
      if (!clientShallow.has(oid)) for (const parent of commit.parents) stack.push(parent);
    } else if (obj.type === 'tree') {
      held.delete(oid);
      holdTree(oid);
    }
  }

  const send: GitObj[] = [];
  const sent = new Set<string>();
  const sendObj = (oid: string): void => {
    if (sent.has(oid) || held.has(oid)) return;
    const obj = repo.get(oid);
    if (obj === undefined) return;
    sent.add(oid);
    send.push(obj);
  };
  const sendTree = (oid: string): void => {
    if (sent.has(oid) || held.has(oid)) return;
    sendObj(oid);
    for (const entry of repo.tree(oid)) {
      if (entry.mode === '160000') continue;
      if (entry.mode === '40000') sendTree(entry.oid);
      else sendObj(entry.oid);
    }
  };

  // Peel tag wants: the tag objects go in the pack, their targets become commit wants.
  const commitWants: string[] = [];
  for (const want of args.wants) {
    let oid = want;
    for (let i = 0; i < 10; i += 1) {
      const obj = repo.get(oid);
      if (obj === undefined) return `not our ref ${want}`;
      if (obj.type !== 'tag') break;
      sendObj(oid);
      oid = parseTag(obj.data).object;
    }
    const obj = repo.get(oid);
    if (obj === undefined) return `not our ref ${want}`;
    if (obj.type === 'commit') commitWants.push(oid);
    else if (obj.type === 'tree') sendTree(oid);
    else sendObj(oid);
  }

  const newShallow: string[] = [];
  const unshallow: string[] = [];
  // Breadth-first by depth from the wants: depth 1 is the wants themselves.
  const depthOf = new Map<string, number>();
  let frontier = commitWants.filter((oid) => !depthOf.has(oid));
  for (const oid of frontier) depthOf.set(oid, 1);
  const commits: string[] = [];
  let level = 1;
  while (frontier.length > 0) {
    const next: string[] = [];
    for (const oid of frontier) {
      const commit = repo.commit(oid);
      if (commit === null) continue;
      const atBoundary = args.depth !== null && level >= args.depth;
      if (held.has(oid) && !(args.depth !== null && clientShallow.has(oid))) continue;
      commits.push(oid);
      if (atBoundary) {
        if (commit.parents.length > 0) newShallow.push(oid);
        continue;
      }
      for (const parent of commit.parents) {
        if (depthOf.has(parent)) continue;
        depthOf.set(parent, level + 1);
        next.push(parent);
      }
    }
    frontier = next;
    level += 1;
  }
  for (const oid of clientShallow) {
    if (depthOf.has(oid) && !newShallow.includes(oid)) unshallow.push(oid);
  }
  for (const oid of commits) {
    const commit = repo.commit(oid);
    if (commit === null) continue;
    sendObj(oid);
    sendTree(commit.tree);
  }
  // Commits the client already holds were walked only to deepen through them: sendObj skips them.
  return { objects: send, shallow: args.depth === null ? [] : newShallow, unshallow, common };
}

function fetchV2(repo: Repo, lines: string[]): GitReply {
  const parsed = parseFetchArgs(lines);
  if (typeof parsed === 'string') return text(400, parsed);
  const cacheKey = parsed.haves.length === 0 && parsed.shallow.length === 0 ? `v2:${parsed.done}:${parsed.depth}:${parsed.wants.join(',')}` : null;
  if (cacheKey !== null) {
    const cached = repo.fetchCache.get(cacheKey);
    if (cached !== undefined) return { status: 200, type: 'application/x-git-upload-pack-result', body: cached };
  }
  const plan = planFetch(repo, parsed);
  if (typeof plan === 'string') {
    return { status: 200, type: 'application/x-git-upload-pack-result', body: Buffer.concat([pkt(`ERR ${plan}\n`), FLUSH]) };
  }
  const parts: Buffer[] = [];
  if (!parsed.done) {
    parts.push(pkt('acknowledgments\n'));
    if (plan.common.length === 0) parts.push(pkt('NAK\n'));
    for (const oid of plan.common) parts.push(pkt(`ACK ${oid}\n`));
    parts.push(pkt('ready\n'), DELIM);
  }
  if (parsed.depth !== null) {
    parts.push(pkt('shallow-info\n'));
    for (const oid of plan.shallow) parts.push(pkt(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(pkt(`unshallow ${oid}\n`));
    parts.push(DELIM);
  }
  parts.push(pkt('packfile\n'));
  parts.push(...sideband(1, writePack(plan.objects)));
  parts.push(FLUSH);
  const body = Buffer.concat(parts);
  if (cacheKey !== null) repo.fetchCache.set(cacheKey, body);
  return { status: 200, type: 'application/x-git-upload-pack-result', body };
}

function uploadPackV0(repo: Repo, packets: Packet[]): GitReply {
  const lines: string[] = [];
  let sawData = false;
  for (const packet of packets) {
    const line = lineOf(packet);
    if (line === null) continue;
    sawData = true;
    lines.push(line);
  }
  if (!sawData) return text(400, 'empty upload-pack request');
  const parsed = parseFetchArgs(lines);
  if (typeof parsed === 'string') return text(400, parsed);
  parsed.done = true;
  const plan = planFetch(repo, parsed);
  if (typeof plan === 'string') return text(400, plan);
  const parts: Buffer[] = [];
  if (parsed.depth !== null) {
    for (const oid of plan.shallow) parts.push(pkt(`shallow ${oid}\n`));
    for (const oid of plan.unshallow) parts.push(pkt(`unshallow ${oid}\n`));
    parts.push(FLUSH);
  }
  const last = plan.common[plan.common.length - 1];
  parts.push(pkt(last === undefined ? 'NAK\n' : `ACK ${last}\n`));
  parts.push(...sideband(1, writePack(plan.objects)));
  parts.push(FLUSH);
  return { status: 200, type: 'application/x-git-upload-pack-result', body: Buffer.concat(parts) };
}

// ------------------------------------------------------------------ receive-pack

interface Command {
  oldOid: string;
  newOid: string;
  ref: string;
}

export function receivePack(repo: Repo, body: Buffer): GitReply {
  let read: { packets: Packet[]; next: number };
  try {
    read = readPackets(body, 0, true);
  } catch (err) {
    return text(400, `bad request: ${(err as Error).message}`);
  }
  const commands: Command[] = [];
  let caps = new Set<string>();
  for (const packet of read.packets) {
    if (packet.kind !== 'data') continue;
    let line = packet.payload.toString('utf8').replace(/\n$/, '');
    const nul = line.indexOf('\0');
    if (nul >= 0) {
      caps = new Set(line.slice(nul + 1).split(' ').filter(Boolean));
      line = line.slice(0, nul);
    }
    const [oldOid, newOid, ref] = line.split(' ');
    if (!isOid(oldOid) || !isOid(newOid) || ref === undefined || !ref.startsWith('refs/')) return text(400, `bad command ${line.slice(0, 80)}`);
    commands.push({ oldOid, newOid, ref });
  }
  if (commands.length === 0) return text(400, 'no commands');

  let unpack = 'ok';
  const incoming = body.subarray(read.next);
  if (incoming.length > 0) {
    try {
      const objects = readPack(incoming, (oid) => repo.get(oid));
      for (const [oid, obj] of objects) if (!repo.objects.has(oid)) repo.objects.set(oid, obj);
    } catch (err) {
      unpack = `error ${(err as Error).message}`;
    }
  }

  const report: string[] = [`unpack ${unpack}\n`];
  const hadBranches = [...repo.refs.keys()].some((name) => name.startsWith('refs/heads/'));
  let firstNewBranch: string | null = null;
  for (const command of commands) {
    const reason = applyCommand(repo, command, unpack === 'ok');
    if (reason === null) {
      report.push(`ok ${command.ref}\n`);
      if (command.newOid !== ZERO_OID && command.oldOid === ZERO_OID && command.ref.startsWith('refs/heads/') && firstNewBranch === null) firstNewBranch = command.ref.slice(11);
    } else {
      report.push(`ng ${command.ref} ${reason}\n`);
    }
  }
  if (!hadBranches && firstNewBranch !== null && repo.branchTip(repo.defaultBranch) === undefined) repo.defaultBranch = firstNewBranch;

  const reportBytes = Buffer.concat([...report.map((line) => pkt(line)), FLUSH]);
  const wantsReport = caps.has('report-status') || caps.has('report-status-v2');
  if (!wantsReport) return { status: 200, type: 'application/x-git-receive-pack-result', body: Buffer.alloc(0) };
  const out = caps.has('side-band-64k') || caps.has('side-band') ? Buffer.concat([...sideband(1, reportBytes), FLUSH]) : reportBytes;
  return { status: 200, type: 'application/x-git-receive-pack-result', body: out };
}

/** Apply one ref update; null when it applied, else the ng reason. */
function applyCommand(repo: Repo, command: Command, unpacked: boolean): string | null {
  const current = repo.refs.get(command.ref) ?? ZERO_OID;
  if (command.newOid === ZERO_OID) {
    if (current === ZERO_OID) return 'no such ref';
    if (command.oldOid !== ZERO_OID && command.oldOid !== current) return 'stale info';
    repo.setRef(command.ref, null);
    return null;
  }
  if (!unpacked) return 'unpacker error';
  const target = repo.get(command.newOid);
  if (target === undefined) return 'missing necessary objects';
  if (command.ref.startsWith('refs/heads/') && repo.get(repo.peel(command.newOid))?.type !== 'commit') return 'not a commit';
  if (current === ZERO_OID) {
    if (command.oldOid !== ZERO_OID) return 'stale info';
    repo.setRef(command.ref, command.newOid);
    return null;
  }
  if (command.ref.startsWith('refs/tags/')) return 'already exists';
  if (command.oldOid !== current) {
    return isAncestor(repo, current, command.newOid) ? 'stale info' : 'non-fast-forward';
  }
  if (!isAncestor(repo, current, command.newOid)) return 'non-fast-forward';
  repo.setRef(command.ref, command.newOid);
  return null;
}
