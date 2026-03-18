import {
  existsSync,
  mkdirSync,
  createWriteStream,
  chmodSync,
  renameSync,
  unlinkSync,
} from "node:fs";
import { join } from "node:path";
import { homedir, platform, arch } from "node:os";
import { execFileSync, spawn } from "node:child_process";
import { pipeline } from "node:stream/promises";
import { Readable } from "node:stream";
import pkg from "../package.json" with { type: "json" };

const GITHUB_REPO = "auxothq/auxot";
const BINARY_NAME = "auxot-worker";
const CACHE_DIR = join(homedir(), ".auxot", "bin");

function detectPlatform(): { os: string; arch: string } {
  const os = platform();
  const cpuArch = arch();
  const osMap: Record<string, string> = {
    linux: "linux",
    darwin: "darwin",
  };
  const archMap: Record<string, string> = {
    x64: "amd64",
    arm64: "arm64",
  };
  const mappedOS = osMap[os];
  const mappedArch = archMap[cpuArch];
  if (!mappedOS) {
    console.error(`Unsupported platform: ${os}`);
    console.error("auxot-worker supports Linux and macOS.");
    process.exit(1);
  }
  if (!mappedArch) {
    console.error(`Unsupported architecture: ${cpuArch}`);
    console.error("auxot-worker supports x64 (amd64) and arm64.");
    process.exit(1);
  }
  return { os: mappedOS, arch: mappedArch };
}

async function getLatestVersion(): Promise<string> {
  const url = `https://api.github.com/repos/${GITHUB_REPO}/releases/latest`;
  const resp = await fetch(url, {
    headers: { Accept: "application/vnd.github.v3+json" },
  });
  if (!resp.ok) {
    throw new Error(`GitHub API error: ${resp.status} ${resp.statusText}`);
  }
  const data = (await resp.json()) as { tag_name: string };
  return data.tag_name;
}

function binaryPath(version: string): string {
  return join(CACHE_DIR, `${BINARY_NAME}-${version}`);
}

async function downloadBinary(
  version: string,
  plat: { os: string; arch: string }
): Promise<string> {
  const dest = binaryPath(version);
  if (existsSync(dest)) {
    return dest;
  }

  mkdirSync(CACHE_DIR, { recursive: true });

  const versionNum = version.replace(/^v/, "");
  const archiveName = `auxot_${versionNum}_${plat.os}_${plat.arch}.tar.gz`;
  const url = `https://github.com/${GITHUB_REPO}/releases/download/${version}/${archiveName}`;

  console.log(
    `Downloading auxot-worker ${version} for ${plat.os}/${plat.arch}...`
  );

  const resp = await fetch(url);
  if (!resp.ok) {
    throw new Error(
      `Download failed: ${resp.status} ${resp.statusText}\nURL: ${url}`
    );
  }

  const tarball = dest + ".tar.gz";
  const body = resp.body;
  if (!body) throw new Error("Empty response body");

  const writeStream = createWriteStream(tarball);
  await pipeline(Readable.fromWeb(body as any), writeStream);

  try {
    execFileSync("tar", ["xzf", tarball, "-C", CACHE_DIR, BINARY_NAME], {
      stdio: "pipe",
    });
    const extractedPath = join(CACHE_DIR, BINARY_NAME);
    if (existsSync(extractedPath)) {
      renameSync(extractedPath, dest);
    }
  } finally {
    if (existsSync(tarball)) unlinkSync(tarball);
  }

  chmodSync(dest, 0o755);
  console.log(`Cached at ${dest}\n`);

  return dest;
}

async function main() {
  const args = process.argv.slice(2);

  if (
    args.includes("--version") ||
    args.includes("-v") ||
    args[0] === "version"
  ) {
    console.log(`@auxot/worker-cli v${pkg.version} (Go binary wrapper)`);
    process.exit(0);
  }

  if (args.includes("--help") || args.includes("-h") || args[0] === "help") {
    console.log(`@auxot/worker-cli — Run the auxot GPU worker

This package downloads and runs the auxot-worker Go binary.
All arguments are passed through to the binary.

Usage:
  npx @auxot/worker-cli --gpu-key gpu.xxx   # Connects to auxot.com by default
  npx @auxot/worker-cli --gpu-key adm_xxx --router-url wss://my-router.fly.dev/ws  # Self-hosted

Options:
  --gpu-key <key>             GPU key from auxot-router setup (required)
  --router-url <url>          Router WebSocket URL (default: wss://auxot.com/api/gpu/client)
  --model-path <path>         Local GGUF model file (air-gapped mode)
  --llama-server-path <path>  Local llama-server binary (air-gapped mode)
  --debug [level]             Debug logging (1 or 2)
  --help                      Show this help
  --version                   Show version

Environment Variables:
  AUXOT_GPU_KEY               GPU key (alternative to --gpu-key)
  AUXOT_ROUTER_URL            Router URL (alternative to --router-url)

More info: https://github.com/auxothq/auxot`);
    process.exit(0);
  }

  const plat = detectPlatform();

  let version: string;
  try {
    version = await getLatestVersion();
  } catch (err: any) {
    console.error("Failed to check for latest version:", err.message);
    console.error(
      "\nTip: Check https://github.com/auxothq/auxot/releases for available versions."
    );
    process.exit(1);
  }

  let binary: string;
  try {
    binary = await downloadBinary(version, plat);
  } catch (err: any) {
    console.error("Failed to download auxot-worker:", err.message);
    process.exit(1);
  }

  const child = spawn(binary, args, {
    stdio: "inherit",
    env: process.env,
  });

  const signals: NodeJS.Signals[] = ["SIGINT", "SIGTERM", "SIGHUP"];
  for (const sig of signals) {
    process.on(sig, () => child.kill(sig));
  }

  child.on("exit", (code, signal) => {
    if (signal) {
      process.kill(process.pid, signal);
    } else {
      process.exit(code ?? 1);
    }
  });
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
