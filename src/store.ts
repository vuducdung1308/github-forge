// The forge's one in-memory store: users, repositories (objects, refs, labels, issues, pull requests),
// and the git operations the REST, GraphQL and MCP surfaces share: server-side commits, merge bases,
// three-way squash merges and tree diffs.

import { createHash } from 'node:crypto';
import {
  type GitObj,
  type ObjType,
  type TreeEntry,
  buildCommit,
  buildTree,
  hashObject,
  isoSeconds,
  parseCommit,
  parseTag,
  parseTree,
} from './git.ts';

/** The login the trial's token names: the seeded scenario's viewer, the same in every trial. */
export const VIEWER_LOGIN = process.env.MAKEFUN_FORGE_LOGIN ?? 'adaiversen';

export interface User {
  kind: 'user';
  id: number;
  login: string;
  name: string;
  email: string;
}

export const viewer: User = { kind: 'user', id: 1001, login: VIEWER_LOGIN, name: VIEWER_LOGIN, email: `${VIEWER_LOGIN}@users.noreply.forge.local` };

export interface Label {
  kind: 'label';
  id: number;
  name: string;
  color: string;
  description: string | null;
}

export interface Comment {
  kind: 'comment';
  id: number;
  body: string;
  createdAt: string;
  updatedAt: string;
  issue: Issue;
}

export interface Issue {
  kind: 'issue';
  id: number;
  repo: Repo;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  stateReason: string | null;
  labels: Label[];
  comments: Comment[];
  createdAt: string;
  updatedAt: string;
  closedAt: string | null;
  /** The cached REST JSON text, dropped on any change. */
  json?: string;
}

export interface Pull {
  kind: 'pull';
  id: number;
  repo: Repo;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  head: string;
  base: string;
  /** Frozen once the pull request is merged or closed. */
  headSha: string | null;
  baseSha: string | null;
  mergeBaseSha: string | null;
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  createdAt: string;
  updatedAt: string;
  closedAt: string | null;
  comments: Comment[];
}

let nextId = 1;
export function newId(): number {
  nextId += 1;
  return 100000 + nextId;
}

export function nowIso(): string {
  return isoSeconds(Math.floor(Date.now() / 1000));
}

export class Repo {
  readonly kind = 'repo';
  readonly id: number;
  readonly owner: User;
  name: string;
  description: string | null;
  private: boolean;
  defaultBranch = 'main';
  readonly createdAt: string;
  updatedAt: string;
  pushedAt: string | null = null;
  readonly objects = new Map<string, GitObj>();
  readonly refs = new Map<string, string>();
  readonly labels = new Map<string, Label>();
  readonly issues = new Map<number, Issue>();
  readonly pulls = new Map<number, Pull>();
  nextNumber = 1;
  /** Fetch responses by request key; cleared whenever a ref moves. */
  readonly fetchCache = new Map<string, Buffer>();

  constructor(owner: User, name: string, description: string | null, isPrivate: boolean) {
    this.id = newId();
    this.owner = owner;
    this.name = name;
    this.description = description;
    this.private = isPrivate;
    this.createdAt = nowIso();
    this.updatedAt = this.createdAt;
  }

  get fullName(): string {
    return `${this.owner.login}/${this.name}`;
  }

  put(type: ObjType, data: Buffer, z?: Buffer): string {
    const oid = hashObject(type, data);
    if (!this.objects.has(oid)) this.objects.set(oid, { type, data, z });
    return oid;
  }

  get(oid: string): GitObj | undefined {
    return this.objects.get(oid);
  }

  setRef(name: string, oid: string | null): void {
    if (oid === null) this.refs.delete(name);
    else this.refs.set(name, oid);
    this.fetchCache.clear();
    this.pushedAt = nowIso();
  }

  branchTip(branch: string): string | undefined {
    return this.refs.get(`refs/heads/${branch}`);
  }

