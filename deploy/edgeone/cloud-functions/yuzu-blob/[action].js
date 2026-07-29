import { getStore } from "@edgeone/pages-blob";
import {
  createPresignedGetURL,
  getSignerCompatible,
  signBlobGET,
} from "../../lib/blob-signing.js";

const MAX_BATCH = 32;
const DEFAULT_EXPIRE_SECONDS = 120;
const LOCATOR_PATTERN = /^media\/[a-f0-9]{64}\/object$/;

export async function onRequest(context) {
  const action = actionFrom(context.request);
  const signerToken = env("EO_YUZU_SIGNER_TOKEN");
  if (!signerToken) {
    return error("signer_not_configured", "signer credential is missing", 503);
  }
  if (!secureEqual(bearer(context.request), signerToken)) {
    return error("not_found", "not found", 404);
  }
  if (context.request.method !== "POST" && action !== "health") {
    return error("method_not_allowed", "method not allowed", 405);
  }

  try {
    switch (action) {
      case "health":
        return json({
          ok: true,
          store: storeName(),
          get_signer_compatible: getSignerCompatible(blobStore()),
        });
      case "put-urls":
        return await putURLs(context.request);
      case "get-urls":
        return await getURLs(context.request);
      case "metadata":
        return await metadata(context.request);
      case "delete":
        return await deleteObjects(context.request);
      default:
        return error("not_found", "unknown signer action", 404);
    }
  } catch (cause) {
    return error(
      cause?.code || "signer_error",
      cause?.message || String(cause),
      cause?.status || 500,
    );
  }
}

async function putURLs(request) {
  const body = await readJSON(request);
  const objects = validateBatch(body?.objects);
  const expireSeconds = boundedInt(
    body?.expire_seconds,
    30,
    900,
    300,
  );
  const store = blobStore();
  const signed = [];
  for (const object of objects) {
    const contentType = normalizeContentType(object.content_type);
    const result = await store.createUploadUrl(object.locator, {
      expireSeconds,
      contentType,
    });
    signed.push({
      locator: object.locator,
      url: result.url,
      expires_at: result.expiresAt,
    });
  }
  return json({ objects: signed });
}

async function getURLs(request) {
  const body = await readJSON(request);
  const locators = validateLocators(body?.locators);
  const expireSeconds = boundedInt(
    body?.expire_seconds,
    30,
    300,
    DEFAULT_EXPIRE_SECONDS,
  );
  const store = blobStore();
  if (!getSignerCompatible(store)) {
    return error(
      "get_signer_unavailable",
      "Blob SDK internals needed for GET signing are unavailable",
      501,
    );
  }
  const objects = [];
  for (const locator of locators) {
    const signed = await signBlobGET(
      store,
      storeName(),
      locator,
      expireSeconds,
    );
    objects.push({
      locator,
      url: signed.url,
      expires_at: signed.expiresAt,
    });
  }
  return json({ objects });
}

async function deleteObjects(request) {
  const body = await readJSON(request);
  const locators = validateLocators(body?.locators);
  const store = blobStore();
  for (const locator of locators) {
    await store.delete(locator);
  }
  return json({ deleted: locators });
}

async function metadata(request) {
  const body = await readJSON(request);
  const locators = validateLocators(body?.locators);
  const store = blobStore();
  const objects = [];
  for (const locator of locators) {
    const value = await store.getMetadata(locator, { consistency: "strong" });
    if (!value) {
      objects.push({ locator, exists: false });
      continue;
    }
    const headers = lowerCaseHeaders(value.headers || {});
    objects.push({
      locator,
      exists: true,
      size_bytes: Number(headers["content-length"] || value.size || 0),
      content_type:
        headers["content-type"] ||
        value.contentType ||
        "application/octet-stream",
      cache_control: headers["cache-control"] || value.cacheControl || "",
      etag: value.etag || headers.etag || "",
    });
  }
  return json({ objects });
}

function validateBatch(value) {
  if (!Array.isArray(value) || value.length === 0 || value.length > MAX_BATCH) {
    throw badRequest(`objects must contain 1-${MAX_BATCH} entries`);
  }
  return value.map((object) => {
    validateLocator(object?.locator);
    return object;
  });
}

function validateLocators(value) {
  if (!Array.isArray(value) || value.length === 0 || value.length > MAX_BATCH) {
    throw badRequest(`locators must contain 1-${MAX_BATCH} entries`);
  }
  for (const locator of value) {
    validateLocator(locator);
  }
  return value;
}

function validateLocator(locator) {
  if (typeof locator !== "string" || !LOCATOR_PATTERN.test(locator)) {
    throw badRequest("invalid Blob locator");
  }
}

function normalizeContentType(value) {
  if (
    typeof value !== "string" ||
    value.length === 0 ||
    value.length > 128 ||
    /[\r\n]/.test(value)
  ) {
    throw badRequest("invalid content_type");
  }
  return value;
}

function badRequest(message) {
  const cause = new Error(message);
  cause.code = "bad_request";
  cause.status = 400;
  return cause;
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

function bearer(request) {
  const header = request.headers.get("Authorization") || "";
  return header.startsWith("Bearer ") ? header.slice(7) : "";
}

function secureEqual(left, right) {
  if (left.length !== right.length) {
    return false;
  }
  let difference = 0;
  for (let index = 0; index < left.length; index += 1) {
    difference |= left.charCodeAt(index) ^ right.charCodeAt(index);
  }
  return difference === 0;
}

function env(name) {
  if (typeof process !== "undefined" && process?.env?.[name]) {
    return process.env[name];
  }
  return typeof globalThis[name] === "string" ? globalThis[name] : "";
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

function boundedInt(value, min, max, fallback) {
  const parsed = Number(value);
  if (!Number.isInteger(parsed)) {
    return fallback;
  }
  return Math.min(Math.max(parsed, min), max);
}

function lowerCaseHeaders(headers) {
  const result = {};
  for (const [name, value] of Object.entries(headers)) {
    result[String(name).toLowerCase()] = String(value);
  }
  return result;
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

function error(code, message, status) {
  return json({ error: { code, message } }, status);
}

export const __test = {
  secureEqual,
  validateLocator,
  createPresignedGetURL,
};
