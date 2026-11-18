// A minimal HTTP/1.1 server on node:net: keep-alive, pipelining, Content-Length and chunked request
// bodies, Expect: 100-continue, and one socket write per response. It replaces node:http on the hot
// path: no streams, no IncomingMessage, no per-header events.

import { type Server, type Socket, createServer } from 'node:net';

export interface HttpRequest {
  method: string;
  /** The request target as sent (path and query). */
  target: string;
  /** Header names lowercased; repeated headers joined with ", ". */
  headers: Record<string, string>;
  body: Buffer;
  /** The address the connection came from. */
  remote: string;
}

export interface HttpResponse {
  status: number;
  /** Header lines beyond Content-Length, already cased. */
  headers?: Record<string, string>;
  body?: Buffer | string;
}

export type Handler = (req: HttpRequest) => HttpResponse | Promise<HttpResponse>;

const REASONS: Record<number, string> = {
  100: 'Continue', 200: 'OK', 201: 'Created', 202: 'Accepted', 204: 'No Content', 304: 'Not Modified',
  400: 'Bad Request', 401: 'Unauthorized', 403: 'Forbidden', 404: 'Not Found', 405: 'Method Not Allowed',
  409: 'Conflict', 411: 'Length Required', 413: 'Payload Too Large', 422: 'Unprocessable Entity',
  500: 'Internal Server Error',
};

const MAX_HEADER_BYTES = 64 * 1024;
const MAX_BODY_BYTES = 256 * 1024 * 1024;
const CRLFCRLF = Buffer.from('\r\n\r\n');
const CONTINUE = Buffer.from('HTTP/1.1 100 Continue\r\n\r\n');

/** Serialize one response into a single buffer. */
function encode(res: HttpResponse, head: boolean, close: boolean): Buffer {
  const body = res.body === undefined ? Buffer.alloc(0) : typeof res.body === 'string' ? Buffer.from(res.body, 'utf8') : res.body;
  let text = `HTTP/1.1 ${res.status} ${REASONS[res.status] ?? 'Status'}\r\n`;
  if (res.headers !== undefined) for (const name in res.headers) text += `${name}: ${res.headers[name]}\r\n`;
  if (res.status !== 204 && res.status !== 304) text += `Content-Length: ${body.length}\r\n`;
  if (close) text += 'Connection: close\r\n';
  text += '\r\n';
  if (head || body.length === 0 || res.status === 204) return Buffer.from(text, 'latin1');
  if (body.length < 16384) {
    const out = Buffer.allocUnsafe(text.length + body.length);
    out.write(text, 0, 'latin1');
    body.copy(out, text.length);
    return out;
  }
  return Buffer.concat([Buffer.from(text, 'latin1'), body]);
}

class Connection {
  private buf: Buffer = Buffer.alloc(0);
  private busy = false;
  private closed = false;
  private readonly socket: Socket;
  private readonly handler: Handler;

  constructor(socket: Socket, handler: Handler) {
    this.socket = socket;
    this.handler = handler;
    socket.setNoDelay(true);
    socket.on('data', (chunk: Buffer) => {
      this.buf = this.buf.length === 0 ? chunk : Buffer.concat([this.buf, chunk]);
      this.pump();
    });
    socket.on('error', () => {
      this.closed = true;
    });
    socket.on('close', () => {
      this.closed = true;
    });
  }

  private fail(status: number): void {
    if (this.closed) return;
    this.closed = true;
    this.socket.end(encode({ status, headers: { 'Content-Type': 'application/json; charset=utf-8' }, body: `{"message":"${REASONS[status] ?? 'Bad Request'}"}` }, false, true));
  }