  commit(oid: string) {
    const obj = this.objects.get(oid);
    if (obj === undefined || obj.type !== 'commit') return null;
    return parseCommit(obj.data);
  }

  tree(oid: string): TreeEntry[] {
    const obj = this.objects.get(oid);
    if (obj === undefined || obj.type !== 'tree') return [];
    return parseTree(obj.data);
  }

  /** Follow tags to the object they name. */
  peel(oid: string): string {
    let current = oid;
    for (let i = 0; i < 10; i += 1) {
      const obj = this.objects.get(current);
      if (obj === undefined || obj.type !== 'tag') return current;
      current = parseTag(obj.data).object;
    }
    return current;
  }

  touch(): void {
    this.updatedAt = nowIso();
  }
}

// ------------------------------------------------------------------ the repository index

const repos = new Map<string, Repo>();

function key(owner: string, name: string): string {
  return `${owner.toLowerCase()}/${name.toLowerCase()}`;
}

export function findRepo(owner: string, name: string): Repo | undefined {
  return repos.get(key(owner, name));
}

export function createRepo(name: string, description: string | null, isPrivate: boolean): Repo | null {
  if (repos.has(key(viewer.login, name))) return null;
  const repo = new Repo(viewer, name, description, isPrivate);
  repos.set(key(viewer.login, name), repo);
  nodes.set(repo.id, repo);
  return repo;
}

export function deleteRepo(repo: Repo): void {
  repos.delete(key(repo.owner.login, repo.name));
  nodes.delete(repo.id);
}

export function listRepos(): Repo[] {
  return [...repos.values()];
}

/** Every object with a global id, by database id (repositories, issues, pulls, comments, labels). */
export const nodes = new Map<number, unknown>();

export function addIssue(repo: Repo, title: string, body: string, labels: Label[]): Issue {
  const now = nowIso();
  const issue: Issue = {
    kind: 'issue',
    id: newId(),
    repo,
    number: repo.nextNumber++,
    title,
    body,
    state: 'open',
    stateReason: null,
    labels,
    comments: [],
    createdAt: now,
    updatedAt: now,
    closedAt: null,
  };
  repo.issues.set(issue.number, issue);
  nodes.set(issue.id, issue);
  repo.touch();
  return issue;
}

export function setIssueState(issue: Issue, state: 'open' | 'closed'): void {
  if (issue.state === state) return;
  issue.state = state;
  issue.updatedAt = nowIso();
  issue.closedAt = state === 'closed' ? issue.updatedAt : null;
  issue.stateReason = state === 'closed' ? 'completed' : 'reopened';
  issue.json = undefined;
}

export function addComment(issue: Issue, body: string): Comment {
  const now = nowIso();
  const comment: Comment = { kind: 'comment', id: newId(), body, createdAt: now, updatedAt: now, issue };
  issue.comments.push(comment);
  issue.updatedAt = now;
  issue.json = undefined;
  nodes.set(comment.id, comment);
  return comment;
}

export function addLabel(repo: Repo, name: string, color: string, description: string | null): Label | null {
  if (repo.labels.has(name.toLowerCase())) return null;
  const label: Label = { kind: 'label', id: newId(), name, color, description };
  repo.labels.set(name.toLowerCase(), label);
  nodes.set(label.id, label);
  return label;
}

/** The labels named, or the first name that does not exist. */
export function resolveLabels(repo: Repo, names: readonly unknown[]): { labels: Label[] } | { missing: string } {
  const labels: Label[] = [];
  for (const raw of names) {
    const name = typeof raw === 'string' ? raw : typeof raw === 'object' && raw !== null && typeof (raw as { name?: unknown }).name === 'string' ? (raw as { name: string }).name : String(raw);
    const label = repo.labels.get(name.toLowerCase());
    if (label === undefined) return { missing: name };
    if (!labels.includes(label)) labels.push(label);
  }
  return { labels };
}

// ------------------------------------------------------------------ history

