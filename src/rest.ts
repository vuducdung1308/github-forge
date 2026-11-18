// GitHub REST v3 under /api/v3: repositories, branches, refs, commits, contents, labels, issues,
// comments and pull requests. Every answer is JSON; errors carry {message, documentation_url}.

import { isOid, parseIdent } from './git.ts';
import {
  type Comment,
  type Issue,
  type Label,
  type Pull,
  type Repo,
  type User,
  addComment,
  addIssue,
  addLabel,
  addPull,
  commitFile,
  createRepo,
  deleteRepo,
  diffCommits,
  findRepo,
  globalIdOf,
  listRepos,
  logFrom,
  lookupPath,
  mergePull,
  nowIso,
  pullBaseSha,
  pullFiles,
  pullHeadSha,
  pullMergeable,
  resolveLabels,
  setIssueState,
  setPullState,
  viewer,
} from './store.ts';

export interface Request {
  method: string;
  path: string;
  query: URLSearchParams;
  body: unknown;
  base: string;
}

export interface Reply {
  status: number;
  body?: unknown;
  /** Pre-serialized JSON text, used in place of `body`. */
  text?: string;
  headers?: Record<string, string>;
}

export class HttpError extends Error {
  readonly status: number;
  readonly extra: Record<string, unknown>;
  constructor(status: number, message: string, extra: Record<string, unknown> = {}) {
    super(message);
    this.status = status;
    this.extra = extra;
  }
}

const DOCS = 'https://docs.github.com/rest';

export function errorBody(status: number, message: string, extra: Record<string, unknown> = {}): Record<string, unknown> {
  return { message, documentation_url: DOCS, status: String(status), ...extra };
}

function notFound(): never {
  throw new HttpError(404, 'Not Found');
}

function invalid(message: string, errors: unknown[] = []): never {
  throw new HttpError(422, message, errors.length > 0 ? { errors } : {});
}

function obj(body: unknown): Record<string, unknown> {
  return typeof body === 'object' && body !== null && !Array.isArray(body) ? (body as Record<string, unknown>) : {};
}

