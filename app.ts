// The forge's HTTP surface: one raw HTTP/1.1 server (src/httpd.ts) routing to git smart HTTP,
// REST v3 (/api/v3), GraphQL v4 (/api/graphql) and MCP (/mcp). main.ts owns the lifecycle.

import type { Server } from 'node:net';
import { gunzipSync } from 'node:zlib';
import { type HttpRequest, type HttpResponse, createHttpServer } from './src/httpd.ts';
import { HttpError, type Reply, errorBody, handleRest } from './src/rest.ts';
import { infoRefs, receivePack, uploadPack } from './src/smart-http.ts';
import { findRepo } from './src/store.ts';
import { handleGraphql } from './src/graphql.ts';
import { handleMcp } from './src/mcp.ts';

const JSON_TYPE = 'application/json; charset=utf-8';
const SCOPES = 'repo, read:org, workflow, gist';

/** The token the trial hands us; with none set (local runs), any bearer token is accepted. */
const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';
const BEARER = `Bearer ${TOKEN}`;

let requestCounter = 0;
const ID_PREFIX = `F0RG:${process.pid.toString(16).toUpperCase()}:`;
function requestId(): string {
  requestCounter += 1;
  return ID_PREFIX + requestCounter.toString(16).toUpperCase();
}

/** The bearer (or token / basic) credential a request carries, or null. */
function credential(header: string | undefined): string | null {
  if (header === undefined || header === '') return null;
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

function authorized(req: HttpRequest): boolean {
  const header = req.headers.authorization;
  if (TOKEN !== '' && header === BEARER) return true; // the hot path: one string compare
  const token = credential(header);
  if (token === null || token === '') return false;
  return TOKEN === '' || token === TOKEN;
}

function json(status: number, body: unknown, extra?: Record<string, string>): HttpResponse {
  const headers: Record<string, string> = { 'Content-Type': JSON_TYPE, 'X-GitHub-Request-Id': requestId() };
  if (extra !== undefined) Object.assign(headers, extra);
  return { status, headers, body: status === 204 ? undefined : typeof body === 'string' ? body : JSON.stringify(body) };
}

function text(status: number, body: string, extra?: Record<string, string>): HttpResponse {
  return { status, headers: { 'Content-Type': 'text/plain; charset=utf-8', 'X-GitHub-Request-Id': requestId(), ...extra }, body };
}

/** The absolute origin the client used, for Link headers and URLs in bodies. */
function baseOf(req: HttpRequest): string {
  return `http://${req.headers.host ?? 'localhost'}`;
}

function splitTarget(target: string): { path: string; query: string } {
  const q = target.indexOf('?');
  let path = q < 0 ? target : target.slice(0, q);
  if (path.startsWith('http://') || path.startsWith('https://')) {
    const slash = path.indexOf('/', path.indexOf('//') + 2);
    path = slash < 0 ? '/' : path.slice(slash);
  }
  return { path, query: q < 0 ? '' : target.slice(q + 1) };
}

const EMPTY_QUERY = new URLSearchParams();

function handleApi(req: HttpRequest, path: string, query: string): HttpResponse {
  if (!authorized(req)) return json(401, errorBody(401, 'Requires authentication'), { 'WWW-Authenticate': 'Bearer realm="forge"' });
  let body: unknown;
  if (req.body.length > 0) {
    try {
      body = JSON.parse(req.body.toString('utf8'));
    } catch {
      return json(400, errorBody(400, 'Problems parsing JSON'));
    }
  }
  const sub = path.slice(7); // after '/api/v3'
  try {
    const reply: Reply = handleRest({ method: req.method === 'HEAD' ? 'GET' : req.method, path: sub, query: query === '' ? EMPTY_QUERY : new URLSearchParams(query), body, base: baseOf(req) });
    const extra = reply.headers ?? {};
    if (sub === '' || sub === '/') extra['X-OAuth-Scopes'] = SCOPES;
    if (reply.status === 204) return json(204, null, extra);
    return json(reply.status, reply.text ?? JSON.stringify(reply.body ?? {}), extra);
  } catch (err) {
    if (err instanceof HttpError) return json(err.status, errorBody(err.status, err.message, err.extra));
    throw err;
  }
}

const GIT_PATH = /^\/([A-Za-z0-9._-]+)\/([A-Za-z0-9._-]+?)(?:\.git)?\/(info\/refs|git-upload-pack|git-receive-pack)$/;

function handleGit(req: HttpRequest, match: RegExpExecArray, query: string): HttpResponse {
  if (!authorized(req)) return text(401, 'Authentication required\n', { 'WWW-Authenticate': 'Basic realm="forge"' });
  const owner = match[1] ?? '';
  const repo = findRepo(owner, match[2] ?? '');
  if (repo === undefined || repo.owner.login.toLowerCase() !== owner.toLowerCase()) return text(404, 'Repository not found\n');
  const action = match[3];
  let reply;
  if (action === 'info/refs' && (req.method === 'GET' || req.method === 'HEAD')) {
    const service = new URLSearchParams(query).get('service') ?? '';
    if (service !== 'git-upload-pack' && service !== 'git-receive-pack') return text(403, 'dumb protocol not supported\n');
    reply = infoRefs(repo, service, (req.headers['git-protocol'] ?? '').includes('version=2'));
  } else if (action === 'git-upload-pack' && req.method === 'POST') {
    reply = uploadPack(repo, inflateBody(req));
  } else if (action === 'git-receive-pack' && req.method === 'POST') {
    reply = receivePack(repo, inflateBody(req));
  } else {
    return text(405, 'Method Not Allowed\n');
  }
  return { status: reply.status, headers: { 'Content-Type': reply.type, 'Cache-Control': 'no-cache' }, body: reply.body };
}

function inflateBody(req: HttpRequest): Buffer {
  return (req.headers['content-encoding'] ?? '').toLowerCase() === 'gzip' ? gunzipSync(req.body) : req.body;
}

function route(req: HttpRequest): HttpResponse {
  const { path, query } = splitTarget(req.target);
  if (path.startsWith('/api/v3') && (path.length === 7 || path.charCodeAt(7) === 47)) return handleApi(req, path, query);
  if (path === '/healthz') return json(200, '{"ok":true}');
  if (path === '/api/graphql' || path === '/graphql') {
    const reply = handleGraphql(req.method, authorized(req), req.body, baseOf(req));
    return json(reply.status, reply.body);
  }
  if (path === '/mcp' || path === '/mcp/') {
    const reply = handleMcp(req.method, authorized(req), req.body, baseOf(req));
    if (reply.body === null) return { status: reply.status, headers: reply.headers };
    return json(reply.status, reply.body, reply.headers);
  }
  const git = GIT_PATH.exec(path);
  if (git !== null) return handleGit(req, git, query);
  return json(404, errorBody(404, 'Not Found'));
}

/** Build the server. It binds nothing until the caller listens; the entry listens on an ephemeral port. */
export function createApp(): Server {
  return createHttpServer(route);
}