  /** Parse and answer every complete request in the buffer, one at a time. */
  private pump(): void {
    while (!this.busy && !this.closed) {
      const end = this.buf.indexOf(CRLFCRLF);
      if (end < 0) {
        if (this.buf.length > MAX_HEADER_BYTES) this.fail(431 as number);
        return;
      }
      const headText = this.buf.toString('latin1', 0, end);
      const lines = headText.split('\r\n');
      const requestLine = (lines[0] ?? '').split(' ');
      if (requestLine.length < 3 || requestLine[0] === '' || !(requestLine[2] ?? '').startsWith('HTTP/1.')) {
        this.fail(400);
        return;
      }
      const headers: Record<string, string> = {};
      for (let i = 1; i < lines.length; i += 1) {
        const line = lines[i] as string;
        const colon = line.indexOf(':');
        if (colon <= 0) continue;
        const name = line.slice(0, colon).trim().toLowerCase();
        const value = line.slice(colon + 1).trim();
        headers[name] = headers[name] === undefined ? value : `${headers[name]}, ${value}`;
      }
      const start = end + 4;
      let body: Buffer;
      let consumed: number;
      const te = headers['transfer-encoding'];
      if (te !== undefined && te.toLowerCase().includes('chunked')) {
        const chunked = readChunked(this.buf, start);
        if (chunked === 'bad') {
          this.fail(400);
          return;
        }
        if (chunked === null) {
          this.expectContinue(headers);
          return;
        }
        body = chunked.body;
        consumed = chunked.next;
      } else {
        const length = headers['content-length'] === undefined ? 0 : Number(headers['content-length']);
        if (!Number.isSafeInteger(length) || length < 0 || length > MAX_BODY_BYTES) {
          this.fail(length > MAX_BODY_BYTES ? 413 : 400);
          return;
        }
        if (this.buf.length < start + length) {
          this.expectContinue(headers);
          return;
        }
        body = this.buf.subarray(start, start + length);
        consumed = start + length;
      }
      this.buf = consumed === this.buf.length ? Buffer.alloc(0) : this.buf.subarray(consumed);
      const http10 = requestLine[2] === 'HTTP/1.0';
      const connection = (headers.connection ?? '').toLowerCase();
      const close = connection.includes('close') || (http10 && !connection.includes('keep-alive'));
      const req: HttpRequest = { method: requestLine[0] as string, target: requestLine[1] as string, headers, body, remote: this.socket.remoteAddress ?? '' };
      this.continued = false;
      let result: HttpResponse | Promise<HttpResponse>;
      try {
        result = this.handler(req);
      } catch (err) {
        result = internalError(err, req);
      }
      if (result instanceof Promise) {
        this.busy = true;
        result.then(
          (res) => this.respond(res, req, close),
          (err: unknown) => this.respond(internalError(err, req), req, close),
        ).finally(() => {
          this.busy = false;
          this.pump();
        });
        return;
      }
      this.respond(result, req, close);
    }
  }

  private continued = false;

  private expectContinue(headers: Record<string, string>): void {
    if (!this.continued && (headers.expect ?? '').toLowerCase() === '100-continue') {
      this.continued = true;
      this.socket.write(CONTINUE);
    }
  }

  private respond(res: HttpResponse, req: HttpRequest, close: boolean): void {
    if (this.closed) return;
    const bytes = encode(res, req.method === 'HEAD', close);
    if (close) {
      this.closed = true;
      this.socket.end(bytes);
    } else {
      this.socket.write(bytes);
    }
  }
}

function internalError(err: unknown, req: HttpRequest): HttpResponse {
  process.stderr.write(`[forge] ${req.method} ${req.target}: ${err instanceof Error ? (err.stack ?? err.message) : String(err)}\n`);
  return { status: 500, headers: { 'Content-Type': 'application/json; charset=utf-8' }, body: '{"message":"Internal Server Error"}' };
}

/** Decode a chunked body starting at `start`: null when incomplete, 'bad' when malformed. */
function readChunked(buf: Buffer, start: number): { body: Buffer; next: number } | null | 'bad' {
  const parts: Buffer[] = [];
  let pos = start;
  for (;;) {
    const lineEnd = buf.indexOf('\r\n', pos, 'latin1');
    if (lineEnd < 0) return null;
    const size = Number.parseInt(buf.toString('latin1', pos, lineEnd).split(';')[0] ?? '', 16);
    if (!Number.isFinite(size) || size < 0) return 'bad';
    pos = lineEnd + 2;
    if (size === 0) {
      // trailers end at an empty line
      for (;;) {
        const trailerEnd = buf.indexOf('\r\n', pos, 'latin1');
        if (trailerEnd < 0) return null;
        const empty = trailerEnd === pos;
        pos = trailerEnd + 2;
        if (empty) return { body: Buffer.concat(parts), next: pos };
      }
    }
    if (buf.length < pos + size + 2) return null;
    parts.push(buf.subarray(pos, pos + size));
    pos += size + 2;
  }
}

export function createHttpServer(handler: Handler): Server {
  return createServer({ noDelay: true, keepAlive: true }, (socket) => {
    new Connection(socket, handler);
  });
}
