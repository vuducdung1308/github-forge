// Model Context Protocol at /mcp: JSON-RPC 2.0 over Streamable HTTP, answering application/json.
// Speaks 2026-07-28 (server/discover, no handshake) and the handshake revisions through initialize.
// The tools act on the same store as REST, through the REST handlers themselves.

import { HttpError, handleRest } from './rest.ts';

const SERVER_INFO = { name: 'forge-mcp', title: 'Forge MCP', version: '1.0.0' };
const MODERN = '2026-07-28';
const SUPPORTED = [MODERN, '2025-11-25', '2025-06-18', '2025-03-26', '2024-11-05'];

interface Tool {
  name: string;
  description: string;
  inputSchema: { type: 'object'; properties: Record<string, unknown>; required: string[] };
  run: (args: Record<string, unknown>, base: string) => unknown;
}

const S = (description: string) => ({ type: 'string', description });
const N = (description: string) => ({ type: 'number', description });
const B = (description: string) => ({ type: 'boolean', description });

function call(method: string, path: string, base: string, body?: unknown, query: Record<string, string> = {}): unknown {
  const reply = handleRest({ method, path, query: new URLSearchParams(query), body, base });
  if (reply.text !== undefined) return JSON.parse(reply.text);
  return reply.body ?? {};
}

const seg = (value: unknown): string => encodeURIComponent(String(value ?? ''));
const repoPath = (a: Record<string, unknown>): string => `/repos/${seg(a.owner)}/${seg(a.repo)}`;

