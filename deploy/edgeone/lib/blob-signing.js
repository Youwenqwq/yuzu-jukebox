export function getSignerCompatible(store) {
  const client = store?.cosClient;
  return Boolean(
    client &&
      typeof client.resolveDomain === "function" &&
      typeof client.resolveCredential === "function" &&
      typeof client.buildCosKey === "function",
  );
}

export async function signBlobGET(store, storeName, locator, expireSeconds) {
  if (!getSignerCompatible(store)) {
    const cause = new Error(
      "Blob SDK internals needed for GET signing are unavailable",
    );
    cause.code = "get_signer_unavailable";
    cause.status = 501;
    throw cause;
  }
  const client = store.cosClient;
  const domain = await client.resolveDomain("eventual");
  const credential = await client.resolveCredential();
  const cosKey = client.buildCosKey(storeName, locator);
  return createPresignedGetURL({
    domain,
    key: cosKey,
    credential,
    expireSeconds,
  });
}

export async function createPresignedGetURL({
  domain,
  key,
  credential,
  expireSeconds,
}) {
  const now = Math.floor(Date.now() / 1000);
  const keyTime = `${now};${now + expireSeconds}`;
  const decodedKey = key
    .split("/")
    .map((part) => safeDecodeURIComponent(part))
    .join("/");
  const encodedKey = key
    .split("/")
    .map((part) => encodeURIComponent(safeDecodeURIComponent(part)))
    .join("/");
  const httpString = `get\n/${decodedKey}\n\n\n`;
  const stringToSign = `sha1\n${keyTime}\n${await sha1Hex(httpString)}\n`;
  const signKey = await hmacSHA1Hex(credential.secretKey, keyTime);
  const signature = await hmacSHA1Hex(signKey, stringToSign);
  const signedURL = new URL(domain);
  signedURL.pathname = `/${encodedKey}`;
  signedURL.searchParams.set("q-sign-algorithm", "sha1");
  signedURL.searchParams.set("q-ak", credential.secretId);
  signedURL.searchParams.set("q-sign-time", keyTime);
  signedURL.searchParams.set("q-key-time", keyTime);
  signedURL.searchParams.set("q-header-list", "");
  signedURL.searchParams.set("q-url-param-list", "");
  signedURL.searchParams.set("q-signature", signature);
  if (credential.sessionToken) {
    signedURL.searchParams.set(
      "x-cos-security-token",
      credential.sessionToken,
    );
  }
  return { url: signedURL.toString(), expiresAt: now + expireSeconds };
}

async function sha1Hex(value) {
  const data = new TextEncoder().encode(value);
  const digest = await crypto.subtle.digest("SHA-1", data);
  return bytesToHex(new Uint8Array(digest));
}

async function hmacSHA1Hex(secret, value) {
  const key = await crypto.subtle.importKey(
    "raw",
    new TextEncoder().encode(secret),
    { name: "HMAC", hash: "SHA-1" },
    false,
    ["sign"],
  );
  const signature = await crypto.subtle.sign(
    "HMAC",
    key,
    new TextEncoder().encode(value),
  );
  return bytesToHex(new Uint8Array(signature));
}

function bytesToHex(bytes) {
  let result = "";
  for (const byte of bytes) {
    result += byte.toString(16).padStart(2, "0");
  }
  return result;
}

function safeDecodeURIComponent(value) {
  try {
    return decodeURIComponent(value);
  } catch {
    return value;
  }
}
