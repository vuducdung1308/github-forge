// GitHub GraphQL v4 at /api/graphql: a hand-written parser and executor over the documents gh and
// github-mcp-server send. Unknown fields are reported in `errors` (HTTP 200); introspection answers
// __type and __schema; a JSON array body is operation batching.

import { parseCommit, parseIdent } from './git.ts';
import { HttpError, openPull, sortIssues } from './rest.ts';
import {
  type Comment,
  type Issue,
  type Label,
  type Pull,
  type Repo,
  addComment,
  addIssue,
  findRepo,
  fromGlobalId,
  globalIdOf,
  listRepos,
  mergePull,
  pullBaseSha,
  pullFiles,
  pullHeadSha,
  pullMergeable,
  setIssueState,
  setPullState,
  viewer,
} from './store.ts';

// ------------------------------------------------------------------ lexer and parser

type Tok = { kind: 'punct' | 'name' | 'int' | 'float' | 'string' | 'eof'; value: string; pos: number };

class GraphQLSyntaxError extends Error {}

function lex(source: string): Tok[] {
  const tokens: Tok[] = [];
  let i = 0;
  while (i < source.length) {
    const ch = source[i] as string;
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',' || ch === '﻿') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < source.length && source[i] !== '\n') i += 1;
      continue;
    }
    if (ch === '.' && source.startsWith('...', i)) {
      tokens.push({ kind: 'punct', value: '...', pos: i });
      i += 3;
      continue;
    }
    if ('!$&()+:=@[]{}|'.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch, pos: i });
      i += 1;
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      const start = i;
      while (i < source.length && /[A-Za-z0-9_]/.test(source[i] as string)) i += 1;
      tokens.push({ kind: 'name', value: source.slice(start, i), pos: start });
      continue;
    }
    if (/[-0-9]/.test(ch)) {
      const match = /^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?/.exec(source.slice(i));
      if (match === null) throw new GraphQLSyntaxError(`Unexpected character ${JSON.stringify(ch)}`);
      tokens.push({ kind: match[2] !== undefined || match[3] !== undefined ? 'float' : 'int', value: match[0], pos: i });
      i += match[0].length;
      continue;
    }
    if (ch === '"') {
      if (source.startsWith('"""', i)) {
        const end = source.indexOf('"""', i + 3);
        if (end < 0) throw new GraphQLSyntaxError('Unterminated block string');
        tokens.push({ kind: 'string', value: blockString(source.slice(i + 3, end)), pos: i });
        i = end + 3;
        continue;
      }
      let j = i + 1;
      let out = '';
      for (;;) {
        const c = source[j];
        if (c === undefined || c === '\n') throw new GraphQLSyntaxError('Unterminated string');
        if (c === '"') break;
        if (c === '\\') {
          const e = source[j + 1];
          if (e === 'u') {
            out += String.fromCharCode(Number.parseInt(source.slice(j + 2, j + 6), 16));
            j += 6;
            continue;
          }
          out += ({ n: '\n', t: '\t', r: '\r', b: '\b', f: '\f' } as Record<string, string>)[e ?? ''] ?? e ?? '';
          j += 2;
          continue;
        }
        out += c;
        j += 1;
      }
      tokens.push({ kind: 'string', value: out, pos: i });
      i = j + 1;
      continue;
    }
    throw new GraphQLSyntaxError(`Unexpected character ${JSON.stringify(ch)}`);
  }
  tokens.push({ kind: 'eof', value: '', pos: source.length });
  return tokens;
}

