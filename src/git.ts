// Git object model, packfiles and pkt-lines, on node:crypto and node:zlib only.
// Everything here is pure: no store, no HTTP. The forge's repositories hold GitObj values keyed by oid.

import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export type ObjType = 'commit' | 'tree' | 'blob' | 'tag';

/** One stored object. `z` caches its zlib-deflated bytes so a pack is written without deflating twice. */
export interface GitObj {
  type: ObjType;
  data: Buffer;
  z?: Buffer;
}

export const ZERO_OID = '0000000000000000000000000000000000000000';
const TYPE_NUM: Record<ObjType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const NUM_TYPE: Record<number, ObjType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

export function hashObject(type: ObjType, data: Buffer): string {
  return createHash('sha1').update(`${type} ${data.length}\0`).update(data).digest('hex');
}

export function isOid(value: unknown): value is string {
  return typeof value === 'string' && /^[0-9a-f]{40}$/.test(value);
}

// ------------------------------------------------------------------ commits, trees, tags

export interface Commit {
  tree: string;
  parents: string[];
  author: string;
  committer: string;
  message: string;
}

export function parseCommit(data: Buffer): Commit {
  const text = data.toString('utf8');
  const split = text.indexOf('\n\n');
  const head = split < 0 ? text : text.slice(0, split);
  const message = split < 0 ? '' : text.slice(split + 2);
  const commit: Commit = { tree: '', parents: [], author: '', committer: '', message };
  for (const line of head.split('\n')) {
    const space = line.indexOf(' ');
    const key = line.slice(0, space);
    const value = line.slice(space + 1);
    if (key === 'tree') commit.tree = value;
    else if (key === 'parent') commit.parents.push(value);
    else if (key === 'author') commit.author = value;
    else if (key === 'committer') commit.committer = value;
  }
  return commit;
}

export function buildCommit(fields: { tree: string; parents: readonly string[]; author: string; committer: string; message: string }): Buffer {
  const lines = [`tree ${fields.tree}`, ...fields.parents.map((p) => `parent ${p}`), `author ${fields.author}`, `committer ${fields.committer}`];
  return Buffer.from(`${lines.join('\n')}\n\n${fields.message}`, 'utf8');
}

/** `Name <email> 1700000000 +0000` -> its parts. */
export function parseIdent(ident: string): { name: string; email: string; date: string } {
  const match = /^(.*?) <([^>]*)> (\d+) ([+-]\d{4})$/.exec(ident);
  if (match === null) return { name: ident, email: '', date: isoSeconds(0) };
  return { name: match[1] ?? '', email: match[2] ?? '', date: isoSeconds(Number(match[3])) };
}

export function isoSeconds(epochSec: number): string {
  return new Date(epochSec * 1000).toISOString().replace(/\.\d{3}Z$/, 'Z');
}

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let pos = 0;
  while (pos < data.length) {
    const space = data.indexOf(0x20, pos);
    const nul = data.indexOf(0, space);
    if (space < 0 || nul < 0 || nul + 21 > data.length) throw new Error('malformed tree');
    entries.push({
      mode: data.toString('latin1', pos, space),
      name: data.toString('utf8', space + 1, nul),
      oid: data.toString('hex', nul + 1, nul + 21),
    });
    pos = nul + 21;
  }
  return entries;
}

/** Git's tree order: names compared bytewise, a directory as if its name ended in '/'. */
function treeKey(entry: TreeEntry): string {
  return entry.mode === '40000' ? `${entry.name}/` : entry.name;
}

export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const ka = Buffer.from(treeKey(a));
    const kb = Buffer.from(treeKey(b));
    return Buffer.compare(ka, kb);
  });
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'), Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

export interface Tag {
  object: string;
  type: string;
  tag: string;
}

export function parseTag(data: Buffer): Tag {
  const tag: Tag = { object: '', type: '', tag: '' };
  const text = data.toString('utf8');
  const end = text.indexOf('\n\n');
  for (const line of (end < 0 ? text : text.slice(0, end)).split('\n')) {
    const space = line.indexOf(' ');
    const key = line.slice(0, space);
    if (key === 'object' || key === 'type' || key === 'tag') tag[key] = line.slice(space + 1);
  }
  return tag;
}

// ------------------------------------------------------------------ deltas

function readSize(delta: Buffer, start: number): { value: number; next: number } {
  let value = 0;
  let shift = 0;
  let pos = start;
  for (;;) {
    const byte = delta[pos++];
    if (byte === undefined) throw new Error('truncated delta header');
    value += (byte & 0x7f) * 2 ** shift;
    shift += 7;
    if ((byte & 0x80) === 0) return { value, next: pos };
  }
}

