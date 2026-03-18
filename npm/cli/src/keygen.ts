import { argon2id } from "hash-wasm";
import { randomBytes } from "node:crypto";

const ARGON2_MEMORY = 65536;
const ARGON2_ITERATIONS = 3;
const ARGON2_PARALLELISM = 4;
const ARGON2_HASH_LENGTH = 32;
const ARGON2_SALT_LENGTH = 16;

export async function generateAdminKey() {
  return generateKey("adm_");
}

export async function generateAPIKey() {
  return generateKey("rtr_");
}

async function generateKey(prefix: string) {
  const secret = randomBytes(32);
  const key = prefix + toBase64Url(secret);
  const hash = await hashKey(key);
  return { key, hash };
}

async function hashKey(key: string): Promise<string> {
  const salt = randomBytes(ARGON2_SALT_LENGTH);
  const hashHex = await argon2id({
    password: key,
    salt,
    parallelism: ARGON2_PARALLELISM,
    iterations: ARGON2_ITERATIONS,
    memorySize: ARGON2_MEMORY,
    hashLength: ARGON2_HASH_LENGTH,
    outputType: "hex",
  });
  const hashBytes = Buffer.from(hashHex, "hex");
  const saltB64 = toRawBase64(salt);
  const hashB64 = toRawBase64(hashBytes);
  return `$argon2id$v=19$m=${ARGON2_MEMORY},t=${ARGON2_ITERATIONS},p=${ARGON2_PARALLELISM}$${saltB64}$${hashB64}`;
}

function toRawBase64(buf: Buffer | Uint8Array): string {
  return Buffer.from(buf).toString("base64").replace(/=+$/, "");
}

function toBase64Url(buf: Buffer | Uint8Array): string {
  return Buffer.from(buf)
    .toString("base64")
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
}