function str(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

// ------------------------------------------------------------------ serializers

export function userJson(user: User, base: string): Record<string, unknown> {
  return {
    login: user.login,
    id: user.id,
    node_id: globalIdOf(user),
    avatar_url: `${base}/avatars/${user.login}`,
    url: `${base}/api/v3/users/${user.login}`,
    html_url: `${base}/${user.login}`,
    type: 'User',
    site_admin: false,
    name: user.name,
    email: user.email,
  };
}

export function repoJson(repo: Repo, base: string): Record<string, unknown> {
  const api = `${base}/api/v3/repos/${repo.fullName}`;
  let openIssues = 0;
  for (const issue of repo.issues.values()) if (issue.state === 'open') openIssues += 1;
  return {
    id: repo.id,
    node_id: globalIdOf(repo),
    name: repo.name,
    full_name: repo.fullName,
    private: repo.private,
    visibility: repo.private ? 'private' : 'public',
    owner: userJson(repo.owner, base),
    html_url: `${base}/${repo.fullName}`,
    description: repo.description,
    fork: false,
    url: api,
    clone_url: `${base}/${repo.fullName}.git`,
    git_url: `${base}/${repo.fullName}.git`,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
    pushed_at: repo.pushedAt,
    size: 0,
    language: null,
    default_branch: repo.defaultBranch,
    has_issues: true,
    has_projects: false,
    has_wiki: false,
    archived: false,
    disabled: false,
    open_issues_count: openIssues,
    open_issues: openIssues,
    forks_count: 0,
    stargazers_count: 0,
    watchers_count: 0,
    permissions: { admin: true, maintain: true, push: true, triage: true, pull: true },
  };
}

function labelJson(label: Label, repo: Repo, base: string): Record<string, unknown> {
  return {
    id: label.id,
    node_id: globalIdOf(label),
    url: `${base}/api/v3/repos/${repo.fullName}/labels/${encodeURIComponent(label.name)}`,
    name: label.name,
    color: label.color,
    description: label.description,
    default: false,
  };
}

export function issueJson(issue: Issue, base: string): Record<string, unknown> {
  const repo = issue.repo;
  const api = `${base}/api/v3/repos/${repo.fullName}`;
  return {
    id: issue.id,
    node_id: globalIdOf(issue),
    url: `${api}/issues/${issue.number}`,
    repository_url: api,
    comments_url: `${api}/issues/${issue.number}/comments`,
    html_url: `${base}/${repo.fullName}/issues/${issue.number}`,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    state_reason: issue.stateReason,
    locked: false,
    user: userJson(viewer, base),
    labels: issue.labels.map((label) => labelJson(label, repo, base)),
    assignee: null,
    assignees: [],
    milestone: null,
    comments: issue.comments.length,
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
    closed_at: issue.closedAt,
    author_association: 'OWNER',
  };
}

function issueText(issue: Issue, base: string): string {
  if (issue.json === undefined) issue.json = JSON.stringify(issueJson(issue, base));
  return issue.json;
}

export function commentJson(comment: Comment, base: string): Record<string, unknown> {
  const repo = comment.issue.repo;
  return {
    id: comment.id,
    node_id: globalIdOf(comment),
    url: `${base}/api/v3/repos/${repo.fullName}/issues/comments/${comment.id}`,
    html_url: `${base}/${repo.fullName}/issues/${comment.issue.number}#issuecomment-${comment.id}`,
    issue_url: `${base}/api/v3/repos/${repo.fullName}/issues/${comment.issue.number}`,
    body: comment.body,
    user: userJson(viewer, base),
    created_at: comment.createdAt,
    updated_at: comment.updatedAt,
    author_association: 'OWNER',
  };
}

function branchSide(pull: Pull, ref: string, sha: string | null, base: string): Record<string, unknown> {
  return { label: `${pull.repo.owner.login}:${ref}`, ref, sha: sha ?? '', user: userJson(viewer, base), repo: repoJson(pull.repo, base) };
}

export function pullJson(pull: Pull, base: string, detail = true): Record<string, unknown> {
  const repo = pull.repo;
  const api = `${base}/api/v3/repos/${repo.fullName}`;
  const json: Record<string, unknown> = {
    id: pull.id,
    node_id: globalIdOf(pull),
    url: `${api}/pulls/${pull.number}`,
    html_url: `${base}/${repo.fullName}/pull/${pull.number}`,
    diff_url: `${base}/${repo.fullName}/pull/${pull.number}.diff`,
    issue_url: `${api}/issues/${pull.number}`,
    number: pull.number,
    state: pull.state,
    locked: false,
    title: pull.title,
    body: pull.body,
    user: userJson(viewer, base),
    labels: [],
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
    closed_at: pull.closedAt,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    draft: false,
    head: branchSide(pull, pull.head, pullHeadSha(pull), base),
    base: branchSide(pull, pull.base, pullBaseSha(pull), base),
    author_association: 'OWNER',
    merged: pull.merged,
  };
  if (detail) {
    const files = pullFiles(pull);
    const mergeable = pullMergeable(pull);
    json.mergeable = mergeable;
    json.mergeable_state = pull.merged ? 'unknown' : mergeable === true ? 'clean' : 'dirty';
    json.merged_by = pull.merged ? userJson(viewer, base) : null;
    json.comments = pull.comments.length;
    json.commits = 1;
    json.additions = files.reduce((sum, file) => sum + file.additions, 0);
    json.deletions = files.reduce((sum, file) => sum + file.deletions, 0);
    json.changed_files = files.length;
  }
  return json;
}

function commitJson(repo: Repo, sha: string, base: string): Record<string, unknown> | null {
  const commit = repo.commit(sha);
  if (commit === null) return null;
  const author = parseIdent(commit.author);
  const committer = parseIdent(commit.committer);
  const api = `${base}/api/v3/repos/${repo.fullName}`;
  return {
    sha,
    node_id: `C_${sha}`,
    url: `${api}/commits/${sha}`,
    html_url: `${base}/${repo.fullName}/commit/${sha}`,
    commit: {
      message: commit.message.replace(/\n$/, ''),
      author,
      committer,
      tree: { sha: commit.tree, url: `${api}/git/trees/${commit.tree}` },
      url: `${api}/git/commits/${sha}`,
    },
    author: null,
    committer: null,
    parents: commit.parents.map((parent) => ({ sha: parent, url: `${api}/commits/${parent}` })),
  };
}

// ------------------------------------------------------------------ pagination

function paginate<T>(req: Request, items: readonly T[], path: string): { page: T[]; headers: Record<string, string> } {
  const perPage = Math.min(100, Math.max(1, Number.parseInt(req.query.get('per_page') ?? '30', 10) || 30));
  const page = Math.max(1, Number.parseInt(req.query.get('page') ?? '1', 10) || 1);
  const slice = items.slice((page - 1) * perPage, page * perPage);
  const last = Math.max(1, Math.ceil(items.length / perPage));
  const link = (target: number, rel: string): string => {
    const query = new URLSearchParams(req.query);
    query.set('per_page', String(perPage));
    query.set('page', String(target));
    return `<${req.base}/api/v3${path}?${query.toString()}>; rel="${rel}"`;
  };
  const links: string[] = [];
  if (page < last) links.push(link(page + 1, 'next'), link(last, 'last'));
  if (page > 1) links.push(link(1, 'first'), link(page - 1, 'prev'));
  return { page: slice, headers: links.length > 0 ? { link: links.join(', ') } : {} };
}

// ------------------------------------------------------------------ routing

type Handler = (req: Request, params: string[]) => Reply;
const routes: Array<{ method: string; pattern: RegExp; handler: Handler }> = [];

function route(method: string, pattern: string, handler: Handler): void {
  const source = pattern.replace(/\{[a-z_]+\*\}/g, '(.+)').replace(/\{[a-z_]+\}/g, '([^/]+)');
  routes.push({ method, pattern: new RegExp(`^${source}$`), handler });
}

export function handleRest(req: Request): Reply {
  const path = req.path.length > 1 ? req.path.replace(/\/+$/, '') : req.path;
  let matchedPath = false;
  for (const entry of routes) {
    const match = entry.pattern.exec(path);
    if (match === null) continue;
    matchedPath = true;
    if (entry.method !== req.method) continue;
    const params = match.slice(1).map((part) => {
      try {
        return decodeURIComponent(part);
      } catch {
        return part;
      }
    });
    return entry.handler(req, params);
  }
  if (matchedPath) throw new HttpError(404, 'Not Found');
  throw new HttpError(404, 'Not Found');
}

function repoOf(params: string[]): Repo {
  const repo = findRepo(params[0] ?? '', params[1] ?? '');
  if (repo === undefined || repo.owner.login.toLowerCase() !== (params[0] ?? '').toLowerCase()) notFound();
  return repo;
}

const ok = (body: unknown, status = 200, headers?: Record<string, string>): Reply => ({ status, body, headers });

// ---- root, meta, user

route('GET', '', (req) => ok(rootJson(req.base)));
route('GET', '/', (req) => ok(rootJson(req.base)));
function rootJson(base: string): Record<string, unknown> {
  return {
    current_user_url: `${base}/api/v3/user`,
    repository_url: `${base}/api/v3/repos/{owner}/{repo}`,
    user_repositories_url: `${base}/api/v3/user/repos`,
    issues_url: `${base}/api/v3/repos/{owner}/{repo}/issues/{number}`,
    rate_limit_url: `${base}/api/v3/rate_limit`,
  };
}
route('GET', '/meta', () => ok({ installed_version: '3.15.0', verifiable_password_authentication: false }));
route('GET', '/rate_limit', () => ok({ resources: { core: { limit: 5000, remaining: 5000, reset: 0, used: 0 } }, rate: { limit: 5000, remaining: 5000, reset: 0, used: 0 } }));
route('GET', '/user', (req) => ok(userJson(viewer, req.base)));
route('GET', '/users/{user}', (req, [login]) => {
  if ((login ?? '').toLowerCase() !== viewer.login.toLowerCase()) notFound();
  return ok(userJson(viewer, req.base));
});

// ---- repositories

route('GET', '/user/repos', (req) => {
  const sorted = listRepos().sort((a, b) => a.id - b.id);
  const { page, headers } = paginate(req, sorted, '/user/repos');
  return ok(page.map((repo) => repoJson(repo, req.base)), 200, headers);
});
route('GET', '/users/{user}/repos', (req, [login]) => {
  const sorted = listRepos().filter((repo) => repo.owner.login.toLowerCase() === (login ?? '').toLowerCase()).sort((a, b) => a.id - b.id);
  const { page, headers } = paginate(req, sorted, `/users/${login}/repos`);
  return ok(page.map((repo) => repoJson(repo, req.base)), 200, headers);
});
route('POST', '/user/repos', (req) => {
  const body = obj(req.body);
  const name = str(body.name);
  if (name === undefined || !/^[A-Za-z0-9._-]{1,100}$/.test(name) || name.endsWith('.git')) {
    invalid('Validation Failed', [{ resource: 'Repository', field: 'name', code: 'invalid' }]);
  }
  const repo = createRepo(name, str(body.description) ?? null, body.private === true || body.visibility === 'private');
  if (repo === null) invalid('Repository creation failed.', [{ resource: 'Repository', field: 'name', code: 'custom', message: 'name already exists on this account' }]);
  if (body.auto_init === true) commitFile(repo, repo.defaultBranch, 'README.md', Buffer.from(`# ${repo.name}\n`), 'Initial commit');
  return ok(repoJson(repo, req.base), 201);
});
route('GET', '/repos/{owner}/{repo}', (req, params) => ok(repoJson(repoOf(params), req.base)));
route('PATCH', '/repos/{owner}/{repo}', (req, params) => {
  const repo = repoOf(params);
  const body = obj(req.body);
  if (typeof body.description === 'string' || body.description === null) repo.description = body.description as string | null;
  if (typeof body.private === 'boolean') repo.private = body.private;
  if (typeof body.default_branch === 'string' && body.default_branch !== '') repo.defaultBranch = body.default_branch;
  repo.touch();
  return ok(repoJson(repo, req.base));
});
route('DELETE', '/repos/{owner}/{repo}', (_req, params) => {
  deleteRepo(repoOf(params));
  return { status: 204 };
});

// ---- branches and refs

function branchJson(repo: Repo, name: string, sha: string, base: string): Record<string, unknown> {
  return { name, commit: { sha, url: `${base}/api/v3/repos/${repo.fullName}/commits/${sha}` }, protected: false };
}
route('GET', '/repos/{owner}/{repo}/branches', (req, params) => {
  const repo = repoOf(params);
  const branches = [...repo.refs.entries()].filter(([name]) => name.startsWith('refs/heads/')).sort(([a], [b]) => (a < b ? -1 : 1));
  const { page, headers } = paginate(req, branches, `/repos/${repo.fullName}/branches`);
  return ok(page.map(([name, sha]) => branchJson(repo, name.slice(11), sha, req.base)), 200, headers);
});
route('GET', '/repos/{owner}/{repo}/branches/{branch*}', (req, params) => {
  const repo = repoOf(params);
  const branch = params[2] ?? '';
  const sha = repo.branchTip(branch);
  if (sha === undefined) throw new HttpError(404, 'Branch not found');
  return ok(branchJson(repo, branch, sha, req.base));
});

function refJson(repo: Repo, ref: string, sha: string, base: string): Record<string, unknown> {
  const type = repo.get(sha)?.type ?? 'commit';
  return { ref, node_id: `REF_${Buffer.from(`${repo.id}:${ref}`).toString('base64url')}`, url: `${base}/api/v3/repos/${repo.fullName}/git/${ref}`, object: { sha, type, url: `${base}/api/v3/repos/${repo.fullName}/git/${type}s/${sha}` } };
}
route('GET', '/repos/{owner}/{repo}/git/refs', (req, params) => {
  const repo = repoOf(params);
  return ok([...repo.refs.entries()].sort(([a], [b]) => (a < b ? -1 : 1)).map(([ref, sha]) => refJson(repo, ref, sha, req.base)));
});
for (const prefix of ['/repos/{owner}/{repo}/git/refs/{ref*}', '/repos/{owner}/{repo}/git/ref/{ref*}']) {
  route('GET', prefix, (req, params) => {
    const repo = repoOf(params);
    const ref = `refs/${params[2] ?? ''}`;
    const sha = repo.refs.get(ref);
    if (sha === undefined) notFound();
    return ok(refJson(repo, ref, sha, req.base));
  });
}
route('POST', '/repos/{owner}/{repo}/git/refs', (req, params) => {
  const repo = repoOf(params);
  const body = obj(req.body);
  const ref = str(body.ref);
  const sha = str(body.sha);
  if (ref === undefined || !ref.startsWith('refs/') || ref.split('/').length < 3) invalid('Reference name is invalid');
  if (sha === undefined || !isOid(sha) || repo.get(sha) === undefined) invalid('Object does not exist');
  if (repo.refs.has(ref)) invalid('Reference already exists');
  repo.setRef(ref, sha);
  return ok(refJson(repo, ref, sha, req.base), 201);
});
route('PATCH', '/repos/{owner}/{repo}/git/refs/{ref*}', (req, params) => {
  const repo = repoOf(params);
  const ref = `refs/${params[2] ?? ''}`;
  const current = repo.refs.get(ref);
  if (current === undefined) notFound();
  const sha = str(obj(req.body).sha);
  if (sha === undefined || repo.get(sha) === undefined) invalid('Object does not exist');
  repo.setRef(ref, sha);
  return ok(refJson(repo, ref, sha, req.base));
});
route('DELETE', '/repos/{owner}/{repo}/git/refs/{ref*}', (_req, params) => {
  const repo = repoOf(params);
  const ref = `refs/${params[2] ?? ''}`;
  if (!repo.refs.has(ref)) invalid('Reference does not exist');
  repo.setRef(ref, null);
  return { status: 204 };
});

// ---- commits and git data

/** A ref name, branch, tag or sha -> a commit sha. */
function resolveRef(repo: Repo, ref: string | null | undefined): string | undefined {
  if (ref === null || ref === undefined || ref === '') return repo.branchTip(repo.defaultBranch);
  const direct = repo.refs.get(ref) ?? repo.refs.get(`refs/heads/${ref}`) ?? repo.refs.get(`refs/tags/${ref}`);
  if (direct !== undefined) return repo.peel(direct);
  if (isOid(ref) && repo.get(ref) !== undefined) return repo.peel(ref);
  return undefined;
}

route('GET', '/repos/{owner}/{repo}/commits', (req, params) => {
  const repo = repoOf(params);
  const sha = resolveRef(repo, req.query.get('sha'));
  if (sha === undefined) {
    if (req.query.get('sha') !== null) notFound();
    throw new HttpError(409, 'Git Repository is empty.');
  }
  const perPage = Math.min(100, Math.max(1, Number.parseInt(req.query.get('per_page') ?? '30', 10) || 30));
  const pageNo = Math.max(1, Number.parseInt(req.query.get('page') ?? '1', 10) || 1);
  const all = logFrom(repo, sha, perPage * pageNo + 1);
  const page = all.slice((pageNo - 1) * perPage, pageNo * perPage);
  const headers: Record<string, string> = {};
  if (all.length > pageNo * perPage) {
    const query = new URLSearchParams(req.query);
    query.set('page', String(pageNo + 1));
    query.set('per_page', String(perPage));
    headers.link = `<${req.base}/api/v3/repos/${repo.fullName}/commits?${query.toString()}>; rel="next"`;
  }
  return ok(page.map((oid) => commitJson(repo, oid, req.base)), 200, headers);
});
route('GET', '/repos/{owner}/{repo}/commits/{ref*}', (req, params) => {
  const repo = repoOf(params);
  const sha = resolveRef(repo, params[2]);
  if (sha === undefined) throw new HttpError(422, `No commit found for SHA: ${params[2]}`);
  const json = commitJson(repo, sha, req.base);
  if (json === null) notFound();
  const parent = repo.commit(sha)?.parents[0] ?? null;
  json.files = diffCommits(repo, parent, sha);
  return ok(json);
});
route('GET', '/repos/{owner}/{repo}/git/commits/{sha}', (req, params) => {
  const repo = repoOf(params);
  const sha = params[2] ?? '';
  const commit = repo.commit(sha);
  if (commit === null) notFound();
  return ok({ sha, tree: { sha: commit.tree }, parents: commit.parents.map((p) => ({ sha: p })), message: commit.message.replace(/\n$/, ''), author: parseIdent(commit.author), committer: parseIdent(commit.committer), url: `${req.base}/api/v3/repos/${repo.fullName}/git/commits/${sha}` });
});
route('GET', '/repos/{owner}/{repo}/git/trees/{sha}', (req, params) => {
  const repo = repoOf(params);
  let sha = params[2] ?? '';
  const commitSha = resolveRef(repo, sha);
  if (commitSha !== undefined && repo.get(commitSha)?.type === 'commit') sha = repo.commit(commitSha)?.tree ?? sha;
  if (repo.get(sha)?.type !== 'tree') notFound();
  const recursive = req.query.get('recursive') !== null;
  const tree: unknown[] = [];
  const walk = (oid: string, prefix: string): void => {
    for (const entry of repo.tree(oid)) {
      const isDir = entry.mode === '40000';
      tree.push({ path: `${prefix}${entry.name}`, mode: isDir ? '040000' : entry.mode, type: isDir ? 'tree' : 'blob', sha: entry.oid, size: isDir ? undefined : repo.get(entry.oid)?.data.length });
      if (isDir && recursive) walk(entry.oid, `${prefix}${entry.name}/`);
    }
  };
  walk(sha, '');
  return ok({ sha, tree, truncated: false });
});
route('GET', '/repos/{owner}/{repo}/git/blobs/{sha}', (_req, params) => {
  const repo = repoOf(params);
  const blob = repo.get(params[2] ?? '');
  if (blob === undefined || blob.type !== 'blob') notFound();
  return ok({ sha: params[2], size: blob.data.length, encoding: 'base64', content: blob.data.toString('base64') });
});

// ---- contents

function contentEntry(repo: Repo, path: string, mode: string, sha: string, base: string, ref: string): Record<string, unknown> {
  const name = path.split('/').pop() ?? path;
  const isDir = mode === '40000';
  const api = `${base}/api/v3/repos/${repo.fullName}/contents/${path}?ref=${encodeURIComponent(ref)}`;
  return {
    type: isDir ? 'dir' : 'file',
    name,
    path,
    sha,
    size: isDir ? 0 : (repo.get(sha)?.data.length ?? 0),
    url: api,
    html_url: `${base}/${repo.fullName}/blob/${ref}/${path}`,
    git_url: `${base}/api/v3/repos/${repo.fullName}/git/${isDir ? 'trees' : 'blobs'}/${sha}`,
    download_url: isDir ? null : `${base}/raw/${repo.fullName}/${ref}/${path}`,
    _links: { self: api },
  };
}

function fileJson(repo: Repo, path: string, sha: string, base: string, ref: string): Record<string, unknown> {
  const blob = repo.get(sha);
  return { ...contentEntry(repo, path, '100644', sha, base, ref), encoding: 'base64', content: blob === undefined ? '' : blob.data.toString('base64') };
}

route('GET', '/repos/{owner}/{repo}/contents', (req, params) => getContents(req, params, ''));
route('GET', '/repos/{owner}/{repo}/contents/{path*}', (req, params) => getContents(req, params, params[2] ?? ''));
function getContents(req: Request, params: string[], rawPath: string): Reply {
  const repo = repoOf(params);
  const path = rawPath.replace(/^\/+|\/+$/g, '');
  const refName = req.query.get('ref') ?? repo.defaultBranch;
  const sha = resolveRef(repo, refName);
  if (sha === undefined) {
    if (req.query.get('ref') === null) throw new HttpError(404, 'This repository is empty.');
    throw new HttpError(404, `No commit found for the ref ${refName}`);
  }
  const tree = repo.commit(sha)?.tree;
  if (tree === undefined) notFound();
  const entry = lookupPath(repo, tree, path);
  if (entry === null) notFound();
  if (entry.mode === '40000') {
    return ok(repo.tree(entry.oid).map((child) => contentEntry(repo, path === '' ? child.name : `${path}/${child.name}`, child.mode, child.oid, req.base, refName)));
  }
  return ok(fileJson(repo, path, entry.oid, req.base, refName));
}

function commitSummary(repo: Repo, sha: string, base: string): Record<string, unknown> {
  const commit = repo.commit(sha);
  const api = `${base}/api/v3/repos/${repo.fullName}`;
  return {
    sha,
    node_id: `C_${sha}`,
    url: `${api}/git/commits/${sha}`,
    html_url: `${base}/${repo.fullName}/commit/${sha}`,
    message: commit?.message.replace(/\n$/, '') ?? '',
    author: commit === null ? null : parseIdent(commit.author),
    committer: commit === null ? null : parseIdent(commit.committer),
    tree: { sha: commit?.tree ?? '' },
    parents: (commit?.parents ?? []).map((parent) => ({ sha: parent })),
  };
}

route('PUT', '/repos/{owner}/{repo}/contents/{path*}', (req, params) => {
  const repo = repoOf(params);
  const path = (params[2] ?? '').replace(/^\/+|\/+$/g, '');
  const body = obj(req.body);
  const message = str(body.message);
  const content = str(body.content);
  if (message === undefined) invalid('Invalid request.\n\n"message" wasn\'t supplied.');
  if (content === undefined) invalid('Invalid request.\n\n"content" wasn\'t supplied.');
  if (path === '') invalid('Invalid request.\n\npath is empty.');
  const branch = str(body.branch) ?? repo.defaultBranch;
  let tip = repo.branchTip(branch);
  if (tip === undefined && str(body.branch) !== undefined && repo.refs.size > 0) {
    // A write to a new branch starts it from the default branch, as a convenience.
    const from = repo.branchTip(repo.defaultBranch);
    if (from === undefined) throw new HttpError(404, `Branch ${branch} not found`);
    repo.setRef(`refs/heads/${branch}`, from);
    tip = from;
  }
  const tree = tip === undefined ? undefined : repo.commit(tip)?.tree;
  const existing = tree === undefined ? null : lookupPath(repo, tree, path);
  if (existing !== null && existing.mode === '40000') invalid('A directory exists at this path');
  const given = str(body.sha);
  if (existing !== null) {
    if (given === undefined) invalid('Invalid request.\n\n"sha" wasn\'t supplied.');
    if (given !== existing.oid) throw new HttpError(409, `${path} does not match ${given}`);
  } else if (given !== undefined && tip !== undefined) {
    throw new HttpError(409, `${path} does not match ${given}`);
  }
  const bytes = Buffer.from(content, 'base64');
  const made = commitFile(repo, branch, path, bytes, message);
  const blobSha = made.blob as string;
  return ok({ content: fileJson(repo, path, blobSha, req.base, branch), commit: commitSummary(repo, made.commit, req.base) }, existing === null ? 201 : 200);
});

route('DELETE', '/repos/{owner}/{repo}/contents/{path*}', (req, params) => {
  const repo = repoOf(params);
  const path = (params[2] ?? '').replace(/^\/+|\/+$/g, '');
  const body = obj(req.body);
  const message = str(body.message) ?? `Delete ${path}`;
  const branch = str(body.branch) ?? repo.defaultBranch;
  const tip = repo.branchTip(branch);
  if (tip === undefined) notFound();
  const tree = repo.commit(tip)?.tree;
  const existing = tree === undefined ? null : lookupPath(repo, tree, path);
  if (existing === null || existing.mode === '40000') notFound();
  const given = str(body.sha);
  if (given === undefined) invalid('Invalid request.\n\n"sha" wasn\'t supplied.');
  if (given !== existing.oid) throw new HttpError(409, `${path} does not match ${given}`);
  const made = commitFile(repo, branch, path, null, message);
  return ok({ content: null, commit: commitSummary(repo, made.commit, req.base) });
});

// ---- labels

route('GET', '/repos/{owner}/{repo}/labels', (req, params) => {
  const repo = repoOf(params);
  return ok([...repo.labels.values()].map((label) => labelJson(label, repo, req.base)));
});
route('POST', '/repos/{owner}/{repo}/labels', (req, params) => {
  const repo = repoOf(params);
  const body = obj(req.body);
  const name = str(body.name);
  if (name === undefined || name.trim() === '') invalid('Validation Failed', [{ resource: 'Label', field: 'name', code: 'missing_field' }]);
  const label = addLabel(repo, name, (str(body.color) ?? 'ededed').replace(/^#/, ''), str(body.description) ?? null);
  if (label === null) invalid('Validation Failed', [{ resource: 'Label', field: 'name', code: 'already_exists' }]);
  return ok(labelJson(label, repo, req.base), 201);
});
route('GET', '/repos/{owner}/{repo}/labels/{name}', (req, params) => {
  const repo = repoOf(params);
  const label = repo.labels.get((params[2] ?? '').toLowerCase());
  if (label === undefined) notFound();
  return ok(labelJson(label, repo, req.base));
});

// ---- issues

function issueOf(repo: Repo, number: string | undefined): Issue {
  const issue = repo.issues.get(Number(number));
  if (issue === undefined) notFound();
  return issue;
}

export function sortIssues<T extends { number: number; updatedAt: string; comments: unknown[] }>(items: T[], sort: string, direction: string): T[] {
  const sign = direction === 'asc' ? 1 : -1;
  return items.sort((a, b) => {
    if (sort === 'updated' && a.updatedAt !== b.updatedAt) return sign * (a.updatedAt < b.updatedAt ? -1 : 1);
    if (sort === 'comments' && a.comments.length !== b.comments.length) return sign * (a.comments.length - b.comments.length);
    return sign * (a.number - b.number);
  });
}

route('GET', '/repos/{owner}/{repo}/issues', (req, params) => {
  const repo = repoOf(params);
  const state = req.query.get('state') ?? 'open';
  const labels = (req.query.get('labels') ?? '').split(',').map((label) => label.trim().toLowerCase()).filter(Boolean);
  let items = [...repo.issues.values()].filter((issue) => state === 'all' || issue.state === state);
  if (labels.length > 0) items = items.filter((issue) => labels.every((name) => issue.labels.some((label) => label.name.toLowerCase() === name)));
  items = sortIssues(items, req.query.get('sort') ?? 'created', req.query.get('direction') ?? 'desc');
  const { page, headers } = paginate(req, items, `/repos/${repo.fullName}/issues`);
  return { status: 200, text: `[${page.map((issue) => issueText(issue, req.base)).join(',')}]`, headers };
});
route('POST', '/repos/{owner}/{repo}/issues', (req, params) => {
  const repo = repoOf(params);
  const body = obj(req.body);
  const title = typeof body.title === 'number' ? String(body.title) : str(body.title);
  if (title === undefined || title === '') invalid('Validation Failed', [{ resource: 'Issue', field: 'title', code: 'missing_field' }]);
  let labels: Label[] = [];
  if (Array.isArray(body.labels)) {
    const resolved = resolveLabels(repo, body.labels);
    if ('missing' in resolved) invalid(`Validation Failed: the label ${JSON.stringify(resolved.missing)} does not exist`, [{ resource: 'Label', field: 'name', code: 'invalid', value: resolved.missing }]);
    labels = resolved.labels;
  }
  const issue = addIssue(repo, title, str(body.body) ?? '', labels);
  return { status: 201, text: issueText(issue, req.base) };
});
route('GET', '/repos/{owner}/{repo}/issues/{number}', (req, params) => {
  const repo = repoOf(params);
  const number = Number(params[2]);
  const issue = repo.issues.get(number);
  if (issue !== undefined) return { status: 200, text: issueText(issue, req.base) };
  const pull = repo.pulls.get(number);
  if (pull === undefined) notFound();
  return ok({ ...pullJson(pull, req.base, false), pull_request: { url: `${req.base}/api/v3/repos/${repo.fullName}/pulls/${pull.number}` }, comments: pull.comments.length });
});
route('PATCH', '/repos/{owner}/{repo}/issues/{number}', (req, params) => {
  const repo = repoOf(params);
  const issue = issueOf(repo, params[2]);
  const body = obj(req.body);
  if (typeof body.title === 'string') issue.title = body.title;
  if (typeof body.body === 'string' || body.body === null) issue.body = (body.body as string | null) ?? '';
  if (Array.isArray(body.labels)) {
    const resolved = resolveLabels(repo, body.labels);
    if ('missing' in resolved) invalid(`Validation Failed: the label ${JSON.stringify(resolved.missing)} does not exist`);
    issue.labels = resolved.labels;
  }
  if (body.state === 'open' || body.state === 'closed') setIssueState(issue, body.state);
  else if (body.state !== undefined) invalid('Validation Failed', [{ resource: 'Issue', field: 'state', code: 'invalid' }]);
  issue.updatedAt = nowIso();
  issue.json = undefined;
  return { status: 200, text: issueText(issue, req.base) };
});
route('GET', '/repos/{owner}/{repo}/issues/{number}/comments', (req, params) => {
  const repo = repoOf(params);
  const issue = issueOf(repo, params[2]);
  const { page, headers } = paginate(req, issue.comments, `/repos/${repo.fullName}/issues/${issue.number}/comments`);
  return ok(page.map((comment) => commentJson(comment, req.base)), 200, headers);
});
route('POST', '/repos/{owner}/{repo}/issues/{number}/comments', (req, params) => {
  const repo = repoOf(params);
  const issue = issueOf(repo, params[2]);
  const body = str(obj(req.body).body);
  if (body === undefined || body === '') invalid('Validation Failed', [{ resource: 'IssueComment', field: 'body', code: 'missing_field' }]);
  return ok(commentJson(addComment(issue, body), req.base), 201);
});
route('GET', '/repos/{owner}/{repo}/issues/{number}/labels', (req, params) => {
  const repo = repoOf(params);
  return ok(issueOf(repo, params[2]).labels.map((label) => labelJson(label, repo, req.base)));
});
route('POST', '/repos/{owner}/{repo}/issues/{number}/labels', (req, params) => {
  const repo = repoOf(params);
  const issue = issueOf(repo, params[2]);
  const raw = Array.isArray(req.body) ? req.body : obj(req.body).labels;
  const resolved = resolveLabels(repo, Array.isArray(raw) ? raw : []);
  if ('missing' in resolved) invalid(`Validation Failed: the label ${JSON.stringify(resolved.missing)} does not exist`);
  for (const label of resolved.labels) if (!issue.labels.includes(label)) issue.labels.push(label);
  issue.json = undefined;
  return ok(issue.labels.map((label) => labelJson(label, repo, req.base)));
});

// ---- pull requests

function pullOf(repo: Repo, number: string | undefined): Pull {
  const pull = repo.pulls.get(Number(number));
  if (pull === undefined) notFound();
  return pull;
}

/** `owner:branch` or `branch` -> branch. */
export function branchName(value: string): string {
  const colon = value.indexOf(':');
  return colon >= 0 ? value.slice(colon + 1) : value;
}

export function openPull(repo: Repo, title: string, body: string, headRaw: string, baseRaw: string): Pull {
  const head = branchName(headRaw);
  const base = branchName(baseRaw);
  if (repo.branchTip(head) === undefined) invalid('Validation Failed', [{ resource: 'PullRequest', field: 'head', code: 'invalid' }]);
  if (repo.branchTip(base) === undefined) invalid('Validation Failed', [{ resource: 'PullRequest', field: 'base', code: 'invalid' }]);
  for (const pull of repo.pulls.values()) {
    if (pull.state === 'open' && pull.head === head && pull.base === base) {
      invalid('Validation Failed', [{ resource: 'PullRequest', code: 'custom', message: `A pull request already exists for ${repo.owner.login}:${head}.` }]);
    }
  }
  return addPull(repo, title, body, head, base);
}

route('GET', '/repos/{owner}/{repo}/pulls', (req, params) => {
  const repo = repoOf(params);
  const state = req.query.get('state') ?? 'open';
  const head = req.query.get('head');
  const base = req.query.get('base');
  let items = [...repo.pulls.values()].filter((pull) => state === 'all' || pull.state === state);
  if (head !== null) items = items.filter((pull) => pull.head === branchName(head));
  if (base !== null) items = items.filter((pull) => pull.base === base);
  items = sortIssues(items, req.query.get('sort') ?? 'created', req.query.get('direction') ?? 'desc');
  const { page, headers } = paginate(req, items, `/repos/${repo.fullName}/pulls`);
  return ok(page.map((pull) => pullJson(pull, req.base, false)), 200, headers);
});
route('POST', '/repos/{owner}/{repo}/pulls', (req, params) => {
  const repo = repoOf(params);
  const body = obj(req.body);
  const title = str(body.title);
  const head = str(body.head);
  const base = str(body.base);
  if (title === undefined || title === '') invalid('Validation Failed', [{ resource: 'PullRequest', field: 'title', code: 'missing_field' }]);
  if (head === undefined) invalid('Validation Failed', [{ resource: 'PullRequest', field: 'head', code: 'missing_field' }]);
  if (base === undefined) invalid('Validation Failed', [{ resource: 'PullRequest', field: 'base', code: 'missing_field' }]);
  return ok(pullJson(openPull(repo, title, str(body.body) ?? '', head, base), req.base), 201);
});
route('GET', '/repos/{owner}/{repo}/pulls/{number}', (req, params) => ok(pullJson(pullOf(repoOf(params), params[2]), req.base)));
route('PATCH', '/repos/{owner}/{repo}/pulls/{number}', (req, params) => {
  const pull = pullOf(repoOf(params), params[2]);
  const body = obj(req.body);
  if (typeof body.title === 'string') pull.title = body.title;
  if (typeof body.body === 'string') pull.body = body.body;
  if (body.state === 'open' || body.state === 'closed') setPullState(pull, body.state);
  pull.updatedAt = nowIso();
  return ok(pullJson(pull, req.base));
});
route('GET', '/repos/{owner}/{repo}/pulls/{number}/files', (req, params) => {
  const repo = repoOf(params);
  const pull = pullOf(repo, params[2]);
  const files = pullFiles(pull).map((file) => ({
    sha: file.sha,
    filename: file.filename,
    status: file.status,
    additions: file.additions,
    deletions: file.deletions,
    changes: file.changes,
    blob_url: `${req.base}/${repo.fullName}/blob/${pullHeadSha(pull)}/${file.filename}`,
    raw_url: `${req.base}/${repo.fullName}/raw/${pullHeadSha(pull)}/${file.filename}`,
    contents_url: `${req.base}/api/v3/repos/${repo.fullName}/contents/${file.filename}?ref=${pullHeadSha(pull)}`,
  }));
  const { page, headers } = paginate(req, files, `/repos/${repo.fullName}/pulls/${pull.number}/files`);
  return ok(page, 200, headers);
});
route('GET', '/repos/{owner}/{repo}/pulls/{number}/commits', (req, params) => {
  const repo = repoOf(params);
  const pull = pullOf(repo, params[2]);
  const head = pullHeadSha(pull);
  return ok(head === null ? [] : [commitJson(repo, head, req.base)]);
});
route('GET', '/repos/{owner}/{repo}/pulls/{number}/merge', (_req, params) => {
  const pull = pullOf(repoOf(params), params[2]);
  if (!pull.merged) notFound();
  return { status: 204 };
});
route('PUT', '/repos/{owner}/{repo}/pulls/{number}/merge', (_req, params) => {
  const pull = pullOf(repoOf(params), params[2]);
  const body = obj(_req.body);
  const expected = str(body.sha);
  if (expected !== undefined && expected !== pullHeadSha(pull)) throw new HttpError(409, 'Head branch was modified. Review and try the merge again.');
  const result = mergePull(pull, str(body.merge_method) ?? 'merge', str(body.commit_title), str(body.commit_message));
  if (!result.ok) throw new HttpError(result.status, result.message);
  return ok({ sha: result.sha, merged: true, message: 'Pull Request successfully merged' });
});
