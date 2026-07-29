import { getStore } from "@edgeone/pages-blob";
import {
  getSignerCompatible,
  signBlobGET,
} from "../../lib/blob-signing.js";

const LOCATOR_PATTERN = /^media\/[a-f0-9]{64}\/object$/;

export async function onRequest(context) {
  const action = actionFrom(context.request);
  if (action === "health") {
    const store = blobStore();
    return json({
      ok: true,
      core_configured: Boolean(env("EO_YUZU_CORE_ORIGIN")),
      edge_credential_configured: Boolean(env("EO_YUZU_EDGE_TOKEN")),
      get_signer_compatible: getSignerCompatible(store),
    });
  }
  if (context.request.method !== "POST") {
    return jsonError("method_not_allowed", "method not allowed", 405);
  }
  try {
    switch (action) {
      case "introspect":
        return await introspect(context.request);
      case "event":
        return await event(context.request);
      default:
        return jsonError("not_found", "unknown control action", 404);
    }
  } catch (cause) {
    return jsonError(
      cause?.code || "control_error",
      cause?.message || String(cause),
      cause?.status || 500,
    );
  }
}

async function introspect(request) {
  const body = await readJSON(request);
  const trackRef = validTrackRef(body?.track_ref);
  const ticket = validTicket(body?.ticket);
  const origin = coreOrigin();
  const response = await fetch(
    new URL("/internal/v1/distribution/introspect", origin),
    {
      method: "POST",
      headers: coreHeaders(),
      body: JSON.stringify({ track_ref: trackRef, ticket }),
    },
  );
  if (!response.ok) {
    return copyCoreError(response);
  }
  const value = await response.json();
  const result = {
    valid: true,
    enabled: value?.enabled !== false,
    acceleration_id: value?.acceleration_id || "",
    track_ref: trackRef,
    ready: false,
    candidate: null,
    signed_url: null,
    fallback_reason:
      value?.fallback_reason ||
      (value?.enabled === false
        ? "acceleration_disabled"
        : "candidate_not_ready"),
  };
  const candidate = value?.candidate;
  if (
    value?.ready &&
    candidate?.layout === "object" &&
    LOCATOR_PATTERN.test(candidate?.locator || "")
  ) {
    try {
      const signed = await signBlobGET(
        blobStore(),
        storeName(),
        candidate.locator,
        120,
      );
      result.ready = true;
      result.candidate = {
        content_version: candidate.content_version,
        locator: candidate.locator,
        layout: candidate.layout,
        size_bytes: candidate.size_bytes,
        content_type: candidate.content_type,
        etag: candidate.etag || "",
      };
      result.signed_url = signed.url;
      result.fallback_reason = "";
    } catch (cause) {
      console.warn("GET signer unavailable", cause?.message || cause);
      result.fallback_reason = "signer_unavailable";
    }
  }
  return json(result);
}

async function event(request) {
  const body = await readJSON(request);
  const trackRef = validTrackRef(body?.track_ref);
  const ticket = validTicket(body?.ticket);
  const response = await fetch(
    new URL("/internal/v1/distribution/events", coreOrigin()),
    {
      method: "POST",
      headers: coreHeaders(),
      body: JSON.stringify({
        track_ref: trackRef,
        ticket,
        kind: body?.kind,
        reason: body?.reason || "",
        duration_ms: boundedInt(body?.duration_ms, 0, 60 * 60 * 1000, 0),
        bytes: boundedInt(body?.bytes, 0, Number.MAX_SAFE_INTEGER, 0),
      }),
    },
  );
  if (!response.ok) {
    return copyCoreError(response);
  }
  return new Response(null, { status: 204 });
}

function coreOrigin() {
  const raw = env("EO_YUZU_CORE_ORIGIN");
  const token = env("EO_YUZU_EDGE_TOKEN");
  if (!raw || !token) {
    const cause = new Error("Cloud control bridge is not configured");
    cause.code = "control_not_configured";
    cause.status = 503;
    throw cause;
  }
  const origin = new URL(raw);
  if (!['http:', 'https:'].includes(origin.protocol)) {
    throw badRequest("invalid configured origin");
  }
  return origin;
}

function coreHeaders() {
  return {
    Authorization: `Bearer ${env("EO_YUZU_EDGE_TOKEN")}`,
    "Content-Type": "application/json",
  };
}

function validTrackRef(value) {
  if (
    typeof value !== "string" ||
    value.length === 0 ||
    value.length > 512 ||
    !value.includes(":") ||
    /[\r\n]/.test(value)
  ) {
    throw badRequest("invalid track_ref");
  }
  return value;
}

function validTicket(value) {
  if (
    typeof value !== "string" ||
    value.length === 0 ||
    value.length > 256 ||
    /[^a-zA-Z0-9_-]/.test(value)
  ) {
    throw badRequest("invalid ticket");
  }
  return value;
}

function badRequest(message) {
  const cause = new Error(message);
  cause.code = "bad_request";
  cause.status = 400;
  return cause;
}

async function readJSON(request) {
  const contentLength = Number(request.headers.get("Content-Length") || 0);
  if (contentLength > 32 * 1024) {
    throw badRequest("request body too large");
  }
  try {
    return await request.json();
  } catch {
    throw badRequest("invalid json");
  }
}

async function copyCoreError(response) {
  const body = await response.text();
  return new Response(body, {
    status: response.status,
    headers: {
      "Cache-Control": "no-store",
      "Content-Type":
        response.headers.get("Content-Type") ||
        "application/json; charset=utf-8",
    },
  });
}

function blobStore() {
  return getStore(storeName());
}

function storeName() {
  return env("EO_YUZU_BLOB_STORE") || "yuzu-media";
}

function actionFrom(request) {
  return new URL(request.url).pathname.split("/").filter(Boolean).at(-1) || "";
}

function boundedInt(value, min, max, fallback) {
  const parsed = Number(value);
  if (!Number.isSafeInteger(parsed)) {
    return fallback;
  }
  return Math.min(Math.max(parsed, min), max);
}

function env(name) {
  return typeof process !== "undefined" && process?.env?.[name]
    ? process.env[name]
    : "";
}

function json(value, status = 200) {
  return new Response(JSON.stringify(value), {
    status,
    headers: {
      "Cache-Control": "no-store",
      "Content-Type": "application/json; charset=utf-8",
    },
  });
}

function jsonError(code, message, status) {
  return json({ error: { code, message } }, status);
}