export function applyDelta(base: Buffer, delta: Buffer): Buffer {
  const source = readSize(delta, 0);
  if (source.value !== base.length) throw new Error('delta base size mismatch');
  const target = readSize(delta, source.next);
  const out = Buffer.allocUnsafe(target.value);
  let pos = target.next;
  let written = 0;
  while (pos < delta.length) {
    const op = delta[pos++] as number;
    if (op & 0x80) {
      let offset = 0;
      let size = 0;
      if (op & 0x01) offset |= delta[pos++] as number;
      if (op & 0x02) offset |= (delta[pos++] as number) << 8;
      if (op & 0x04) offset |= (delta[pos++] as number) << 16;
      if (op & 0x08) offset += (delta[pos++] as number) * 2 ** 24;
      if (op & 0x10) size |= delta[pos++] as number;
      if (op & 0x20) size |= (delta[pos++] as number) << 8;
      if (op & 0x40) size |= (delta[pos++] as number) << 16;
      if (size === 0) size = 0x10000;
      if (offset + size > base.length || written + size > out.length) throw new Error('delta copy out of range');
      base.copy(out, written, offset, offset + size);
      written += size;
    } else if (op > 0) {
      if (written + op > out.length || pos + op > delta.length) throw new Error('delta insert out of range');
      delta.copy(out, written, pos, pos + op);
      pos += op;
      written += op;
    } else {
      throw new Error('delta opcode 0');
    }
  }
  if (written !== out.length) throw new Error('delta result size mismatch');
  return out;
}

// ------------------------------------------------------------------ packs

interface PackEntry {
  offset: number;
  kind: number;
  raw: Buffer;
  z?: Buffer;
  baseOffset?: number;
  baseOid?: string;
}

/**
 * Read a packfile into objects. Deltas (ofs and ref) are resolved; a ref-delta base may come from
 * `lookup` (a thin pack against the repository). The trailer is verified.
 */
export function readPack(pack: Buffer, lookup: (oid: string) => GitObj | undefined): Map<string, GitObj> {
  const objects = new Map<string, GitObj>();
  if (pack.length === 0) return objects;
  if (pack.length < 32 || pack.toString('latin1', 0, 4) !== 'PACK') throw new Error('not a packfile');
  const version = pack.readUInt32BE(4);
  if (version !== 2 && version !== 3) throw new Error(`pack version ${version}`);
  const count = pack.readUInt32BE(8);
  const trailer = pack.subarray(pack.length - 20).toString('hex');
  const digest = createHash('sha1').update(pack.subarray(0, pack.length - 20)).digest('hex');
  if (trailer !== digest) throw new Error('pack checksum mismatch');
  const entries: PackEntry[] = [];
  const byOffset = new Map<number, PackEntry>();
  let pos = 12;
  for (let index = 0; index < count; index += 1) {
    const offset = pos;
    let byte = pack[pos++] as number;
    const kind = (byte >> 4) & 7;
    let shift = 4;
    while (byte & 0x80) {
      byte = pack[pos++] as number;
      shift += 7;
    }
    void shift;
    const entry: PackEntry = { offset, kind, raw: Buffer.alloc(0) };
    if (kind === 6) {
      byte = pack[pos++] as number;
      let rel = byte & 0x7f;
      while (byte & 0x80) {
        byte = pack[pos++] as number;
        rel = (rel + 1) * 128 + (byte & 0x7f);
      }
      entry.baseOffset = offset - rel;
    } else if (kind === 7) {
      entry.baseOid = pack.toString('hex', pos, pos + 20);
      pos += 20;
    } else if (NUM_TYPE[kind] === undefined) {
      throw new Error(`bad pack object type ${kind}`);
    }
    const inflated = inflateSync(pack.subarray(pos, pack.length - 20), { info: true }) as unknown as { buffer: Buffer; engine: { bytesWritten: number } };
    const used = inflated.engine.bytesWritten;
    entry.raw = inflated.buffer;
    if (kind !== 6 && kind !== 7) entry.z = Buffer.from(pack.subarray(pos, pos + used));
    pos += used;
    entries.push(entry);
    byOffset.set(offset, entry);
  }
  const resolved = new Map<number, { type: ObjType; data: Buffer }>();
  const byOid = new Map<string, { type: ObjType; data: Buffer }>();
  const resolve = (entry: PackEntry, depth: number): { type: ObjType; data: Buffer } | null => {
    const done = resolved.get(entry.offset);
    if (done !== undefined) return done;
    if (depth > 4000) throw new Error('delta chain too deep');
    let result: { type: ObjType; data: Buffer } | null = null;
    if (entry.kind === 6) {
      const base = byOffset.get(entry.baseOffset as number);
      if (base === undefined) throw new Error('ofs-delta base not in pack');
      const baseObj = resolve(base, depth + 1);
      if (baseObj === null) return null;
      result = { type: baseObj.type, data: applyDelta(baseObj.data, entry.raw) };
    } else if (entry.kind === 7) {
      const oid = entry.baseOid as string;
      const baseObj = byOid.get(oid) ?? lookup(oid) ?? objects.get(oid);
      if (baseObj === undefined) return null;
      result = { type: baseObj.type, data: applyDelta(baseObj.data, entry.raw) };
    } else {
      result = { type: NUM_TYPE[entry.kind] as ObjType, data: entry.raw };
    }
    resolved.set(entry.offset, result);
    const oid = hashObject(result.type, result.data);
    byOid.set(oid, result);
    if (!objects.has(oid)) objects.set(oid, { type: result.type, data: result.data, z: entry.z });
    return result;
  };
  // Ref-deltas may name a base that appears later in the pack: sweep until nothing new resolves.
  let pending = entries;
  while (pending.length > 0) {
    const next: PackEntry[] = [];
    for (const entry of pending) if (resolve(entry, 0) === null) next.push(entry);
    if (next.length === pending.length) throw new Error('pack holds a delta whose base is missing');
    pending = next;
  }
  return objects;
}

