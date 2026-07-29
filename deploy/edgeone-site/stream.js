// Set this non-secret URL to the managed acceleration's control_base_url
// before pasting the function into the EdgeOne site dashboard.
const CONTROL_ORIGIN = "https://makers.invalid/yuzu-edge";
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

addEventListener("fetch", (event) => {
  event.passThroughOnException();
  event.respondWith(handleStream(event));
});

async function handleStream(event) {
  const request = event.request;
  if (request.method !== "GET") {
    return jsonError("method_not_allowed", "method not allowed", 405);
  }
  const requestURL = new URL(request.url);
  const ref = trackRef(requestURL);
  const ticket = requestURL.searchParams.get("ticket") || "";
  let control;
  try {
    const response = await fetch(controlURL("introspect"), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ track_ref: ref, ticket }),
      eo: timeoutSettings(10_000, 20_000),
    });
    if (!response.ok) {
      return fallback(event, ref, ticket, "control_unavailable");
    }
    control = await response.json();
  } catch (error) {
    console.warn("Control bridge unavailable", error?.message || error);
    return fallback(event, ref, ticket, "control_unavailable");
  }

  if (control?.ready && control?.signed_url) {
    const blob = await tryBlob(event, control.signed_url, ref, ticket);
    if (blob.response) {
      return blob.response;
    }
    return fallback(event, ref, ticket, blob.reason);
  }
  return fallback(
    event,
    ref,
    ticket,
    validFallbackReason(control?.fallback_reason)
      ? control.fallback_reason
      : "candidate_not_ready",
  );
}

async function tryBlob(event, signedURL, ref, ticket) {
  const started = Date.now();
  try {
    const response = await fetch(signedURL, {
      headers: requestHeaders(event.request),
      eo: timeoutSettings(10_000, 60_000),
    });
    if (![200, 206, 304, 416].includes(response.status)) {
      return { response: null, reason: "blob_http_status" };
    }
    const headers = filteredHeaders(response.headers);
    headers.set("X-Yuzu-Distribution", "edgeone-blob");
    reportEvent(event, ref, ticket, "blob_served", "", {
      duration_ms: Date.now() - started,
      bytes: responseLength(response.headers),
    });
    return {
      response: new Response(response.body, {
        status: response.status,
        statusText: response.statusText,
        headers,
      }),
      reason: "",
    };
  } catch (error) {
    console.warn("Blob proxy failed", error?.message || error);
    return { response: null, reason: "blob_fetch_error" };
  }
}

async function fallback(event, ref, ticket, reason) {
  try {
    const response = await fetch(event.request, {
      eo: timeoutSettings(10_000, 60_000),
    });
    const headers = filteredHeaders(response.headers);
    headers.set("X-Yuzu-Distribution", "origin-fallback");
    reportEvent(event, ref, ticket, "fallback", reason);
    return new Response(response.body, {
      status: response.status,
      statusText: response.statusText,
      headers,
    });
  } catch (error) {
    return jsonError(
      "origin_unavailable",
      error?.message || "origin request failed",
      502,
    );
  }
}

function reportEvent(event, trackRefValue, ticket, kind, reason, fields = {}) {
  const task = fetch(controlURL("event"), {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      track_ref: trackRefValue,
      ticket,
      kind,
      reason,
      ...fields,
    }),
    eo: timeoutSettings(5_000, 5_000),
  }).catch(() => {});
  event.waitUntil(task);
}

function controlURL(action) {
  return new URL(action, `${CONTROL_ORIGIN.replace(/\/+$/, "")}/`).toString();
}

function requestHeaders(request) {
  const headers = new Headers();
  for (const name of ["Range", "If-Range", "If-None-Match", "If-Modified-Since"]) {
    const value = request.headers.get(name);
    if (value) {
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

function responseLength(headers) {
  const value = Number(headers.get("Content-Length") || 0);
  return Number.isSafeInteger(value) && value > 0 ? value : 0;
}

function trackRef(requestURL) {
  const marker = "/stream/v1/";
  const index = requestURL.pathname.indexOf(marker);
  if (index < 0) {
    return "";
  }
  try {
    return decodeURIComponent(requestURL.pathname.slice(index + marker.length));
  } catch {
    return "";
  }
}

function timeoutSettings(connectTimeout, readTimeout) {
  return {
    timeoutSetting: {
      connectTimeout,
      readTimeout,
      writeTimeout: connectTimeout,
    },
  };
}

function validFallbackReason(value) {
  return [
    "acceleration_disabled",
    "candidate_not_ready",
    "signer_unavailable",
  ].includes(value);
}

function jsonError(code, message, status) {
  return new Response(JSON.stringify({ error: { code, message } }), {
    status,
    headers: { "Content-Type": "application/json; charset=utf-8" },
  });
}
