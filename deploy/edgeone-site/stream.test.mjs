import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = (
  await readFile(new URL("./stream.js", import.meta.url), "utf8")
).replace("https://makers.invalid/yuzu-edge", "https://makers.test/yuzu-edge");

async function invoke(fetchImpl, path = "/stream/v1/local%3Asong?ticket=ticket") {
  let listener;
  const waits = [];
  const context = vm.createContext({
    URL,
    Request,
    Response,
    Headers,
    console,
    fetch: fetchImpl,
    addEventListener(type, callback) {
      assert.equal(type, "fetch");
      listener = callback;
    },
  });
  vm.runInContext(source, context);
  let responsePromise;
  const request = new Request(`https://jukebox.test${path}`, {
    headers: { Range: "bytes=2-5" },
  });
  listener({
    request,
    passThroughOnException() {},
    respondWith(value) {
      responsePromise = Promise.resolve(value);
    },
    waitUntil(value) {
      waits.push(Promise.resolve(value));
    },
  });
  const response = await responsePromise;
  await Promise.all(waits);
  return response;
}

test("ready candidate streams Blob Range", async () => {
  const calls = [];
  const response = await invoke(async (input, init = {}) => {
    const url = typeof input === "string" ? input : input.url;
    calls.push({ url, init });
    if (url.endsWith("/introspect")) {
      return Response.json({
        ready: true,
        signed_url: "https://blob.test/object?signature=short-lived",
      });
    }
    if (url.startsWith("https://blob.test/")) {
      assert.equal(init.headers.get("Range"), "bytes=2-5");
      return new Response("2345", {
        status: 206,
        headers: {
          "Content-Type": "audio/mpeg",
          "Content-Range": "bytes 2-5/16",
          "Content-Length": "4",
        },
      });
    }
    if (url.endsWith("/event")) {
      return new Response(null, { status: 204 });
    }
    throw new Error(`unexpected fetch ${url}`);
  });
  assert.equal(response.status, 206);
  assert.equal(await response.text(), "2345");
  assert.equal(response.headers.get("X-Yuzu-Distribution"), "edgeone-blob");
  assert.equal(calls.some((call) => call.url === "https://jukebox.test/stream/v1/local%3Asong?ticket=ticket"), false);
});

test("not-ready candidate falls back through the site origin", async () => {
  let fallbackRequest;
  let fallbackEvent;
  const response = await invoke(async (input, init = {}) => {
    const url = typeof input === "string" ? input : input.url;
    if (url.endsWith("/introspect")) {
      return Response.json({ ready: false, fallback_reason: "candidate_not_ready" });
    }
    if (input instanceof Request && input.url.startsWith("https://jukebox.test/")) {
      fallbackRequest = input;
      return new Response("origin", { status: 206, headers: { "Content-Type": "audio/mpeg" } });
    }
    if (url.endsWith("/event")) {
      fallbackEvent = JSON.parse(init.body);
      return new Response(null, { status: 204 });
    }
    throw new Error(`unexpected fetch ${url}`);
  });
  assert.ok(fallbackRequest);
  assert.equal(fallbackRequest.headers.get("Range"), "bytes=2-5");
  assert.equal(await response.text(), "origin");
  assert.equal(response.headers.get("X-Yuzu-Distribution"), "origin-fallback");
  assert.equal(fallbackEvent.kind, "fallback");
  assert.equal(fallbackEvent.reason, "candidate_not_ready");
});

test("control outage falls back instead of returning 502", async () => {
  let fallbackEvent;
  const response = await invoke(async (input, init = {}) => {
    const url = typeof input === "string" ? input : input.url;
    if (url.endsWith("/introspect")) {
      throw new Error("control offline");
    }
    if (input instanceof Request && input.url.startsWith("https://jukebox.test/")) {
      return new Response("origin", { status: 200 });
    }
    if (url.endsWith("/event")) {
      fallbackEvent = JSON.parse(init.body);
      return new Response(null, { status: 204 });
    }
    throw new Error(`unexpected fetch ${url}`);
  });
  assert.equal(response.status, 200);
  assert.equal(await response.text(), "origin");
  assert.equal(response.headers.get("X-Yuzu-Distribution"), "origin-fallback");
  assert.equal(fallbackEvent.kind, "fallback");
  assert.equal(fallbackEvent.reason, "control_unavailable");
});