/** Is `ancestor` reachable from `descendant` (or equal to it)? */
export function isAncestor(repo: Repo, ancestor: string, descendant: string): boolean {
  if (ancestor === descendant) return true;
  const seen = new Set<string>([descendant]);
  const queue = [descendant];
  while (queue.length > 0) {
    const oid = queue.pop() as string;
    const commit = repo.commit(oid);
    if (commit === null) continue;
    for (const parent of commit.parents) {
      if (parent === ancestor) return true;
      if (!seen.has(parent)) {
        seen.add(parent);
        queue.push(parent);
      }
    }
  }
  return false;
}

export function ancestors(repo: Repo, tip: string): Set<string> {
  const seen = new Set<string>([tip]);
  const queue = [tip];
  while (queue.length > 0) {
    const commit = repo.commit(queue.pop() as string);
    if (commit === null) continue;
    for (const parent of commit.parents) {
      if (!seen.has(parent)) {
        seen.add(parent);
        queue.push(parent);
      }
    }
  }
  return seen;
}

export function mergeBase(repo: Repo, a: string, b: string): string | null {
  const fromA = ancestors(repo, a);
  const seen = new Set<string>([b]);
  const queue = [b];
  while (queue.length > 0) {
    const oid = queue.shift() as string;
    if (fromA.has(oid)) return oid;
    const commit = repo.commit(oid);
    if (commit === null) continue;
    for (const parent of commit.parents) {
      if (!seen.has(parent)) {
        seen.add(parent);
        queue.push(parent);
      }
    }
  }
  return null;
}

/** Commits reachable from `tip`, newest first (by committer time, then discovery order). */
export function logFrom(repo: Repo, tip: string, limit: number): string[] {
  const out: string[] = [];
  const seen = new Set<string>([tip]);
  const queue = [tip];
  while (queue.length > 0 && out.length < limit) {
    const oid = queue.shift() as string;
    const commit = repo.commit(oid);
    if (commit === null) continue;
    out.push(oid);
    for (const parent of commit.parents) {
      if (!seen.has(parent)) {
        seen.add(parent);
        queue.push(parent);
      }
    }
  }
  return out;
}

// ------------------------------------------------------------------ trees

export interface FlatEntry {
  oid: string;
  mode: string;
}

/** path -> blob for every file under a tree. */
export function flatten(repo: Repo, treeOid: string | null, prefix = '', out = new Map<string, FlatEntry>()): Map<string, FlatEntry> {
  if (treeOid === null) return out;
  for (const entry of repo.tree(treeOid)) {
    const path = `${prefix}${entry.name}`;
    if (entry.mode === '40000') flatten(repo, entry.oid, `${path}/`, out);
    else out.set(path, { oid: entry.oid, mode: entry.mode });
  }
  return out;
}

/** Write a tree (and its subtrees) holding exactly the files of `flat`. */
export function treeFromFlat(repo: Repo, flat: ReadonlyMap<string, FlatEntry>): string {
  const files: TreeEntry[] = [];
  const dirs = new Map<string, Map<string, FlatEntry>>();
  for (const [path, entry] of flat) {
    const slash = path.indexOf('/');
    if (slash < 0) {
      files.push({ mode: entry.mode, name: path, oid: entry.oid });
      continue;
    }
    const dir = path.slice(0, slash);
    let child = dirs.get(dir);
    if (child === undefined) {
      child = new Map();
      dirs.set(dir, child);
    }
    child.set(path.slice(slash + 1), entry);
  }
  for (const [name, child] of dirs) files.push({ mode: '40000', name, oid: treeFromFlat(repo, child) });
  return repo.put('tree', buildTree(files));
}

/** The entry at a path under a tree, or null. */
export function lookupPath(repo: Repo, treeOid: string, path: string): TreeEntry | null {
  const parts = path.split('/').filter((part) => part !== '');
  if (parts.length === 0) return { mode: '40000', name: '', oid: treeOid };
  let current = treeOid;
  for (let i = 0; i < parts.length; i += 1) {
    const entry = repo.tree(current).find((candidate) => candidate.name === parts[i]);
    if (entry === undefined) return null;
    if (i === parts.length - 1) return entry;
    if (entry.mode !== '40000') return null;
    current = entry.oid;
  }
  return null;
}