const TOOLS: Tool[] = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: { type: 'object', properties: { name: S('Repository name'), description: S('Repository description'), private: B('Whether the repository is private'), autoInit: B('Initialize with a README') }, required: ['name'] },
    run: (a, base) => call('POST', '/user/repos', base, { name: a.name, description: a.description, private: a.private === true, auto_init: a.autoInit === true }),
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: { type: 'object', properties: { owner: S('Repository owner'), repo: S('Repository name') }, required: ['owner', 'repo'] },
    run: (a, base) => call('GET', repoPath(a), base),
  },
  {
    name: 'list_repos',
    description: 'List the repositories of the authenticated user.',
    inputSchema: { type: 'object', properties: { page: N('Page number'), perPage: N('Results per page') }, required: [] },
    run: (a, base) => call('GET', '/user/repos', base, undefined, { page: String(a.page ?? 1), per_page: String(a.perPage ?? 30) }),
  },
  {
    name: 'create_issue',
    description: 'Create a new issue in a repository.',
    inputSchema: { type: 'object', properties: { owner: S('Repository owner'), repo: S('Repository name'), title: S('Issue title'), body: S('Issue body'), labels: { type: 'array', items: { type: 'string' }, description: 'Labels to apply' } }, required: ['owner', 'repo', 'title'] },
    run: (a, base) => call('POST', `${repoPath(a)}/issues`, base, { title: a.title, body: a.body, labels: a.labels }),
  },
  {
    name: 'get_issue',
    description: 'Get one issue of a repository by number.',
    inputSchema: { type: 'object', properties: { owner: S('Repository owner'), repo: S('Repository name'), issue_number: N('Issue number') }, required: ['owner', 'repo', 'issue_number'] },
    run: (a, base) => call('GET', `${repoPath(a)}/issues/${seg(a.issue_number)}`, base),
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository, filtered by state.',
    inputSchema: { type: 'object', properties: { owner: S('Repository owner'), repo: S('Repository name'), state: { type: 'string', enum: ['open', 'closed', 'all'], description: 'Issue state filter' }, page: N('Page number'), perPage: N('Results per page') }, required: ['owner', 'repo'] },
    run: (a, base) => call('GET', `${repoPath(a)}/issues`, base, undefined, { state: String(a.state ?? 'open').toLowerCase(), page: String(a.page ?? 1), per_page: String(a.perPage ?? 100) }),
  },
  {
    name: 'update_issue',
    description: 'Update an issue: its title, body or state.',
    inputSchema: { type: 'object', properties: { owner: S('Repository owner'), repo: S('Repository name'), issue_number: N('Issue number'), title: S('New title'), body: S('New body'), state: { type: 'string', enum: ['open', 'closed'], description: 'New state' } }, required: ['owner', 'repo', 'issue_number'] },
    run: (a, base) => call('PATCH', `${repoPath(a)}/issues/${seg(a.issue_number)}`, base, { title: a.title, body: a.body, state: a.state }),
  },
  {
    name: 'add_issue_comment',
    description: 'Add a comment to an issue.',
    inputSchema: { type: 'object', properties: { owner: S('Repository owner'), repo: S('Repository name'), issue_number: N('Issue number'), body: S('Comment text') }, required: ['owner', 'repo', 'issue_number', 'body'] },
    run: (a, base) => call('POST', `${repoPath(a)}/issues/${seg(a.issue_number)}/comments`, base, { body: a.body }),
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch into a base branch.',
    inputSchema: { type: 'object', properties: { owner: S('Repository owner'), repo: S('Repository name'), title: S('Pull request title'), body: S('Pull request description'), head: S('Branch with the changes'), base: S('Branch to merge into') }, required: ['owner', 'repo', 'title', 'head', 'base'] },
    run: (a, base) => call('POST', `${repoPath(a)}/pulls`, base, { title: a.title, body: a.body, head: a.head, base: a.base }),
  },
  {
    name: 'get_pull_request',
    description: 'Get one pull request by number.',
    inputSchema: { type: 'object', properties: { owner: S('Repository owner'), repo: S('Repository name'), pull_number: N('Pull request number') }, required: ['owner', 'repo', 'pull_number'] },
    run: (a, base) => call('GET', `${repoPath(a)}/pulls/${seg(a.pull_number)}`, base),
  },
  {
    name: 'list_pull_requests',
    description: 'List the pull requests of a repository, filtered by state.',
    inputSchema: { type: 'object', properties: { owner: S('Repository owner'), repo: S('Repository name'), state: { type: 'string', enum: ['open', 'closed', 'all'], description: 'Pull request state filter' } }, required: ['owner', 'repo'] },
    run: (a, base) => call('GET', `${repoPath(a)}/pulls`, base, undefined, { state: String(a.state ?? 'open'), per_page: '100' }),
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request (merge, squash or rebase).',
    inputSchema: { type: 'object', properties: { owner: S('Repository owner'), repo: S('Repository name'), pull_number: N('Pull request number'), merge_method: { type: 'string', enum: ['merge', 'squash', 'rebase'], description: 'Merge method' }, commit_title: S('Title of the merge commit'), commit_message: S('Body of the merge commit') }, required: ['owner', 'repo', 'pull_number'] },
    run: (a, base) => call('PUT', `${repoPath(a)}/pulls/${seg(a.pull_number)}/merge`, base, { merge_method: a.merge_method ?? 'squash', commit_title: a.commit_title, commit_message: a.commit_message }),
  },
  {
    name: 'list_branches',
    description: 'List the branches of a repository.',
    inputSchema: { type: 'object', properties: { owner: S('Repository owner'), repo: S('Repository name') }, required: ['owner', 'repo'] },
    run: (a, base) => call('GET', `${repoPath(a)}/branches`, base, undefined, { per_page: '100' }),
  },
  {
    name: 'create_branch',
    description: 'Create a branch from another branch (the default branch when none is named).',
    inputSchema: { type: 'object', properties: { owner: S('Repository owner'), repo: S('Repository name'), branch: S('New branch name'), from_branch: S('Source branch') }, required: ['owner', 'repo', 'branch'] },
    run: (a, base) => {
      const repoJson = call('GET', repoPath(a), base) as { default_branch: string };
      const from = call('GET', `${repoPath(a)}/branches/${seg(a.from_branch ?? repoJson.default_branch)}`, base) as { commit: { sha: string } };
      return call('POST', `${repoPath(a)}/git/refs`, base, { ref: `refs/heads/${a.branch}`, sha: from.commit.sha });
    },
  },
  {
    name: 'get_file_contents',
    description: 'Get the contents of a file or directory at a ref.',
    inputSchema: { type: 'object', properties: { owner: S('Repository owner'), repo: S('Repository name'), path: S('Path in the repository'), ref: S('Branch, tag or commit') }, required: ['owner', 'repo', 'path'] },
    run: (a, base) => call('GET', `${repoPath(a)}/contents/${String(a.path ?? '').split('/').map(seg).join('/')}`, base, undefined, a.ref === undefined ? {} : { ref: String(a.ref) }),
  },
  {
    name: 'create_or_update_file',
    description: 'Create or update one file with a commit on a branch.',
    inputSchema: { type: 'object', properties: { owner: S('Repository owner'), repo: S('Repository name'), path: S('Path in the repository'), content: S('File content (plain text)'), message: S('Commit message'), branch: S('Branch to commit to'), sha: S('Blob sha of the file being replaced') }, required: ['owner', 'repo', 'path', 'content', 'message'] },
    run: (a, base) =>
      call('PUT', `${repoPath(a)}/contents/${String(a.path ?? '').split('/').map(seg).join('/')}`, base, {
        message: a.message,
        content: Buffer.from(String(a.content ?? ''), 'utf8').toString('base64'),
        branch: a.branch,
        sha: a.sha,
      }),
  },
];

/** The tools/list answer, serialized once so every call returns the same bytes. */
const TOOL_LIST = TOOLS.map(({ name, description, inputSchema }) => ({ name, description, inputSchema }));
const byName = new Map(TOOLS.map((tool) => [tool.name, tool]));

