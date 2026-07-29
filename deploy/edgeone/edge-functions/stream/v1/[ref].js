const FORWARDED_RESPONSE_HEADERS = [
  "Accept-Ranges",
  "Cache-Control",
  "Content-Disposition",
  "Content-Length",
  "Content-Range",
  "Content-Type",
  "ETag",
  "Last-Modified",
];

export async function onRequest(context) {
  if (context.request.method !== "GET") {
    return jsonError("method_not_allowed", "method not allowed", 405);
  }
  const requestURL = new URL(context.request.url);
  const ref = trackRef(context, requestURL);
  const ticket = requestURL.searchParams.get("ticket") || "";
  let control;
  try {
    const response = await fetch(
      new URL("/yuzu-edge/introspect", requestURL),
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ track_ref: ref, ticket }),
        eo: timeoutSettings(10_000, 20_000),
      },
    );
    if (!response.ok) {
      return copyResponse(response, "control-rejected");
    }
    control = await response.json();
  } catch (error) {
    return jsonError(
      "control_unavailable",
      error?.message || "control request failed",
      502,
    );
  }

  if (control?.ready && control?.signed_url) {
    const blobResponse = await tryBlob(context, control.signed_url, ref, ticket);
    if (blobResponse) {
      return blobResponse;
    }
  }
  return fallback(context, control?.origin_url, ref, ticket);
}

async function tryBlob(context, signedURL, ref, ticket) {
  const started = Date.now();
  try {
    const blobHeaders = requestHeaders(context.request);
    const response = await fetch(signedURL, {
      headers: blobHeaders,
      eo: timeoutSettings(10_000, 60_000),
    });
    if (![200, 206, 304, 416].includes(response.status)) {
      return null;
    }
    const headers = filteredHeaders(response.headers);
    headers.set("X-Yuzu-Distribution", "edgeone-blob");
    reportEvent(context, ref, ticket, "blob_served", {
      duration_ms: Date.now() - started,
      bytes: responseLength(response.headers),
    });
    return new Response(response.body, {
      status: response.status,
      statusText: response.statusText,
      headers,
    });
  } catch (error) {
    console.warn("Blob proxy failed", error?.message || error);
    return null;
  }
}

async function fallback(context, rawOriginURL, ref, ticket) {
  let originURL;
  try {
    originURL = new URL(rawOriginURL);
    if (originURL.host === new URL(context.request.url).host) {
      throw new Error("origin would recurse through EdgeOne");
    }
  } catch (error) {
    return jsonError("origin_invalid", error?.message || "invalid origin URL", 502);
  }
  try {
    const response = await fetch(originURL, {
      headers: requestHeaders(context.request),
      eo: timeoutSettings(10_000, 60_000),
    });
    const responseHeaders = filteredHeaders(response.headers);
    responseHeaders.set("X-Yuzu-Distribution", "origin-fallback");
    reportEvent(context, ref, ticket, "fallback");
    return new Response(response.body, {
      status: response.status,
      statusText: response.statusText,
      headers: responseHeaders,
    });
  } catch (error) {
    return jsonError(
      "origin_unavailable",
      error?.message || "origin request failed",
      502,
    );
  }
}

function reportEvent(context, trackRef, ticket, kind, fields = {}) {
  const task = fetch(new URL("/yuzu-edge/event", context.request.url), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      track_ref: trackRef,
      ticket,
      kind,
      ...fields,
    }),
    eo: timeoutSettings(5_000, 5_000),
  }).catch(() => {});
  if (typeof context.waitUntil === "function") {
    context.waitUntil(task);
  }
}

function requestHeaders(request) {
  const headers = new Headers();
  for (const name of [
    "Range",
    "If-Range",
    "If-None-Match",
    "If-Modified-Since",
    "User-Agent",
  ]) {
    const value = request.headers.get(name);
    if (value !== null) {
      headers.set(name, value);
    }
  }
  return headers;
}

function filteredHeaders(source) {
  const headers = new Headers();
  for (const name of FORWARDED_RESPONSE_HEADERS) {
    const value = source.get(name);
    if (value !== null) {
      headers.set(name, value);
    }
  }
  return headers;
}

function copyResponse(response, distributionPath) {
  const headers = filteredHeaders(response.headers);
  headers.set("X-Yuzu-Distribution", distributionPath);
  if (!headers.has("Content-Type")) {
    headers.set("Content-Type", "application/json; charset=utf-8");
  }
  return new Response(response.body, {
    status: response.status,
    statusText: response.statusText,
    headers,
  });
}

function responseLength(headers) {
  const value = Number(headers.get("Content-Length") || 0);
  return Number.isSafeInteger(value) && value > 0 ? value : 0;
}

function trackRef(context, requestURL) {
  const raw = context.params?.ref || requestURL.pathname.split("/").at(-1) || "";
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

function timeoutSettings(connectTimeout, readTimeout) {
  return {
    timeoutSetting: {
      connectTimeout,
      readTimeout,
      writeTimeout: 10_000,
    },
  };
}

function jsonError(code, message, status) {
  return new Response(JSON.stringify({ error: { code, message } }), {
    status,
    headers: {
      "Cache-Control": "no-store",
      "Content-Type": "application/json; charset=utf-8",
    },
  });
}

export const __test = {
  filteredHeaders,
  responseLength,
  trackRef,
};