/** Set (blob) or remove (null) one path, returning the new root tree, or null when it becomes empty. */
function updateTree(repo: Repo, treeOid: string | null, parts: readonly string[], blob: string | null): string | null {
  const entries = treeOid === null ? [] : repo.tree(treeOid).slice();
  const [head, ...rest] = parts;
  const index = entries.findIndex((entry) => entry.name === head);
  const existing = index >= 0 ? entries[index] : undefined;
  let replacement: TreeEntry | null;
  if (rest.length === 0) {
    replacement = blob === null ? null : { mode: '100644', name: head as string, oid: blob };
  } else {
    const child = updateTree(repo, existing !== undefined && existing.mode === '40000' ? existing.oid : null, rest, blob);
    replacement = child === null ? null : { mode: '40000', name: head as string, oid: child };
  }
  if (index >= 0) entries.splice(index, 1);
  if (replacement !== null) entries.push(replacement);
  if (entries.length === 0) return null;
  return repo.put('tree', buildTree(entries));
}

const EMPTY_TREE = buildTree([]);

function identity(epochSec: number): string {
  return `${viewer.name} <${viewer.email}> ${epochSec} +0000`;
}

/** Make a commit on a branch that sets (or deletes, content null) one file. Answers the commit and blob. */
export function commitFile(repo: Repo, branch: string, path: string, content: Buffer | null, message: string): { commit: string; blob: string | null } {
  const tip = repo.branchTip(branch);
  const baseTree = tip === undefined ? null : (repo.commit(tip)?.tree ?? null);
  const blob = content === null ? null : repo.put('blob', content);
  const parts = path.split('/').filter((part) => part !== '');
  const tree = updateTree(repo, baseTree, parts, blob) ?? repo.put('tree', EMPTY_TREE);
  const commit = makeCommit(repo, tree, tip === undefined ? [] : [tip], message);
  repo.setRef(`refs/heads/${branch}`, commit);
  return { commit, blob };
}

let lastStamp = 0;
export function makeCommit(repo: Repo, tree: string, parents: readonly string[], message: string): string {
  // Strictly increasing seconds keep two otherwise identical commits apart.
  lastStamp = Math.max(lastStamp + 1, Math.floor(Date.now() / 1000));
  const stamp = identity(lastStamp);
  const text = message.endsWith('\n') ? message : `${message}\n`;
  return repo.put('commit', buildCommit({ tree, parents, author: stamp, committer: stamp, message: text }));
}

// ------------------------------------------------------------------ merges and diffs

/** Three-way merge of trees by path; null on a conflict. */
export function mergeTrees(repo: Repo, baseTree: string | null, ours: string | null, theirs: string | null): string | null {
  const base = flatten(repo, baseTree);
  const left = flatten(repo, ours);
  const right = flatten(repo, theirs);
  const out = new Map<string, FlatEntry>();
  const paths = new Set<string>([...base.keys(), ...left.keys(), ...right.keys()]);
  const same = (a: FlatEntry | undefined, b: FlatEntry | undefined): boolean => (a === undefined ? b === undefined : b !== undefined && a.oid === b.oid && a.mode === b.mode);
  for (const path of paths) {
    const b = base.get(path);
    const l = left.get(path);
    const r = right.get(path);
    let pick: FlatEntry | undefined;
    if (same(l, r)) pick = l;
    else if (same(b, r)) pick = l;
    else if (same(b, l)) pick = r;
    else return null;
    if (pick !== undefined) out.set(path, pick);
  }
  return treeFromFlat(repo, out);
}

export interface MergePlan {
  ok: boolean;
  tree: string | null;
  baseTip: string;
  headTip: string;
  mergeBase: string | null;
}