function packHeader(type: ObjType, size: number): Buffer {
  const bytes: number[] = [];
  let byte = (TYPE_NUM[type] << 4) | (size & 0x0f);
  let rest = Math.floor(size / 16);
  while (rest > 0) {
    bytes.push(byte | 0x80);
    byte = rest & 0x7f;
    rest = Math.floor(rest / 128);
  }
  bytes.push(byte);
  return Buffer.from(bytes);
}

export function deflated(obj: GitObj): Buffer {
  if (obj.z === undefined) obj.z = deflateSync(obj.data, { level: 1 });
  return obj.z;
}

/** A version-2 pack of whole (undeltified) objects, its SHA-1 trailer appended. */
export function writePack(objects: readonly GitObj[]): Buffer {
  const parts: Buffer[] = [];
  const header = Buffer.alloc(12);
  header.write('PACK', 0, 'latin1');
  header.writeUInt32BE(2, 4);
  header.writeUInt32BE(objects.length, 8);
  parts.push(header);
  for (const obj of objects) parts.push(packHeader(obj.type, obj.data.length), deflated(obj));
  const body = Buffer.concat(parts);
  return Buffer.concat([body, createHash('sha1').update(body).digest()]);
}

// ------------------------------------------------------------------ pkt-lines

export const FLUSH = Buffer.from('0000', 'latin1');
export const DELIM = Buffer.from('0001', 'latin1');

export function pkt(payload: string | Buffer): Buffer {
  const body = typeof payload === 'string' ? Buffer.from(payload, 'utf8') : payload;
  const head = Buffer.from((body.length + 4).toString(16).padStart(4, '0'), 'latin1');
  return Buffer.concat([head, body]);
}

export type Packet = { kind: 'data'; payload: Buffer } | { kind: 'flush' } | { kind: 'delim' } | { kind: 'end' };

/** Read pkt-lines from `start` until a flush (included) or the end of the buffer. */
export function readPackets(buf: Buffer, start = 0, stopAtFlush = true): { packets: Packet[]; next: number } {
  const packets: Packet[] = [];
  let pos = start;
  while (pos < buf.length) {
    if (pos + 4 > buf.length) throw new Error('truncated pkt-line length');
    const hex = buf.toString('latin1', pos, pos + 4);
    if (!/^[0-9a-fA-F]{4}$/.test(hex)) throw new Error('bad pkt-line length');
    const length = Number.parseInt(hex, 16);
    if (length === 0) {
      packets.push({ kind: 'flush' });
      pos += 4;
      if (stopAtFlush) return { packets, next: pos };
      continue;
    }
    if (length === 1) {
      packets.push({ kind: 'delim' });
      pos += 4;
      continue;
    }
    if (length === 2) {
      packets.push({ kind: 'end' });
      pos += 4;
      continue;
    }
    if (length < 4 || pos + length > buf.length) throw new Error('truncated pkt-line');
    packets.push({ kind: 'data', payload: buf.subarray(pos + 4, pos + length) });
    pos += length;
  }
  return { packets, next: pos };
}

/** The packet's payload as text, trailing newline removed. */
export function lineOf(packet: Packet): string | null {
  if (packet.kind !== 'data') return null;
  return packet.payload.toString('utf8').replace(/\n$/, '');
}

/** Frame `data` in side-band packets on `band`, each at most 65520 bytes on the wire. */
export function sideband(band: number, data: Buffer): Buffer[] {
  const out: Buffer[] = [];
  const max = 65515;
  for (let pos = 0; pos < data.length; pos += max) {
    const chunk = data.subarray(pos, Math.min(data.length, pos + max));
    out.push(pkt(Buffer.concat([Buffer.from([band]), chunk])));
  }
  return out;
}