function blockString(raw: string): string {
  const lines = raw.replace(/\\"""/g, '"""').split(/\r\n|\n|\r/);
  let indent = Infinity;
  for (const line of lines.slice(1)) {
    const lead = /^[ \t]*/.exec(line)?.[0].length ?? 0;
    if (lead < line.length) indent = Math.min(indent, lead);
  }
  const out = lines.map((line, index) => (index === 0 || indent === Infinity ? line : line.slice(indent)));
  while (out.length > 0 && (out[0] ?? '').trim() === '') out.shift();
  while (out.length > 0 && (out[out.length - 1] ?? '').trim() === '') out.pop();
  return out.join('\n');
}

type Value =
  | { kind: 'var'; name: string }
  | { kind: 'int' | 'float' | 'string' | 'enum'; value: string }
  | { kind: 'bool'; value: boolean }
  | { kind: 'null' }
  | { kind: 'list'; values: Value[] }
  | { kind: 'object'; fields: Array<[string, Value]> };

interface Directive {
  name: string;
  args: Array<[string, Value]>;
}

type Selection =
  | { kind: 'field'; alias: string | null; name: string; args: Array<[string, Value]>; directives: Directive[]; selections: Selection[] | null }
  | { kind: 'inline'; on: string | null; directives: Directive[]; selections: Selection[] }
  | { kind: 'spread'; name: string; directives: Directive[] };

interface Operation {
  type: 'query' | 'mutation' | 'subscription';
  name: string | null;
  vars: Array<{ name: string; type: string; default: Value | null }>;
  selections: Selection[];
}

interface Document {
  operations: Operation[];
  fragments: Map<string, { on: string; selections: Selection[] }>;
}

class Parser {
  private i = 0;
  private readonly t: Tok[];
  constructor(tokens: Tok[]) {
    this.t = tokens;
  }

  private peek(): Tok {
    return this.t[this.i] as Tok;
  }

  private next(): Tok {
    return this.t[this.i++] as Tok;
  }

  private is(value: string): boolean {
    const tok = this.peek();
    return tok.kind === 'punct' && tok.value === value;
  }

  private expect(value: string): void {
    const tok = this.next();
    if (tok.kind !== 'punct' || tok.value !== value) throw new GraphQLSyntaxError(`Expected ${value}, found ${tok.value === '' ? '<EOF>' : tok.value}`);
  }

  private name(): string {
    const tok = this.next();
    if (tok.kind !== 'name') throw new GraphQLSyntaxError(`Expected Name, found ${tok.value === '' ? '<EOF>' : tok.value}`);
    return tok.value;
  }

  document(): Document {
    const doc: Document = { operations: [], fragments: new Map() };
    while (this.peek().kind !== 'eof') {
      if (this.is('{')) {
        doc.operations.push({ type: 'query', name: null, vars: [], selections: this.selectionSet() });
        continue;
      }
      const word = this.name();
      if (word === 'fragment') {
        const name = this.name();
        if (this.name() !== 'on') throw new GraphQLSyntaxError('Expected "on"');
        const on = this.name();
        this.directives();
        doc.fragments.set(name, { on, selections: this.selectionSet() });
        continue;
      }
      if (word !== 'query' && word !== 'mutation' && word !== 'subscription') throw new GraphQLSyntaxError(`Unexpected Name "${word}"`);
      const op: Operation = { type: word, name: null, vars: [], selections: [] };
      if (this.peek().kind === 'name') op.name = this.name();
      if (this.is('(')) {
        this.next();
        while (!this.is(')')) {
          this.expect('$');
          const name = this.name();
          this.expect(':');
          const type = this.typeRef();
          let def: Value | null = null;
          if (this.is('=')) {
            this.next();
            def = this.value(true);
          }
          this.directives();
          op.vars.push({ name, type, default: def });
        }
        this.next();
      }
      this.directives();
      op.selections = this.selectionSet();
      doc.operations.push(op);
    }
    return doc;
  }

  private typeRef(): string {
    let out: string;
    if (this.is('[')) {
      this.next();
      out = `[${this.typeRef()}]`;
      this.expect(']');
    } else {
      out = this.name();
    }
    if (this.is('!')) {
      this.next();
      out += '!';
    }
    return out;
  }

  private directives(): Directive[] {
    const out: Directive[] = [];
    while (this.is('@')) {
      this.next();
      out.push({ name: this.name(), args: this.is('(') ? this.args() : [] });
    }
    return out;
  }

  private args(): Array<[string, Value]> {
    this.expect('(');
    const out: Array<[string, Value]> = [];
    while (!this.is(')')) {
      const name = this.name();
      this.expect(':');
      out.push([name, this.value(false)]);
    }
    this.next();
    return out;
  }

  private value(constant: boolean): Value {
    const tok = this.peek();
    if (tok.kind === 'punct') {
      if (tok.value === '$' && !constant) {
        this.next();
        return { kind: 'var', name: this.name() };
      }
      if (tok.value === '[') {
        this.next();
        const values: Value[] = [];
        while (!this.is(']')) values.push(this.value(constant));
        this.next();
        return { kind: 'list', values };
      }
      if (tok.value === '{') {
        this.next();
        const fields: Array<[string, Value]> = [];
        while (!this.is('}')) {
          const name = this.name();
          this.expect(':');
          fields.push([name, this.value(constant)]);
        }
        this.next();
        return { kind: 'object', fields };
      }
      throw new GraphQLSyntaxError(`Unexpected ${tok.value}`);
    }
    this.next();
    if (tok.kind === 'int' || tok.kind === 'float' || tok.kind === 'string') return { kind: tok.kind, value: tok.value };
    if (tok.kind === 'name') {
      if (tok.value === 'true' || tok.value === 'false') return { kind: 'bool', value: tok.value === 'true' };
      if (tok.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: tok.value };
    }
    throw new GraphQLSyntaxError('Unexpected <EOF>');
  }

  private selectionSet(): Selection[] {
    this.expect('{');
    const out: Selection[] = [];
    while (!this.is('}')) {
      if (this.is('...')) {
        this.next();
        if (this.peek().kind === 'name' && this.peek().value !== 'on') {
          const name = this.name();
          out.push({ kind: 'spread', name, directives: this.directives() });
          continue;
        }
        let on: string | null = null;
        if (this.peek().kind === 'name' && this.peek().value === 'on') {
          this.next();
          on = this.name();
        }
        const directives = this.directives();
        out.push({ kind: 'inline', on, directives, selections: this.selectionSet() });
        continue;
      }
      let name = this.name();
      let alias: string | null = null;
      if (this.is(':')) {
        this.next();
        alias = name;
        name = this.name();
      }
      const args = this.is('(') ? this.args() : [];
      const directives = this.directives();
      const selections = this.is('{') ? this.selectionSet() : null;
      out.push({ kind: 'field', alias, name, args, directives, selections });
    }
    this.next();
    return out;
  }
}

// ------------------------------------------------------------------ schema

interface Ctx {
  base: string;
}

interface FieldDef {
  type: string;
  args?: Record<string, string>;
  description?: string;
  resolve: (parent: any, args: Record<string, any>, ctx: Ctx) => unknown;
}

interface TypeDef {
  kind: 'OBJECT' | 'INTERFACE' | 'ENUM' | 'SCALAR' | 'INPUT_OBJECT' | 'UNION';
  description?: string;
  fields?: Record<string, FieldDef>;
  enumValues?: string[];
  interfaces?: string[];
  possibleTypes?: string[];
  inputFields?: Record<string, string>;
}

class FieldError extends Error {
  readonly type: string;
  constructor(message: string, type = 'UNPROCESSABLE') {
    super(message);
    this.type = type;
  }
}

const prop = (type: string, key: string): FieldDef => ({ type, resolve: (parent) => parent[key] });

function connection<T>(items: readonly T[], args: Record<string, any>): Record<string, unknown> {
  let start = 0;
  let end = items.length;
  const decode = (cursor: unknown): number | null => {
    if (typeof cursor !== 'string') return null;
    const match = /^cursor:v2:(\d+)$/.exec(Buffer.from(cursor, 'base64').toString('utf8'));
    return match === null ? null : Number(match[1]);
  };
  const after = decode(args.after);
  if (after !== null) start = Math.max(start, after + 1);
  const before = decode(args.before);
  if (before !== null) end = Math.min(end, before);
  if (typeof args.first === 'number') {
    if (args.first < 0) throw new FieldError('`first` must be non-negative', 'INVALID_ARGUMENT');
    end = Math.min(end, start + args.first);
  }
  if (typeof args.last === 'number') start = Math.max(start, end - args.last);
  const cursor = (index: number): string => Buffer.from(`cursor:v2:${index}`).toString('base64');
  const slice = items.slice(start, end);
  const edges = slice.map((node, index) => ({ cursor: cursor(start + index), node }));
  return {
    nodes: slice,
    edges,
    totalCount: items.length,
    pageInfo: {
      hasNextPage: end < items.length,
      hasPreviousPage: start > 0,
      startCursor: slice.length > 0 ? cursor(start) : null,
      endCursor: slice.length > 0 ? cursor(end - 1) : null,
    },
  };
}

const CONN_ARGS = { first: 'Int', after: 'String', last: 'Int', before: 'String' };

function connectionTypes(node: string): Record<string, TypeDef> {
  return {
    [`${node}Connection`]: {
      kind: 'OBJECT',
      fields: {
        nodes: prop(`[${node}]`, 'nodes'),
        edges: prop(`[${node}Edge]`, 'edges'),
        totalCount: prop('Int!', 'totalCount'),
        pageInfo: prop('PageInfo!', 'pageInfo'),
      },
    },
    [`${node}Edge`]: { kind: 'OBJECT', fields: { cursor: prop('String!', 'cursor'), node: prop(node, 'node') } },
  };
}

function issueState(issue: Issue): string {
  return issue.state === 'open' ? 'OPEN' : 'CLOSED';
}

function pullState(pull: Pull): string {
  return pull.merged ? 'MERGED' : pull.state === 'open' ? 'OPEN' : 'CLOSED';
}

function commitNode(repo: Repo, oid: string | null): Record<string, unknown> | null {
  if (oid === null) return null;
  const commit = repo.commit(oid);
  if (commit === null) return null;
  const author = parseIdent(commit.author);
  const committer = parseIdent(commit.committer);
  const message = commit.message.replace(/\n$/, '');
  return {
    __typename: 'Commit',
    id: `C_${oid}`,
    oid,
    abbreviatedOid: oid.slice(0, 7),
    message,
    messageHeadline: message.split('\n')[0] ?? '',
    messageBody: message.split('\n').slice(2).join('\n'),
    committedDate: committer.date,
    authoredDate: author.date,
    url: `/${repo.fullName}/commit/${oid}`,
    tree: { oid: commit.tree },
    author: { name: author.name, email: author.email, date: author.date },
    committer: { name: committer.name, email: committer.email, date: committer.date },
  };
}

function refNode(repo: Repo, name: string): Record<string, unknown> | null {
  const oid = repo.refs.get(name);
  if (oid === undefined) return null;
  const slash = name.lastIndexOf('/', name.startsWith('refs/heads/') ? 10 : name.startsWith('refs/tags/') ? 9 : name.length);
  const prefix = name.startsWith('refs/heads/') ? 'refs/heads/' : name.startsWith('refs/tags/') ? 'refs/tags/' : name.slice(0, slash + 1);
  return { __typename: 'Ref', id: `REF_${Buffer.from(`${repo.id}:${name}`).toString('base64url')}`, name: name.slice(prefix.length), prefix, target: commitNode(repo, repo.peel(oid)), repository: repo };
}

function orderIssues<T extends { number: number; updatedAt: string; comments: unknown[] }>(items: T[], orderBy: any): T[] {
  if (orderBy === null || orderBy === undefined) return items.sort((a, b) => a.number - b.number);
  const field = String(orderBy.field ?? 'CREATED_AT');
  const direction = String(orderBy.direction ?? 'ASC') === 'DESC' ? 'desc' : 'asc';
  return sortIssues(items, field === 'UPDATED_AT' ? 'updated' : field === 'COMMENTS' ? 'comments' : 'created', direction);
}

function labelsMatch(labels: Label[], wanted: unknown): boolean {
  if (!Array.isArray(wanted) || wanted.length === 0) return true;
  return wanted.every((name) => labels.some((label) => label.name.toLowerCase() === String(name).toLowerCase()));
}

const repoFields: Record<string, FieldDef> = {
  id: { type: 'ID!', resolve: (r: Repo) => globalIdOf(r) },
  databaseId: prop('Int', 'id'),
  name: prop('String!', 'name'),
  nameWithOwner: { type: 'String!', resolve: (r: Repo) => r.fullName },
  owner: { type: 'User!', resolve: (r: Repo) => r.owner },
  description: prop('String', 'description'),
  url: { type: 'URI!', resolve: (r: Repo, _a, ctx) => `${ctx.base}/${r.fullName}` },
  sshUrl: { type: 'String!', resolve: (r: Repo, _a, ctx) => `${ctx.base}/${r.fullName}.git` },
  isPrivate: prop('Boolean!', 'private'),
  visibility: { type: 'RepositoryVisibility!', resolve: (r: Repo) => (r.private ? 'PRIVATE' : 'PUBLIC') },
  isFork: { type: 'Boolean!', resolve: () => false },
  isArchived: { type: 'Boolean!', resolve: () => false },
  isEmpty: { type: 'Boolean!', resolve: (r: Repo) => r.refs.size === 0 },
  isTemplate: { type: 'Boolean!', resolve: () => false },
  createdAt: prop('DateTime!', 'createdAt'),
  updatedAt: prop('DateTime!', 'updatedAt'),
  pushedAt: prop('DateTime', 'pushedAt'),
  stargazerCount: { type: 'Int!', resolve: () => 0 },
  forkCount: { type: 'Int!', resolve: () => 0 },
  viewerPermission: { type: 'RepositoryPermission', resolve: () => 'ADMIN' },
  viewerCanAdminister: { type: 'Boolean!', resolve: () => true },
  hasIssuesEnabled: { type: 'Boolean!', resolve: () => true },
  hasWikiEnabled: { type: 'Boolean!', resolve: () => false },
  hasProjectsEnabled: { type: 'Boolean!', resolve: () => false },
  mergeCommitAllowed: { type: 'Boolean!', resolve: () => true },
  squashMergeAllowed: { type: 'Boolean!', resolve: () => true },
  rebaseMergeAllowed: { type: 'Boolean!', resolve: () => true },
  defaultBranchRef: { type: 'Ref', resolve: (r: Repo) => refNode(r, `refs/heads/${r.defaultBranch}`) },
  ref: { type: 'Ref', args: { qualifiedName: 'String!' }, resolve: (r: Repo, a) => refNode(r, String(a.qualifiedName).startsWith('refs/') ? String(a.qualifiedName) : `refs/heads/${a.qualifiedName}`) },
  refs: {
    type: 'RefConnection',
    args: { refPrefix: 'String!', ...CONN_ARGS },
    resolve: (r: Repo, a) => connection([...r.refs.keys()].filter((name) => name.startsWith(String(a.refPrefix ?? 'refs/'))).sort().map((name) => refNode(r, name)), a),
  },
  issue: { type: 'Issue', args: { number: 'Int!' }, resolve: (r: Repo, a) => r.issues.get(Number(a.number)) ?? null },
  issues: {
    type: 'IssueConnection!',
    args: { ...CONN_ARGS, states: '[IssueState!]', orderBy: 'IssueOrder', labels: '[String!]', filterBy: 'IssueFilters' },
    resolve: (r: Repo, a) => {
      const states = Array.isArray(a.states) && a.states.length > 0 ? a.states.map(String) : null;
      const filterStates = a.filterBy?.states;
      let items = [...r.issues.values()].filter((issue) => (states === null || states.includes(issueState(issue))) && labelsMatch(issue.labels, a.labels));
      if (Array.isArray(filterStates) && filterStates.length > 0) items = items.filter((issue) => filterStates.includes(issueState(issue)));
      return connection(orderIssues(items, a.orderBy), a);
    },
  },
  pullRequest: { type: 'PullRequest', args: { number: 'Int!' }, resolve: (r: Repo, a) => r.pulls.get(Number(a.number)) ?? null },
  pullRequests: {
    type: 'PullRequestConnection!',
    args: { ...CONN_ARGS, states: '[PullRequestState!]', orderBy: 'IssueOrder', headRefName: 'String', baseRefName: 'String', labels: '[String!]' },
    resolve: (r: Repo, a) => {
      const states = Array.isArray(a.states) && a.states.length > 0 ? a.states.map(String) : null;
      const items = [...r.pulls.values()].filter(
        (pull) =>
          (states === null || states.includes(pullState(pull))) &&
          (typeof a.headRefName !== 'string' || pull.head === a.headRefName) &&
          (typeof a.baseRefName !== 'string' || pull.base === a.baseRefName),
      );
      return connection(orderIssues(items, a.orderBy), a);
    },
  },
  issueOrPullRequest: { type: 'IssueOrPullRequest', args: { number: 'Int!' }, resolve: (r: Repo, a) => r.issues.get(Number(a.number)) ?? r.pulls.get(Number(a.number)) ?? null },
  labels: { type: 'LabelConnection', args: { ...CONN_ARGS, query: 'String' }, resolve: (r: Repo, a) => connection([...r.labels.values()], a) },
  label: { type: 'Label', args: { name: 'String!' }, resolve: (r: Repo, a) => r.labels.get(String(a.name).toLowerCase()) ?? null },
  assignableUsers: { type: 'UserConnection!', args: CONN_ARGS, resolve: (_r, a) => connection([viewer], a) },
};

const commentFields = (subject: 'Issue' | 'PullRequest'): Record<string, FieldDef> => ({
  comments: { type: 'IssueCommentConnection!', args: CONN_ARGS, resolve: (i: Issue | Pull, a) => connection(i.comments, a) },
  labels: { type: 'LabelConnection', args: CONN_ARGS, resolve: (i: any, a) => connection(i.labels ?? [], a) },
  assignees: { type: 'UserConnection!', args: CONN_ARGS, resolve: (_i, a) => connection([], a) },
  author: { type: 'User', resolve: () => viewer },
  repository: { type: 'Repository!', resolve: (i: Issue | Pull) => i.repo },
  id: { type: 'ID!', resolve: (i: Issue | Pull) => globalIdOf(i) },
  databaseId: prop('Int', 'id'),
  number: prop('Int!', 'number'),
  title: prop('String!', 'title'),
  body: prop('String!', 'body'),
  bodyText: prop('String!', 'body'),
  createdAt: prop('DateTime!', 'createdAt'),
  updatedAt: prop('DateTime!', 'updatedAt'),
  closedAt: prop('DateTime', 'closedAt'),
  closed: { type: 'Boolean!', resolve: (i: Issue | Pull) => i.state === 'closed' },
  locked: { type: 'Boolean!', resolve: () => false },
  viewerDidAuthor: { type: 'Boolean!', resolve: () => true },
  url: { type: 'URI!', resolve: (i: Issue | Pull, _a, ctx) => `${ctx.base}/${i.repo.fullName}/${subject === 'Issue' ? 'issues' : 'pull'}/${i.number}` },
});

const TYPES: Record<string, TypeDef> = {
  Query: {
    kind: 'OBJECT',
    fields: {
      viewer: { type: 'User!', resolve: () => viewer },
      user: { type: 'User', args: { login: 'String!' }, resolve: (_p, a) => (String(a.login).toLowerCase() === viewer.login.toLowerCase() ? viewer : null) },
      repositoryOwner: { type: 'User', args: { login: 'String!' }, resolve: (_p, a) => (String(a.login).toLowerCase() === viewer.login.toLowerCase() ? viewer : null) },
      repository: {
        type: 'Repository',
        args: { owner: 'String!', name: 'String!', followRenames: 'Boolean' },
        resolve: (_p, a) => {
          const repo = findRepo(String(a.owner), String(a.name));
          return repo !== undefined && repo.owner.login.toLowerCase() === String(a.owner).toLowerCase() ? repo : null;
        },
      },
      node: { type: 'Node', args: { id: 'ID!' }, resolve: (_p, a) => fromGlobalId(a.id) ?? null },
      nodes: { type: '[Node]!', args: { ids: '[ID!]!' }, resolve: (_p, a) => (Array.isArray(a.ids) ? a.ids.map((id: unknown) => fromGlobalId(id) ?? null) : []) },
      rateLimit: { type: 'RateLimit', resolve: () => ({ limit: 5000, cost: 1, remaining: 4999, resetAt: '2030-01-01T00:00:00Z', used: 1 }) },
    },
  },
  Mutation: { kind: 'OBJECT', fields: {} },
  Node: { kind: 'INTERFACE', fields: { id: { type: 'ID!', resolve: (n) => globalIdOf(n) } }, possibleTypes: ['Repository', 'Issue', 'PullRequest', 'IssueComment', 'Label', 'User'] },
  IssueOrPullRequest: { kind: 'UNION', possibleTypes: ['Issue', 'PullRequest'] },
  User: {
    kind: 'OBJECT',
    interfaces: ['Node', 'Actor', 'RepositoryOwner'],
    fields: {
      id: { type: 'ID!', resolve: (u) => globalIdOf(u) },
      databaseId: prop('Int', 'id'),
      login: prop('String!', 'login'),
      name: prop('String', 'name'),
      email: prop('String!', 'email'),
      url: { type: 'URI!', resolve: (u, _a, ctx) => `${ctx.base}/${u.login}` },
      avatarUrl: { type: 'URI!', args: { size: 'Int' }, resolve: (u, _a, ctx) => `${ctx.base}/avatars/${u.login}` },
      repositories: { type: 'RepositoryConnection!', args: { ...CONN_ARGS, privacy: 'RepositoryPrivacy' }, resolve: (_u, a) => connection(listRepos().sort((x, y) => x.id - y.id), a) },
      repository: { type: 'Repository', args: { name: 'String!' }, resolve: (u, a) => findRepo(u.login, String(a.name)) ?? null },
    },
  },
  Repository: { kind: 'OBJECT', interfaces: ['Node'], fields: repoFields },
  Ref: {
    kind: 'OBJECT',
    interfaces: ['Node'],
    fields: { id: prop('ID!', 'id'), name: prop('String!', 'name'), prefix: prop('String!', 'prefix'), target: prop('Commit', 'target'), repository: prop('Repository!', 'repository') },
  },
  Commit: {
    kind: 'OBJECT',
    interfaces: ['Node'],
    fields: {
      id: prop('ID!', 'id'),
      oid: prop('GitObjectID!', 'oid'),
      abbreviatedOid: prop('String!', 'abbreviatedOid'),
      message: prop('String!', 'message'),
      messageHeadline: prop('String!', 'messageHeadline'),
      messageBody: prop('String!', 'messageBody'),
      committedDate: prop('DateTime!', 'committedDate'),
      authoredDate: prop('DateTime!', 'authoredDate'),
      url: prop('URI!', 'url'),
      tree: prop('Tree!', 'tree'),
      author: prop('GitActor', 'author'),
      committer: prop('GitActor', 'committer'),
    },
  },
  Tree: { kind: 'OBJECT', fields: { oid: prop('GitObjectID!', 'oid') } },
  GitActor: { kind: 'OBJECT', fields: { name: prop('String', 'name'), email: prop('String', 'email'), date: prop('GitTimestamp', 'date') } },
  Issue: {
    kind: 'OBJECT',
    interfaces: ['Node'],
    fields: {
      ...commentFields('Issue'),
      state: { type: 'IssueState!', resolve: (i: Issue) => issueState(i) },
      stateReason: { type: 'IssueStateReason', resolve: (i: Issue) => (i.stateReason === null ? null : i.stateReason.toUpperCase()) },
    },
  },
  PullRequest: {
    kind: 'OBJECT',
    interfaces: ['Node'],
    fields: {
      ...commentFields('PullRequest'),
      state: { type: 'PullRequestState!', resolve: (p: Pull) => pullState(p) },
      merged: prop('Boolean!', 'merged'),
      mergedAt: prop('DateTime', 'mergedAt'),
      isDraft: { type: 'Boolean!', resolve: () => false },
      headRefName: prop('String!', 'head'),
      baseRefName: prop('String!', 'base'),
      headRefOid: { type: 'GitObjectID!', resolve: (p: Pull) => pullHeadSha(p) },
      baseRefOid: { type: 'GitObjectID!', resolve: (p: Pull) => pullBaseSha(p) },
      headRepository: { type: 'Repository', resolve: (p: Pull) => p.repo },
      baseRepository: { type: 'Repository', resolve: (p: Pull) => p.repo },
      headRef: { type: 'Ref', resolve: (p: Pull) => refNode(p.repo, `refs/heads/${p.head}`) },
      baseRef: { type: 'Ref', resolve: (p: Pull) => refNode(p.repo, `refs/heads/${p.base}`) },
      mergeable: { type: 'MergeableState!', resolve: (p: Pull) => (p.merged ? 'UNKNOWN' : pullMergeable(p) === true ? 'MERGEABLE' : 'CONFLICTING') },
      mergeCommit: { type: 'Commit', resolve: (p: Pull) => commitNode(p.repo, p.mergeCommitSha) },
      additions: { type: 'Int!', resolve: (p: Pull) => pullFiles(p).reduce((sum, file) => sum + file.additions, 0) },
      deletions: { type: 'Int!', resolve: (p: Pull) => pullFiles(p).reduce((sum, file) => sum + file.deletions, 0) },
      changedFiles: { type: 'Int!', resolve: (p: Pull) => pullFiles(p).length },
      files: {
        type: 'PullRequestChangedFileConnection',
        args: CONN_ARGS,
        resolve: (p: Pull, a) => connection(pullFiles(p).map((file) => ({ path: file.filename, additions: file.additions, deletions: file.deletions, changeType: file.status === 'added' ? 'ADDED' : file.status === 'removed' ? 'DELETED' : 'MODIFIED' })), a),
      },
      commits: { type: 'PullRequestCommitConnection!', args: CONN_ARGS, resolve: (p: Pull, a) => connection([{ commit: commitNode(p.repo, pullHeadSha(p)) }], a) },
    },
  },
  PullRequestChangedFile: { kind: 'OBJECT', fields: { path: prop('String!', 'path'), additions: prop('Int!', 'additions'), deletions: prop('Int!', 'deletions'), changeType: prop('PatchStatus!', 'changeType') } },
  PullRequestCommit: { kind: 'OBJECT', fields: { commit: prop('Commit!', 'commit') } },
  IssueComment: {
    kind: 'OBJECT',
    interfaces: ['Node'],
    fields: {
      id: { type: 'ID!', resolve: (c: Comment) => globalIdOf(c) },
      databaseId: prop('Int', 'id'),
      body: prop('String!', 'body'),
      createdAt: prop('DateTime!', 'createdAt'),
      updatedAt: prop('DateTime!', 'updatedAt'),
      author: { type: 'User', resolve: () => viewer },
      url: { type: 'URI!', resolve: (c: Comment, _a, ctx) => `${ctx.base}/${c.issue.repo.fullName}/issues/${c.issue.number}#issuecomment-${c.id}` },
    },
  },
  Label: {
    kind: 'OBJECT',
    interfaces: ['Node'],
    fields: { id: { type: 'ID!', resolve: (l: Label) => globalIdOf(l) }, name: prop('String!', 'name'), color: prop('String!', 'color'), description: prop('String', 'description') },
  },
  PageInfo: {
    kind: 'OBJECT',
    fields: { hasNextPage: prop('Boolean!', 'hasNextPage'), hasPreviousPage: prop('Boolean!', 'hasPreviousPage'), startCursor: prop('String', 'startCursor'), endCursor: prop('String', 'endCursor') },
  },
  RateLimit: { kind: 'OBJECT', fields: { limit: prop('Int!', 'limit'), cost: prop('Int!', 'cost'), remaining: prop('Int!', 'remaining'), resetAt: prop('DateTime!', 'resetAt'), used: prop('Int!', 'used') } },
  ...connectionTypes('Issue'),
  ...connectionTypes('PullRequest'),
  ...connectionTypes('IssueComment'),
  ...connectionTypes('Label'),
  ...connectionTypes('User'),
  ...connectionTypes('Repository'),
  ...connectionTypes('Ref'),
  ...connectionTypes('PullRequestChangedFile'),
  ...connectionTypes('PullRequestCommit'),
  IssueState: { kind: 'ENUM', enumValues: ['OPEN', 'CLOSED'] },
  IssueStateReason: { kind: 'ENUM', enumValues: ['COMPLETED', 'NOT_PLANNED', 'REOPENED', 'DUPLICATE'] },
  PullRequestState: { kind: 'ENUM', enumValues: ['OPEN', 'CLOSED', 'MERGED'] },
  MergeableState: { kind: 'ENUM', enumValues: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'] },
  PullRequestMergeMethod: { kind: 'ENUM', enumValues: ['MERGE', 'SQUASH', 'REBASE'] },
  RepositoryPermission: { kind: 'ENUM', enumValues: ['ADMIN', 'MAINTAIN', 'WRITE', 'TRIAGE', 'READ'] },
  RepositoryVisibility: { kind: 'ENUM', enumValues: ['PRIVATE', 'PUBLIC', 'INTERNAL'] },
  RepositoryPrivacy: { kind: 'ENUM', enumValues: ['PRIVATE', 'PUBLIC'] },
  SearchType: { kind: 'ENUM', enumValues: ['ISSUE', 'ISSUE_ADVANCED', 'REPOSITORY', 'USER', 'DISCUSSION'] },
  IssueOrderField: { kind: 'ENUM', enumValues: ['CREATED_AT', 'UPDATED_AT', 'COMMENTS'] },
  OrderDirection: { kind: 'ENUM', enumValues: ['ASC', 'DESC'] },
  PatchStatus: { kind: 'ENUM', enumValues: ['ADDED', 'DELETED', 'RENAMED', 'COPIED', 'MODIFIED', 'CHANGED'] },
  IssueOrder: { kind: 'INPUT_OBJECT', inputFields: { field: 'IssueOrderField!', direction: 'OrderDirection!' } },
  IssueFilters: { kind: 'INPUT_OBJECT', inputFields: { states: '[IssueState!]', labels: '[String!]' } },
  Actor: { kind: 'INTERFACE', fields: { login: prop('String!', 'login') }, possibleTypes: ['User'] },
  RepositoryOwner: { kind: 'INTERFACE', fields: { login: prop('String!', 'login'), id: { type: 'ID!', resolve: (u) => globalIdOf(u) } }, possibleTypes: ['User'] },
  ID: { kind: 'SCALAR' },
  String: { kind: 'SCALAR' },
  Int: { kind: 'SCALAR' },
  Float: { kind: 'SCALAR' },
  Boolean: { kind: 'SCALAR' },
  DateTime: { kind: 'SCALAR' },
  GitTimestamp: { kind: 'SCALAR' },
  GitObjectID: { kind: 'SCALAR' },
  URI: { kind: 'SCALAR' },
};

// ------------------------------------------------------------------ mutations

function nodeOf<T>(id: unknown, kind: string, what: string): T {
  const found = fromGlobalId(id) as { kind?: string } | undefined;
  if (found === undefined || found.kind !== kind) throw new FieldError(`Could not resolve to a node with the global id of '${String(id)}'`, 'NOT_FOUND');
  return found as T;
  void what;
}

function inputOf(args: Record<string, any>): Record<string, any> {
  const input = args.input;
  if (typeof input !== 'object' || input === null) throw new FieldError('Argument input is required', 'INVALID_ARGUMENT');
  return input;
}

function wrapRest<T>(run: () => T): T {
  try {
    return run();
  } catch (err) {
    if (err instanceof HttpError) {
      const detail = Array.isArray(err.extra.errors) ? (err.extra.errors as Array<{ message?: string }>).map((e) => e.message).filter(Boolean).join('; ') : '';
      throw new FieldError(detail === '' ? err.message : detail);
    }
    throw err;
  }
}

function payload(type: string, fields: Record<string, string>): void {
  TYPES[type] = { kind: 'OBJECT', fields: Object.fromEntries(Object.entries(fields).map(([name, t]) => [name, prop(t, name)])) };
}

payload('CreateIssuePayload', { issue: 'Issue', clientMutationId: 'String' });
payload('CloseIssuePayload', { issue: 'Issue', clientMutationId: 'String' });
payload('ReopenIssuePayload', { issue: 'Issue', clientMutationId: 'String' });
payload('UpdateIssuePayload', { issue: 'Issue', clientMutationId: 'String' });
payload('CreatePullRequestPayload', { pullRequest: 'PullRequest', clientMutationId: 'String' });
payload('MergePullRequestPayload', { pullRequest: 'PullRequest', clientMutationId: 'String' });
payload('ClosePullRequestPayload', { pullRequest: 'PullRequest', clientMutationId: 'String' });
payload('ReopenPullRequestPayload', { pullRequest: 'PullRequest', clientMutationId: 'String' });
payload('UpdatePullRequestPayload', { pullRequest: 'PullRequest', clientMutationId: 'String' });
payload('AddCommentPayload', { commentEdge: 'IssueCommentEdge', subject: 'Node', timelineEdge: 'IssueCommentEdge', clientMutationId: 'String' });

const mutationFields = (TYPES.Mutation as TypeDef).fields as Record<string, FieldDef>;
mutationFields.createIssue = {
  type: 'CreateIssuePayload',
  args: { input: 'CreateIssueInput!' },
  resolve: (_p, args) => {
    const input = inputOf(args);
    const repo = nodeOf<Repo>(input.repositoryId, 'repo', 'repository');
    if (typeof input.title !== 'string' || input.title === '') throw new FieldError('Title can\'t be blank');
    const labels = Array.isArray(input.labelIds) ? input.labelIds.map((id: unknown) => nodeOf<Label>(id, 'label', 'label')) : [];
    return { issue: addIssue(repo, input.title, typeof input.body === 'string' ? input.body : '', labels), clientMutationId: input.clientMutationId ?? null };
  },
};
mutationFields.closeIssue = {
  type: 'CloseIssuePayload',
  args: { input: 'CloseIssueInput!' },
  resolve: (_p, args) => {
    const input = inputOf(args);
    const issue = nodeOf<Issue>(input.issueId, 'issue', 'issue');
    setIssueState(issue, 'closed');
    if (input.stateReason === 'NOT_PLANNED') issue.stateReason = 'not_planned';
    return { issue, clientMutationId: input.clientMutationId ?? null };
  },
};
mutationFields.reopenIssue = {
  type: 'ReopenIssuePayload',
  args: { input: 'ReopenIssueInput!' },
  resolve: (_p, args) => {
    const input = inputOf(args);
    const issue = nodeOf<Issue>(input.issueId, 'issue', 'issue');
    setIssueState(issue, 'open');
    return { issue, clientMutationId: input.clientMutationId ?? null };
  },
};
mutationFields.updateIssue = {
  type: 'UpdateIssuePayload',
  args: { input: 'UpdateIssueInput!' },
  resolve: (_p, args) => {
    const input = inputOf(args);
    const issue = nodeOf<Issue>(input.id, 'issue', 'issue');
    if (typeof input.title === 'string') issue.title = input.title;
    if (typeof input.body === 'string') issue.body = input.body;
    if (input.state === 'OPEN' || input.state === 'CLOSED') setIssueState(issue, input.state === 'OPEN' ? 'open' : 'closed');
    issue.json = undefined;
    return { issue, clientMutationId: input.clientMutationId ?? null };
  },
};
mutationFields.createPullRequest = {
  type: 'CreatePullRequestPayload',
  args: { input: 'CreatePullRequestInput!' },
  resolve: (_p, args) => {
    const input = inputOf(args);
    const repo = nodeOf<Repo>(input.repositoryId, 'repo', 'repository');
    if (typeof input.title !== 'string' || input.title === '') throw new FieldError('Title can\'t be blank');
    const pull = wrapRest(() => openPull(repo, input.title, typeof input.body === 'string' ? input.body : '', String(input.headRefName ?? ''), String(input.baseRefName ?? '')));
    return { pullRequest: pull, clientMutationId: input.clientMutationId ?? null };
  },
};
mutationFields.mergePullRequest = {
  type: 'MergePullRequestPayload',
  args: { input: 'MergePullRequestInput!' },
  resolve: (_p, args) => {
    const input = inputOf(args);
    const pull = nodeOf<Pull>(input.pullRequestId, 'pull', 'pull request');
    if (typeof input.expectedHeadOid === 'string' && input.expectedHeadOid !== pullHeadSha(pull)) throw new FieldError('Head branch was modified. Review and try the merge again.');
    const method = String(input.mergeMethod ?? 'MERGE').toLowerCase();
    const result = mergePull(pull, method, typeof input.commitHeadline === 'string' ? input.commitHeadline : undefined, typeof input.commitBody === 'string' ? input.commitBody : undefined);
    if (!result.ok) throw new FieldError(result.message);
    return { pullRequest: pull, clientMutationId: input.clientMutationId ?? null };
  },
};
mutationFields.closePullRequest = {
  type: 'ClosePullRequestPayload',
  args: { input: 'ClosePullRequestInput!' },
  resolve: (_p, args) => {
    const input = inputOf(args);
    const pull = nodeOf<Pull>(input.pullRequestId, 'pull', 'pull request');
    setPullState(pull, 'closed');
    return { pullRequest: pull, clientMutationId: input.clientMutationId ?? null };
  },
};
mutationFields.reopenPullRequest = {
  type: 'ReopenPullRequestPayload',
  args: { input: 'ReopenPullRequestInput!' },
  resolve: (_p, args) => {
    const input = inputOf(args);
    const pull = nodeOf<Pull>(input.pullRequestId, 'pull', 'pull request');
    setPullState(pull, 'open');
    return { pullRequest: pull, clientMutationId: input.clientMutationId ?? null };
  },
};
mutationFields.addComment = {
  type: 'AddCommentPayload',
  args: { input: 'AddCommentInput!' },
  resolve: (_p, args) => {
    const input = inputOf(args);
    const subject = fromGlobalId(input.subjectId) as Issue | Pull | undefined;
    if (subject === undefined || (subject.kind !== 'issue' && subject.kind !== 'pull')) throw new FieldError(`Could not resolve to a node with the global id of '${String(input.subjectId)}'`, 'NOT_FOUND');
    if (typeof input.body !== 'string' || input.body === '') throw new FieldError('Body can\'t be blank');
    const comment = addComment(subject as Issue, input.body);
    if (subject.kind === 'pull') {
      (subject as Issue).comments.pop();
      subject.comments.push(comment);
    }
    const edge = { cursor: Buffer.from(`cursor:v2:${subject.comments.length - 1}`).toString('base64'), node: comment };
    return { commentEdge: edge, timelineEdge: edge, subject, clientMutationId: input.clientMutationId ?? null };
  },
};

for (const name of ['CreateIssueInput', 'CloseIssueInput', 'ReopenIssueInput', 'UpdateIssueInput', 'CreatePullRequestInput', 'MergePullRequestInput', 'ClosePullRequestInput', 'ReopenPullRequestInput', 'AddCommentInput']) {
  TYPES[name] = { kind: 'INPUT_OBJECT', inputFields: { clientMutationId: 'String' } };
}

function typeOf(value: unknown): string {
  const kind = (value as { kind?: string; __typename?: string }).kind;
  switch (kind) {
    case 'repo':
      return 'Repository';
    case 'issue':
      return 'Issue';
    case 'pull':
      return 'PullRequest';
    case 'comment':
      return 'IssueComment';
    case 'label':
      return 'Label';
    case 'user':
      return 'User';
    default:
      return (value as { __typename?: string }).__typename ?? 'Node';
  }
}

// ------------------------------------------------------------------ introspection

const introspectionCache = new Map<string, unknown>();

function introType(ref: string): unknown {
  if (ref.endsWith('!')) return { kind: 'NON_NULL', name: null, description: null, ofType: introType(ref.slice(0, -1)) };
  if (ref.startsWith('[')) return { kind: 'LIST', name: null, description: null, ofType: introType(ref.slice(1, -1)) };
  return introNamed(ref);
}

function introNamed(name: string): unknown {
  const cached = introspectionCache.get(name);
  if (cached !== undefined) return cached;
  const def = TYPES[name];
  if (def === undefined) return null;
  const out: Record<string, unknown> = { kind: def.kind, name, description: def.description ?? null, ofType: null, specifiedByURL: null };
  introspectionCache.set(name, out);
  out.fields = def.fields === undefined ? null : Object.entries(def.fields).map(([fieldName, field]) => ({
    name: fieldName,
    description: field.description ?? null,
    args: Object.entries(field.args ?? {}).map(([argName, argType]) => ({ name: argName, description: null, type: introType(argType), defaultValue: null, isDeprecated: false, deprecationReason: null })),
    type: introType(field.type),
    isDeprecated: false,
    deprecationReason: null,
  }));
  out.enumValues = def.enumValues === undefined ? null : def.enumValues.map((value) => ({ name: value, description: null, isDeprecated: false, deprecationReason: null }));
  out.interfaces = def.kind === 'OBJECT' ? (def.interfaces ?? []).map((iface) => introNamed(iface)) : null;
  out.possibleTypes = def.possibleTypes === undefined ? null : def.possibleTypes.map((type) => introNamed(type));
  out.inputFields = def.inputFields === undefined ? null : Object.entries(def.inputFields).map(([fieldName, type]) => ({ name: fieldName, description: null, type: introType(type), defaultValue: null, isDeprecated: false, deprecationReason: null }));
  return out;
}

function schemaIntro(): unknown {
  return {
    description: null,
    queryType: introNamed('Query'),
    mutationType: introNamed('Mutation'),
    subscriptionType: null,
    types: Object.keys(TYPES).map((name) => introNamed(name)),
    directives: [
      { name: 'include', description: null, locations: ['FIELD', 'FRAGMENT_SPREAD', 'INLINE_FRAGMENT'], args: [{ name: 'if', description: null, type: introType('Boolean!'), defaultValue: null }], isRepeatable: false },
      { name: 'skip', description: null, locations: ['FIELD', 'FRAGMENT_SPREAD', 'INLINE_FRAGMENT'], args: [{ name: 'if', description: null, type: introType('Boolean!'), defaultValue: null }], isRepeatable: false },
    ],
  };
}

// ------------------------------------------------------------------ execution

interface GqlError {
  message: string;
  type?: string;
  path?: Array<string | number>;
  locations?: Array<{ line: number; column: number }>;
  extensions?: Record<string, unknown>;
}

class Executor {
  readonly errors: GqlError[] = [];
  private readonly doc: Document;
  private readonly vars: Record<string, unknown>;
  private readonly ctx: Ctx;

  constructor(doc: Document, vars: Record<string, unknown>, ctx: Ctx) {
    this.doc = doc;
    this.vars = vars;
    this.ctx = ctx;
  }

  value(node: Value): any {
    switch (node.kind) {
      case 'var':
        return this.vars[node.name] ?? null;
      case 'int':
        return Number.parseInt(node.value, 10);
      case 'float':
        return Number.parseFloat(node.value);
      case 'string':
      case 'enum':
        return node.value;
      case 'bool':
        return node.value;
      case 'null':
        return null;
      case 'list':
        return node.values.map((value) => this.value(value));
      case 'object':
        return Object.fromEntries(node.fields.map(([name, value]) => [name, this.value(value)]));
    }
  }

  private included(directives: Directive[]): boolean {
    for (const directive of directives) {
      const arg = directive.args.find(([name]) => name === 'if');
      const value = arg === undefined ? true : Boolean(this.value(arg[1]));
      if (directive.name === 'skip' && value) return false;
      if (directive.name === 'include' && !value) return false;
    }
    return true;
  }

  private applies(condition: string | null, typeName: string): boolean {
    if (condition === null || condition === typeName) return true;
    const def = TYPES[condition];
    if (def === undefined) return false;
    if (def.possibleTypes?.includes(typeName)) return true;
    return TYPES[typeName]?.interfaces?.includes(condition) ?? false;
  }

  /** Static check: every field named exists on its type. */
  validate(typeName: string, selections: Selection[], path: string[], seen = new Set<string>()): void {
    const def = TYPES[typeName];
    for (const sel of selections) {
      if (sel.kind === 'spread') {
        const fragment = this.doc.fragments.get(sel.name);
        if (fragment === undefined) {
          this.errors.push({ message: `Fragment ${sel.name} was used, but not defined`, extensions: { code: 'undefinedFragment' } });
          continue;
        }
        if (seen.has(sel.name)) continue;
        seen.add(sel.name);
        this.validate(TYPES[fragment.on] !== undefined ? fragment.on : typeName, fragment.selections, path, seen);
        continue;
      }
      if (sel.kind === 'inline') {
        if (sel.on !== null && TYPES[sel.on] === undefined) {
          this.errors.push({ message: `No such type ${sel.on}, so it can't be a fragment condition`, extensions: { code: 'undefinedType', typeName: sel.on } });
          continue;
        }
        this.validate(sel.on ?? typeName, sel.selections, path, seen);
        continue;
      }
      if (sel.name === '__typename') continue;
      if (typeName === 'Query' && (sel.name === '__type' || sel.name === '__schema')) continue;
      const field = def?.fields?.[sel.name];
      if (field === undefined) {
        this.errors.push({
          message: `Field '${sel.name}' doesn't exist on type '${typeName}'`,
          path: [...path, sel.name],
          extensions: { code: 'undefinedField', typeName, fieldName: sel.name },
        });
        continue;
      }
      const named = field.type.replace(/[[\]!]/g, '');
      if (sel.selections !== null) this.validate(named, sel.selections, [...path, sel.alias ?? sel.name], seen);
    }
  }

  private collect(typeName: string, selections: Selection[], into: Array<Extract<Selection, { kind: 'field' }>>): void {
    for (const sel of selections) {
      if (!this.included(sel.directives)) continue;
      if (sel.kind === 'field') into.push(sel);
      else if (sel.kind === 'inline') {
        if (this.applies(sel.on, typeName)) this.collect(typeName, sel.selections, into);
      } else {
        const fragment = this.doc.fragments.get(sel.name);
        if (fragment !== undefined && this.applies(fragment.on, typeName)) this.collect(typeName, fragment.selections, into);
      }
    }
  }

  object(typeName: string, value: any, selections: Selection[], path: Array<string | number>): Record<string, unknown> {
    const fields: Array<Extract<Selection, { kind: 'field' }>> = [];
    this.collect(typeName, selections, fields);
    const out: Record<string, unknown> = {};
    for (const sel of fields) {
      const key = sel.alias ?? sel.name;
      if (sel.name === '__typename') {
        out[key] = typeName;
        continue;
      }
      const args = Object.fromEntries(sel.args.map(([name, node]) => [name, this.value(node)]));
      if (typeName === 'Query' && sel.name === '__type') {
        out[key] = this.dynamic(introNamed(String(args.name)), sel.selections ?? []);
        continue;
      }
      if (typeName === 'Query' && sel.name === '__schema') {
        out[key] = this.dynamic(schemaIntro(), sel.selections ?? []);
        continue;
      }
      const def = TYPES[typeName]?.fields?.[sel.name];
      if (def === undefined) {
        out[key] = null;
        continue;
      }
      const fieldPath = [...path, key];
      let raw: unknown;
      try {
        raw = def.resolve(value, args, this.ctx);
      } catch (err) {
        if (err instanceof FieldError) {
          this.errors.push({ type: err.type, path: fieldPath, message: err.message });
          out[key] = null;
          continue;
        }
        throw err;
      }
      const completed = this.complete(def.type, raw, sel.selections, fieldPath);
      const prior = out[key];
      out[key] = prior !== undefined && isPlain(prior) && isPlain(completed) ? { ...prior, ...completed } : completed;
    }
    return out;
  }

  private complete(type: string, value: unknown, selections: Selection[] | null, path: Array<string | number>): unknown {
    const bare = type.endsWith('!') ? type.slice(0, -1) : type;
    if (value === null || value === undefined) return null;
    if (bare.startsWith('[')) {
      if (!Array.isArray(value)) return null;
      return value.map((item, index) => this.complete(bare.slice(1, -1), item, selections, [...path, index]));
    }
    const def = TYPES[bare];
    if (def === undefined || def.kind === 'SCALAR' || def.kind === 'ENUM' || selections === null) return value;
    const concrete = def.kind === 'INTERFACE' || def.kind === 'UNION' ? typeOf(value) : bare;
    return this.object(concrete, value, selections, path);
  }

  /** Introspection values are plain objects: select their properties directly. */
  private dynamic(value: unknown, selections: Selection[]): unknown {
    if (value === null || value === undefined) return null;
    if (Array.isArray(value)) return value.map((item) => this.dynamic(item, selections));
    if (!isPlain(value)) return value;
    const record = value as Record<string, unknown>;
    const fields: Array<Extract<Selection, { kind: 'field' }>> = [];
    const typeName = typeof record.kind === 'string' && typeof record.name === 'string' && 'ofType' in record ? '__Type' : '__Object';
    this.collect(typeName, selections, fields);
    const out: Record<string, unknown> = {};
    for (const sel of fields) {
      const key = sel.alias ?? sel.name;
      if (sel.name === '__typename') {
        out[key] = typeName;
        continue;
      }
      let item = record[sel.name];
      if (sel.name === 'fields' || sel.name === 'enumValues' || sel.name === 'inputFields') {
        const args = Object.fromEntries(sel.args.map(([name, node]) => [name, this.value(node)]));
        void args;
      }
      if (item === undefined) item = null;
      out[key] = sel.selections === null ? item : this.dynamic(item, sel.selections);
    }
    return out;
  }
}

function isPlain(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function execute(body: unknown, ctx: Ctx): Record<string, unknown> {
  if (!isPlain(body)) return { errors: [{ message: 'A query attribute must be specified and must be a string.' }] };
  const query = body.query;
  if (typeof query !== 'string' || query.trim() === '') return { errors: [{ message: 'A query attribute must be specified and must be a string.' }] };
  let doc: Document;
  try {
    doc = new Parser(lex(query)).document();
  } catch (err) {
    return { errors: [{ message: `Parse error: ${(err as Error).message}`, extensions: { code: 'parseError' } }] };
  }
  const operationName = typeof body.operationName === 'string' && body.operationName !== '' ? body.operationName : null;
  const operation = operationName === null ? (doc.operations.length === 1 ? doc.operations[0] : undefined) : doc.operations.find((op) => op.name === operationName);
  if (operation === undefined) {
    return { errors: [{ message: operationName === null ? 'An operation name is required when the document holds more than one operation' : `No operation named "${operationName}"` }] };
  }
  const given = isPlain(body.variables) ? body.variables : {};
  const vars: Record<string, unknown> = {};
  const executor = new Executor(doc, vars, ctx);
  for (const variable of operation.vars) {
    if (variable.name in given) vars[variable.name] = given[variable.name];
    else if (variable.default !== null) vars[variable.name] = executor.value(variable.default);
    else if (variable.type.endsWith('!')) {
      return { errors: [{ message: `Variable $${variable.name} of type ${variable.type} was provided invalid value`, extensions: { code: 'variableMismatch' } }] };
    } else vars[variable.name] = null;
  }
  const root = operation.type === 'mutation' ? 'Mutation' : 'Query';
  executor.validate(root, operation.selections, [operation.type + (operation.name === null ? '' : ` ${operation.name}`)]);
  if (executor.errors.length > 0) return { errors: executor.errors };
  const data = executor.object(root, null, operation.selections, []);
  return executor.errors.length > 0 ? { data, errors: executor.errors } : { data };
}

export function handleGraphql(method: string, authed: boolean, raw: Buffer, base = ''): { status: number; body: unknown } {
  if (!authed) return { status: 401, body: { message: 'This endpoint requires you to be authenticated.', documentation_url: 'https://docs.github.com/graphql/guides/forming-calls-with-graphql#authenticating-with-graphql' } };
  if (method !== 'POST') return { status: 405, body: { message: 'Use POST for GraphQL requests' } };
  let body: unknown;
  try {
    body = JSON.parse(raw.toString('utf8'));
  } catch {
    return { status: 400, body: { message: 'Problems parsing JSON' } };
  }
  const ctx: Ctx = { base };
  if (Array.isArray(body)) return { status: 200, body: body.map((entry) => safeExecute(entry, ctx)) };
  return { status: 200, body: safeExecute(body, ctx) };
}

function safeExecute(body: unknown, ctx: Ctx): Record<string, unknown> {
  try {
    return execute(body, ctx);
  } catch (err) {
    process.stderr.write(`[forge] graphql: ${err instanceof Error ? (err.stack ?? err.message) : String(err)}\n`);
    return { data: null, errors: [{ message: 'Something went wrong while executing your query.' }] };
  }
}

void parseCommit;