export function planMerge(repo: Repo, baseTip: string, headTip: string): MergePlan {
  const base = mergeBase(repo, baseTip, headTip);
  const treeOf = (oid: string | null): string | null => (oid === null ? null : (repo.commit(oid)?.tree ?? null));
  const tree = mergeTrees(repo, treeOf(base), treeOf(baseTip), treeOf(headTip));
  return { ok: tree !== null, tree, baseTip, headTip, mergeBase: base };
}

export interface FileChange {
  filename: string;
  status: 'added' | 'removed' | 'modified';
  additions: number;
  deletions: number;
  changes: number;
  sha: string;
}

function lines(repo: Repo, oid: string | undefined): string[] {
  if (oid === undefined) return [];
  const obj = repo.get(oid);
  if (obj === undefined) return [];
  const text = obj.data.toString('utf8');
  if (text === '') return [];
  return text.replace(/\n$/, '').split('\n');
}

function lineDelta(before: string[], after: string[]): { additions: number; deletions: number } {
  const pool = new Map<string, number>();
  for (const line of before) pool.set(line, (pool.get(line) ?? 0) + 1);
  let additions = 0;
  for (const line of after) {
    const left = pool.get(line) ?? 0;
    if (left > 0) pool.set(line, left - 1);
    else additions += 1;
  }
  let deletions = 0;
  for (const left of pool.values()) deletions += left;
  return { additions, deletions };
}

/** The files that differ between two commits' trees. */
export function diffCommits(repo: Repo, from: string | null, to: string): FileChange[] {
  const before = flatten(repo, from === null ? null : (repo.commit(from)?.tree ?? null));
  const after = flatten(repo, repo.commit(to)?.tree ?? null);
  const changes: FileChange[] = [];
  const paths = [...new Set([...before.keys(), ...after.keys()])].sort();
  for (const path of paths) {
    const b = before.get(path);
    const a = after.get(path);
    if (b !== undefined && a !== undefined && b.oid === a.oid) continue;
    const delta = lineDelta(lines(repo, b?.oid), lines(repo, a?.oid));
    changes.push({
      filename: path,
      status: b === undefined ? 'added' : a === undefined ? 'removed' : 'modified',
      additions: delta.additions,
      deletions: delta.deletions,
      changes: delta.additions + delta.deletions,
      sha: (a ?? b)?.oid ?? '',
    });
  }
  return changes;
}

// ------------------------------------------------------------------ pull requests

export function addPull(repo: Repo, title: string, body: string, head: string, base: string): Pull {
  const now = nowIso();
  const pull: Pull = {
    kind: 'pull',
    id: newId(),
    repo,
    number: repo.nextNumber++,
    title,
    body,
    state: 'open',
    head,
    base,
    headSha: null,
    baseSha: null,
    mergeBaseSha: null,
    merged: false,
    mergedAt: null,
    mergeCommitSha: null,
    createdAt: now,
    updatedAt: now,
    closedAt: null,
    comments: [],
  };
  repo.pulls.set(pull.number, pull);
  nodes.set(pull.id, pull);
  repo.touch();
  return pull;
}

export function pullHeadSha(pull: Pull): string | null {
  return pull.headSha ?? pull.repo.branchTip(pull.head) ?? null;
}

export function pullBaseSha(pull: Pull): string | null {
  return pull.baseSha ?? pull.repo.branchTip(pull.base) ?? null;
}

export function pullFiles(pull: Pull): FileChange[] {
  const head = pullHeadSha(pull);
  if (head === null) return [];
  let from = pull.mergeBaseSha;
  if (from === null) {
    const base = pullBaseSha(pull);
    from = base === null ? null : mergeBase(pull.repo, base, head);
  }
  return diffCommits(pull.repo, from, head);
}

