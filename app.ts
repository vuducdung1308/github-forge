// The forge's HTTP surface: one node:http server routing to git smart HTTP, REST v3 (/api/v3),
// GraphQL v4 (/api/graphql) and MCP (/mcp). main.ts owns the lifecycle; this file owns the server.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { HttpError, type Reply, errorBody, handleRest } from './src/rest.ts';
import { infoRefs, receivePack, uploadPack } from './src/smart-http.ts';
import { findRepo } from './src/store.ts';
import { handleGraphql } from './src/graphql.ts';
import { handleMcp } from './src/mcp.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const SCOPES = 'repo, read:org, workflow, gist';
let requestCounter = 0;

/** The token the trial hands us; with none set (local runs), any bearer token is accepted. */
const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';

/** The bearer (or token / basic) credential a request carries, or null. */
function credential(req: IncomingMessage): string | null {
  const header = req.headers.authorization;
  if (typeof header !== 'string' || header === '') return null;
  const space = header.indexOf(' ');
  const scheme = (space < 0 ? header : header.slice(0, space)).toLowerCase();
  const value = space < 0 ? '' : header.slice(space + 1).trim();
  if (scheme === 'bearer' || scheme === 'token') return value;
  if (scheme === 'basic') {
    const decoded = Buffer.from(value, 'base64').toString('utf8');
    const colon = decoded.indexOf(':');
    return colon < 0 ? decoded : decoded.slice(colon + 1);
  }
  return null;
}

export function authorized(req: IncomingMessage): boolean {
  const token = credential(req);
  if (token === null || token === '') return false;
  return TOKEN === '' || token === TOKEN;
}

function requestId(): string {
  requestCounter += 1;
  return `F0RG:${process.pid.toString(16).toUpperCase()}:${requestCounter.toString(16).toUpperCase().padStart(8, '0')}`;
}

function send(res: ServerResponse, status: number, body: string | Buffer, headers: Record<string, string>): void {
  const bytes = typeof body === 'string' ? Buffer.from(body, 'utf8') : body;
  res.writeHead(status, { 'content-length': String(bytes.length), 'x-github-request-id': requestId(), ...headers });
  res.end(bytes);
}