type RpcId = string | number | null;

interface Reply {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
}

function rpcError(id: RpcId, code: number, message: string, data?: unknown): Record<string, unknown> {
  return { jsonrpc: '2.0', id, error: data === undefined ? { code, message } : { code, message, data } };
}

function rpcResult(id: RpcId, result: unknown): Record<string, unknown> {
  return { jsonrpc: '2.0', id, result };
}

function toolText(value: unknown, isError = false): Record<string, unknown> {
  return { content: [{ type: 'text', text: typeof value === 'string' ? value : JSON.stringify(value) }], isError };
}

function runTool(params: Record<string, unknown>, id: RpcId, base: string): Record<string, unknown> {
  const name = params.name;
  if (typeof name !== 'string') return rpcError(id, -32602, 'Invalid params: tools/call needs a tool name');
  const tool = byName.get(name);
  if (tool === undefined) return rpcError(id, -32602, `Unknown tool: ${name}`);
  const args = typeof params.arguments === 'object' && params.arguments !== null && !Array.isArray(params.arguments) ? (params.arguments as Record<string, unknown>) : {};
  const missing = tool.inputSchema.required.filter((key) => args[key] === undefined || args[key] === null || args[key] === '');
  if (missing.length > 0) return rpcResult(id, toolText(`missing required argument(s): ${missing.join(', ')}`, true));
  try {
    return rpcResult(id, toolText(tool.run(args, base)));
  } catch (err) {
    if (err instanceof HttpError) return rpcResult(id, toolText(`${err.status}: ${err.message}`, true));
    return rpcResult(id, toolText(`tool failed: ${err instanceof Error ? err.message : String(err)}`, true));
  }
}

function dispatch(message: unknown, base: string): Record<string, unknown> | null {
  if (typeof message !== 'object' || message === null || Array.isArray(message)) return rpcError(null, -32600, 'Invalid Request');
  const msg = message as Record<string, unknown>;
  const id = (typeof msg.id === 'string' || typeof msg.id === 'number' ? msg.id : null) as RpcId;
  const isNotification = !('id' in msg);
  const method = msg.method;
  if (msg.jsonrpc !== '2.0' || typeof method !== 'string') return isNotification ? null : rpcError(id, -32600, 'Invalid Request');
  const params = typeof msg.params === 'object' && msg.params !== null ? (msg.params as Record<string, unknown>) : {};
  if (isNotification) return null;
  switch (method) {
    case 'server/discover':
      return rpcResult(id, {
        supportedVersions: SUPPORTED,
        capabilities: { tools: { listChanged: false } },
        serverInfo: SERVER_INFO,
        instructions: 'A GitHub-compatible forge: repositories, issues and pull requests.',
        _meta: { 'io.modelcontextprotocol/serverInfo': SERVER_INFO },
      });
    case 'initialize': {
      const asked = typeof params.protocolVersion === 'string' ? params.protocolVersion : '';
      return rpcResult(id, { protocolVersion: SUPPORTED.includes(asked) ? asked : '2025-11-25', capabilities: { tools: { listChanged: false } }, serverInfo: SERVER_INFO });
    }
    case 'ping':
      return rpcResult(id, {});
    case 'tools/list':
      return rpcResult(id, { tools: TOOL_LIST });
    case 'tools/call':
      return runTool(params, id, base);
    case 'resources/list':
      return rpcResult(id, { resources: [] });
    case 'prompts/list':
      return rpcResult(id, { prompts: [] });
    default:
      return rpcError(id, -32601, `Method not found: ${method}`);
  }
}

export function handleMcp(method: string, authed: boolean, raw: Buffer, base: string): Reply {
  if (!authed) return { status: 401, body: rpcError(null, -32001, 'Unauthorized: a Bearer token is required'), headers: { 'www-authenticate': 'Bearer realm="forge"' } };
  if (method === 'GET') return { status: 405, body: rpcError(null, -32000, 'This server opens no GET stream'), headers: { allow: 'POST, DELETE' } };
  if (method === 'DELETE') return { status: 204, body: null };
  if (method !== 'POST') return { status: 405, body: rpcError(null, -32000, 'Method not allowed'), headers: { allow: 'POST' } };
  let message: unknown;
  try {
    message = JSON.parse(raw.toString('utf8'));
  } catch {
    return { status: 400, body: rpcError(null, -32700, 'Parse error') };
  }
  if (Array.isArray(message)) {
    const answers = message.map((entry) => dispatch(entry, base)).filter((entry) => entry !== null);
    return answers.length === 0 ? { status: 202, body: null } : { status: 200, body: answers };
  }
  const answer = dispatch(message, base);
  if (answer === null) return { status: 202, body: null };
  return { status: 200, body: answer };
}