const mergeableCache = new Map<string, boolean>();
export function pullMergeable(pull: Pull): boolean | null {
  if (pull.merged) return null;
  const head = pullHeadSha(pull);
  const base = pullBaseSha(pull);
  if (head === null || base === null) return false;
  const cacheKey = `${pull.repo.id}:${base}:${head}`;
  let ok = mergeableCache.get(cacheKey);
  if (ok === undefined) {
    ok = planMerge(pull.repo, base, head).ok;
    mergeableCache.set(cacheKey, ok);
  }
  return ok;
}

export type MergeResult = { ok: true; sha: string } | { ok: false; status: number; message: string };

/** Squash-merge (or merge-commit) a pull request onto its base branch. */
export function mergePull(pull: Pull, method: string, commitTitle?: string, commitMessage?: string): MergeResult {
  if (pull.merged) return { ok: false, status: 405, message: 'Pull Request is not mergeable: it is already merged' };
  if (pull.state !== 'open') return { ok: false, status: 405, message: 'Pull Request is not mergeable: it is closed' };
  const repo = pull.repo;
  const head = repo.branchTip(pull.head);
  const base = repo.branchTip(pull.base);
  if (head === undefined || base === undefined) return { ok: false, status: 405, message: 'Pull Request is not mergeable: a branch is missing' };
  const plan = planMerge(repo, base, head);
  if (!plan.ok || plan.tree === null) return { ok: false, status: 405, message: 'Pull Request is not mergeable' };
  const title = commitTitle !== undefined && commitTitle !== '' ? commitTitle : `${pull.title} (#${pull.number})`;
  const message = commitMessage !== undefined && commitMessage !== '' ? `${title}\n\n${commitMessage}` : title;
  const parents = method === 'merge' ? [base, head] : [base];
  const sha = makeCommit(repo, plan.tree, parents, message);
  repo.setRef(`refs/heads/${pull.base}`, sha);
  const now = nowIso();
  pull.headSha = head;
  pull.baseSha = base;
  pull.mergeBaseSha = plan.mergeBase;
  pull.merged = true;
  pull.mergedAt = now;
  pull.mergeCommitSha = sha;
  pull.state = 'closed';
  pull.closedAt = now;
  pull.updatedAt = now;
  return { ok: true, sha };
}

export function setPullState(pull: Pull, state: 'open' | 'closed'): void {
  if (pull.merged || pull.state === state) return;
  pull.state = state;
  pull.updatedAt = nowIso();
  if (state === 'closed') {
    pull.closedAt = pull.updatedAt;
    pull.headSha = pullHeadSha(pull);
    pull.baseSha = pullBaseSha(pull);
  } else {
    pull.closedAt = null;
    pull.headSha = null;
    pull.baseSha = null;
  }
}

/** An opaque, stable global id: type prefix + base64url of the database id. */
export function globalId(prefix: string, id: number): string {
  const digest = createHash('sha1').update(`forge:${prefix}:${id}`).digest('base64url').slice(0, 6);
  return `${prefix}_${Buffer.from(`${id}:${digest}`).toString('base64url')}`;
}

export function fromGlobalId(id: unknown): unknown {
  if (typeof id !== 'string') return undefined;
  const under = id.indexOf('_');
  if (under < 0) return undefined;
  const decoded = Buffer.from(id.slice(under + 1), 'base64url').toString('utf8');
  const number = Number(decoded.split(':')[0]);
  if (!Number.isSafeInteger(number)) return undefined;
  const found = nodes.get(number);
  if (found === undefined) return undefined;
  return globalIdOf(found) === id ? found : undefined;
}

export function globalIdOf(value: unknown): string {
  const obj = value as { kind?: string; id: number };
  switch (obj.kind) {
    case 'repo':
      return globalId('R', obj.id);
    case 'issue':
      return globalId('I', obj.id);
    case 'pull':
      return globalId('PR', obj.id);
    case 'comment':
      return globalId('IC', obj.id);
    case 'label':
      return globalId('LA', obj.id);
    case 'user':
      return globalId('U', obj.id);
    default:
      return globalId('X', obj.id);
  }
}
