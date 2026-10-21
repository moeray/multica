import { request as httpRequest, type IncomingMessage } from "node:http";
import { request as httpsRequest } from "node:https";
import { Readable } from "node:stream";
import type { ReadableStream as NodeReadableStream } from "node:stream/web";

import { resolveRemoteApiUrl } from "../../../config/runtime-urls";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// Uploads can be up to 2 GiB. Keep them off the proxy.ts -> next.config
// rewrite path: Next's middleware body clone is capped at 10 MiB by default
// and buffers the body, which truncated large uploads and surfaced as a 500.
// This route streams the request straight through to the Go API instead.
const HOP_BY_HOP_HEADERS = new Set([
  "connection",
  "keep-alive",
  "proxy-authenticate",
  "proxy-authorization",
  "te",
  "trailer",
  "transfer-encoding",
  "upgrade",
  "host",
]);

function backendBaseUrl(): string {
  return resolveRemoteApiUrl(process.env) ?? "http://localhost:8080";
}

export async function POST(request: Request): Promise<Response> {
  if (!request.body) {
    return Response.json({ error: "missing request body" }, { status: 400 });
  }

  const headers: Record<string, string> = {};
  request.headers.forEach((value, key) => {
    if (!HOP_BY_HOP_HEADERS.has(key.toLowerCase())) {
      headers[key] = value;
    }
  });

  const target = new URL(`${backendBaseUrl()}/api/upload-file`);
  const send = target.protocol === "https:" ? httpsRequest : httpRequest;
  const upstreamRequest = send(target, { method: "POST", headers });
  // Undici's fetch defaults to a 300s headers timeout, which cannot cover a
  // multi-gigabyte body on a slow link. Node's raw request has no such timer.
  upstreamRequest.setTimeout(0);

  const abort = () => upstreamRequest.destroy(new Error("client aborted upload"));
  request.signal.addEventListener("abort", abort, { once: true });

  const responsePromise = new Promise<IncomingMessage>((resolve, reject) => {
    upstreamRequest.once("response", resolve);
    upstreamRequest.once("error", reject);
  });

  Readable.fromWeb(
    request.body as unknown as NodeReadableStream<Uint8Array>,
  ).pipe(upstreamRequest);

  let upstream: IncomingMessage;
  try {
    upstream = await responsePromise;
  } catch (error) {
    request.signal.removeEventListener("abort", abort);
    console.error("upload proxy failed", error);
    return Response.json({ error: "upload proxy failed" }, { status: 502 });
  }
  upstream.once("close", () => request.signal.removeEventListener("abort", abort));

  const responseHeaders = new Headers();
  const contentType = upstream.headers["content-type"];
  if (typeof contentType === "string") {
    responseHeaders.set("content-type", contentType);
  }
  responseHeaders.set("cache-control", "no-store");

  return new Response(Readable.toWeb(upstream) as unknown as ReadableStream, {
    status: upstream.statusCode ?? 502,
    headers: responseHeaders,
  });
}
