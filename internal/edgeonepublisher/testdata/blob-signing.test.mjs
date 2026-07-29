import assert from "node:assert/strict";
import { createHash, createHmac } from "node:crypto";
import test from "node:test";

import {
  createPresignedGetURL,
  getSignerCompatible,
  signBlobGET,
} from "../../../deploy/edgeone/lib/blob-signing.js";

test("GET signer compatibility requires the isolated SDK methods", () => {
  assert.equal(getSignerCompatible(null), false);
  assert.equal(getSignerCompatible({ cosClient: {} }), false);
  assert.equal(
    getSignerCompatible({
      cosClient: {
        resolveDomain() {},
        resolveCredential() {},
        buildCosKey() {},
      },
    }),
    true,
  );
});

test("presigned GET uses the COS V5 canonical request", async () => {
  const originalNow = Date.now;
  Date.now = () => 1_700_000_000_000;
  try {
    const key =
      "project/yuzu-media/media/" + "ab".repeat(32) + "/track object";
    const credential = {
      secretId: "test-secret-id",
      secretKey: "test-secret-key",
      sessionToken: "test-session-token",
    };
    const signed = await createPresignedGetURL({
      domain: "https://blob.example.test",
      key,
      credential,
      expireSeconds: 120,
    });

    const keyTime = "1700000000;1700000120";
    const httpString = `get\n/${key}\n\n\n`;
    const stringToSign = `sha1\n${keyTime}\n${createHash("sha1").update(httpString).digest("hex")}\n`;
    const signKey = createHmac("sha1", credential.secretKey)
      .update(keyTime)
      .digest("hex");
    const expectedSignature = createHmac("sha1", signKey)
      .update(stringToSign)
      .digest("hex");

    const url = new URL(signed.url);
    assert.equal(signed.expiresAt, 1_700_000_120);
    assert.equal(
      url.pathname,
      "/project/yuzu-media/media/" + "ab".repeat(32) + "/track%20object",
    );
    assert.equal(url.searchParams.get("q-sign-algorithm"), "sha1");
    assert.equal(url.searchParams.get("q-ak"), credential.secretId);
    assert.equal(url.searchParams.get("q-sign-time"), keyTime);
    assert.equal(url.searchParams.get("q-key-time"), keyTime);
    assert.equal(url.searchParams.get("q-signature"), expectedSignature);
    assert.equal(
      url.searchParams.get("x-cos-security-token"),
      credential.sessionToken,
    );
  } finally {
    Date.now = originalNow;
  }
});

test("SDK adapter builds a signed URL and rejects incompatible clients", async () => {
  const locator = `media/${"cd".repeat(32)}/object`;
  const store = {
    cosClient: {
      async resolveDomain() {
        return "https://blob.example.test";
      },
      async resolveCredential() {
        return { secretId: "id", secretKey: "key" };
      },
      buildCosKey(storeName, value) {
        return `project/${storeName}/${value}`;
      },
    },
  };
  const signed = await signBlobGET(store, "yuzu-media", locator, 60);
  assert.equal(
    new URL(signed.url).pathname,
    `/project/yuzu-media/${locator}`,
  );

  await assert.rejects(
    () => signBlobGET({}, "yuzu-media", locator, 60),
    (error) => error.code === "get_signer_unavailable" && error.status === 501,
  );
});