export function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  send(res, status, status === 204 ? '' : JSON.stringify(body), { 'content-type': JSON_TYPE, ...headers });
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (chunk: Buffer) => chunks.push(chunk));
    req.on('end', () => resolve(chunks.length === 1 ? (chunks[0] as Buffer) : Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function baseOf(req: IncomingMessage): string {
  return `http://${req.headers.host ?? 'localhost'}`;
}

const API_HEADERS = { 'x-oauth-scopes': SCOPES, 'x-accepted-oauth-scopes': 'repo', 'x-github-media-type': 'github.v3; format=json' };

function restReply(res: ServerResponse, reply: Reply): void {
  const headers = { ...API_HEADERS, ...(reply.headers ?? {}) };
  if (reply.status === 204) {
    send(res, 204, '', { ...headers });
    return;
  }
  if (reply.text !== undefined) send(res, reply.status, reply.text, { 'content-type': JSON_TYPE, ...headers });
  else sendJson(res, reply.status, reply.body ?? {}, headers);
}

async function handleApi(req: IncomingMessage, res: ServerResponse, url: URL): Promise<void> {
  if (!authorized(req)) {
    sendJson(res, 401, errorBody(401, 'Requires authentication'), { 'www-authenticate': 'Bearer realm="forge"' });
    return;
  }
  const raw = await readBody(req);
  let body: unknown;
  if (raw.length > 0) {
    try {
      body = JSON.parse(raw.toString('utf8'));
    } catch {
      sendJson(res, 400, errorBody(400, 'Problems parsing JSON'), API_HEADERS);
      return;
    }
  }
  try {
    const reply = handleRest({ method: req.method ?? 'GET', path: url.pathname.slice('/api/v3'.length), query: url.searchParams, body, base: baseOf(req) });
    restReply(res, reply);
  } catch (err) {
    if (err instanceof HttpError) sendJson(res, err.status, errorBody(err.status, err.message, err.extra), API_HEADERS);
    else throw err;
  }
}

const GIT_PATH = /^\/([A-Za-z0-9._-]+)\/([A-Za-z0-9._-]+?)(?:\.git)?\/(info\/refs|git-upload-pack|git-receive-pack)$/;

async function handleGit(req: IncomingMessage, res: ServerResponse, url: URL, match: RegExpExecArray): Promise<void> {
  const body = await readBody(req);
  if (!authorized(req)) {
    send(res, 401, 'Authentication required\n', { 'content-type': 'text/plain; charset=utf-8', 'www-authenticate': 'Basic realm="forge"' });
    return;
  }
  const owner = match[1] ?? '';
  const repo = findRepo(owner, match[2] ?? '');
  if (repo === undefined || repo.owner.login.toLowerCase() !== owner.toLowerCase()) {
    send(res, 404, 'Repository not found\n', { 'content-type': 'text/plain; charset=utf-8' });
    return;
  }
  const action = match[3];
  const v2 = /version=2/.test(String(req.headers['git-protocol'] ?? ''));
  let reply;
  if (action === 'info/refs' && req.method === 'GET') {
    const service = url.searchParams.get('service') ?? '';
    if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
      send(res, 403, 'dumb protocol not supported\n', { 'content-type': 'text/plain; charset=utf-8' });
      return;
    }
    reply = infoRefs(repo, service, v2);
  } else if (action === 'git-upload-pack' && req.method === 'POST') {
    reply = uploadPack(repo, await gunzipIfNeeded(req, body));
  } else if (action === 'git-receive-pack' && req.method === 'POST') {
    reply = receivePack(repo, await gunzipIfNeeded(req, body));
  } else {
    send(res, 405, 'Method Not Allowed\n', { 'content-type': 'text/plain; charset=utf-8' });
    return;
  }
  send(res, reply.status, reply.body, { 'content-type': reply.type, 'cache-control': 'no-cache' });
}

async function gunzipIfNeeded(req: IncomingMessage, body: Buffer): Promise<Buffer> {
  if (String(req.headers['content-encoding'] ?? '').toLowerCase() !== 'gzip') return body;
  const { gunzipSync } = await import('node:zlib');
  return gunzipSync(body);
}

async function route(req: IncomingMessage, res: ServerResponse): Promise<void> {
  let url: URL;
  try {
    url = new URL(req.url ?? '/', 'http://forge');
  } catch {
    sendJson(res, 400, { message: 'Bad Request' });
    return;
  }
  const path = url.pathname;
  if (path === '/healthz') {
    sendJson(res, 200, { ok: true });
    return;
  }
  if (path === '/api/v3' || path.startsWith('/api/v3/')) {
    await handleApi(req, res, url);
    return;
  }
  if (path === '/api/graphql' || path === '/graphql') {
    const body = await readBody(req);
    const reply = handleGraphql(req.method ?? 'GET', authorized(req), body, baseOf(req));
    sendJson(res, reply.status, reply.body, API_HEADERS);
    return;
  }
  if (path === '/mcp' || path === '/mcp/') {
    const body = await readBody(req);
    const reply = handleMcp(req.method ?? 'GET', authorized(req), body, baseOf(req));
    if (reply.body === null) send(res, reply.status, '', reply.headers ?? {});
    else sendJson(res, reply.status, reply.body, reply.headers ?? {});
    return;
  }
  const git = GIT_PATH.exec(path);
  if (git !== null) {
    await handleGit(req, res, url, git);
    return;
  }
  await readBody(req);
  sendJson(res, 404, errorBody(404, 'Not Found'));
}

/** Build the server. It binds nothing until the caller listens; the entry listens on an ephemeral port. */
export function createApp(): Server {
  const server = createServer((req: IncomingMessage, res: ServerResponse) => {
    route(req, res).catch((err: unknown) => {
      process.stderr.write(`[forge] ${req.method} ${req.url}: ${err instanceof Error ? (err.stack ?? err.message) : String(err)}\n`);
      if (!res.headersSent) sendJson(res, 500, errorBody(500, 'Internal Server Error'));
      else res.destroy();
    });
  });
  server.keepAliveTimeout = 120_000;
  server.headersTimeout = 125_000;
  server.requestTimeout = 0;
  return server;
}
