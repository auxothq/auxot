import { execFileSync, spawnSync } from "node:child_process";
import { writeFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import { createInterface } from "node:readline";
import { tmpdir } from "node:os";
import { randomBytes } from "node:crypto";
import { generateAdminKey, generateAPIKey } from "./keygen.js";
import pkg from "../package.json" with { type: "json" };

const DEFAULT_MODEL = "Qwen3-Coder-30B-A3B";
const DOCKER_IMAGE = "ghcr.io/auxothq/auxot-router";
const GITHUB_REPO = "https://github.com/auxothq/auxot";

const args = process.argv.slice(2);
const command = args[0];

function getFlag(name: string): boolean {
  return args.includes(`--${name}`);
}

function getOption(name: string): string | undefined {
  const idx = args.indexOf(`--${name}`);
  if (idx >= 0 && idx + 1 < args.length) return args[idx + 1];
  return undefined;
}

async function main() {
  switch (command) {
    case "setup":
      await runSetup();
      break;
    case "deploy":
      await runDeploy();
      break;
    case "verify":
      await runVerify();
      break;
    case "help":
    case "--help":
    case "-h":
      printHelp();
      break;
    case "version":
    case "--version":
    case "-v":
      console.log(`auxot-cli v${pkg.version}`);
      break;
    default:
      console.error(`Unknown command: ${command}`);
      console.error('Run "npx @auxot/cli help" for usage.');
      process.exit(1);
  }
}

function banner(text: string) {
  const lines = text.split("\n");
  const maxLen = Math.max(...lines.map((l) => l.length));
  const width = Math.max(maxLen + 4, 64);
  const border = "#".repeat(width);
  console.log("");
  console.log(border);
  for (const line of lines) {
    const padded = line.padEnd(width - 4);
    console.log(`# ${padded} #`);
  }
  console.log(border);
  console.log("");
}

function prompt(question: string, defaultValue: string): Promise<string> {
  const rl = createInterface({ input: process.stdin, output: process.stdout });
  return new Promise((resolve) => {
    rl.question(`${question} [${defaultValue}]: `, (answer) => {
      rl.close();
      resolve(answer.trim() || defaultValue);
    });
  });
}

async function runSetup() {
  const fly = getFlag("fly");
  const model = getOption("model") || DEFAULT_MODEL;
  const appName = getOption("app") || `auxot-${randomBytes(3).toString("hex")}`;

  console.log("Generating keys...");
  const adminKey = await generateAdminKey();
  const apiKey = await generateAPIKey();

  if (fly) {
    banner(
      "YOUR SECRET KEYS \u2014 SAVE THESE NOW\nThey will NOT be shown again."
    );
    console.log(`  GPU Key (give to GPU workers):`);
    console.log(`    ${adminKey.key}`);
    console.log("");
    console.log(`  API Key (give to API callers):`);
    console.log(`    ${apiKey.key}`);

    banner(
      `FLY.IO SETUP \u2014 Run all of these\nSee ${GITHUB_REPO}#deploy-to-flyio for other methods`
    );
    console.log(`export APP_NAME="${appName}"`);
    console.log("");
    console.log(`fly apps create $APP_NAME`);
    console.log("");
    console.log(`cat > fly.toml << 'EOF'`);
    console.log(`app = "$APP_NAME"`);
    console.log(`primary_region = 'iad'`);
    console.log("");
    console.log(`[http_service]`);
    console.log(`  internal_port = 8080`);
    console.log(`  force_https = true`);
    console.log(`  auto_stop_machines = 'off'`);
    console.log(`  auto_start_machines = true`);
    console.log(`  min_machines_running = 1`);
    console.log("");
    console.log(`  [[http_service.checks]]`);
    console.log(`    grace_period = '10s'`);
    console.log(`    interval = '30s'`);
    console.log(`    method = 'GET'`);
    console.log(`    timeout = '5s'`);
    console.log(`    path = '/health'`);
    console.log("");
    console.log(`[vm]`);
    console.log(`  cpu_kind = 'shared'`);
    console.log(`  cpus = 1`);
    console.log(`  memory_mb = 256`);
    console.log(`EOF`);
    console.log("");
    console.log(`fly secrets set \\`);
    console.log(`  AUXOT_ADMIN_KEY_HASH='${adminKey.hash}' \\`);
    console.log(`  AUXOT_API_KEY_HASH='${apiKey.hash}' \\`);
    console.log(`  AUXOT_MODEL='${model}' \\`);
    console.log(`  -a $APP_NAME`);
    console.log("");
    console.log(
      `fly deploy --image ${DOCKER_IMAGE}:latest --ha=false -a $APP_NAME`
    );

    banner(
      `CONNECT YOUR GPU \u2014 Run this on the machine with your GPU\nThis is a long-running process. Keep it running.\nSee ${GITHUB_REPO}#run-locally for binary download`
    );
    console.log(
      `npx --yes @auxot/worker-cli --gpu-key ${adminKey.key} --router-url wss://$APP_NAME.fly.dev/ws`
    );

    banner("CONFIGURE YOUR TOOLS");
    console.log(
      "  Point any OpenAI or Anthropic-compatible tool at your router:"
    );
    console.log("");
    console.log(`  OpenAI base URL:     https://$APP_NAME.fly.dev/api/openai`);
    console.log(
      `  Anthropic base URL:  https://$APP_NAME.fly.dev/api/anthropic`
    );
    console.log(`  API Key:             ${apiKey.key}`);
    console.log("");
    console.log(
      "  Works with: Claude Code, Cursor, Open WebUI, LangChain, etc."
    );

    banner("VERIFY YOUR SETUP");
    console.log(`npx --yes @auxot/cli verify \\`);
    console.log(`  --router-url https://$APP_NAME.fly.dev \\`);
    console.log(`  --api-key ${apiKey.key}`);
    console.log("");
  } else {
    banner(
      "YOUR SECRET KEYS \u2014 SAVE THESE NOW\nThey will NOT be shown again."
    );
    console.log(`  GPU Key (give to GPU workers):`);
    console.log(`    ${adminKey.key}`);
    console.log("");
    console.log(`  API Key (give to API callers):`);
    console.log(`    ${apiKey.key}`);

    banner("ENVIRONMENT VARIABLES");
    console.log(`AUXOT_ADMIN_KEY_HASH='${adminKey.hash}'`);
    console.log(`AUXOT_API_KEY_HASH='${apiKey.hash}'`);
    console.log(`AUXOT_MODEL=${model}`);
    console.log(
      `# AUXOT_REDIS_URL=redis://localhost:6379  # Optional: uses embedded Redis if not set`
    );

    banner("VERIFY YOUR SETUP");
    console.log(`npx --yes @auxot/cli verify \\`);
    console.log(`  --router-url http://localhost:8080 \\`);
    console.log(`  --api-key ${apiKey.key}`);
    console.log("");
  }
}

async function runVerify() {
  const routerUrl =
    getOption("router-url") || getOption("router") || "http://localhost:8080";
  const apiKey = getOption("api-key") || getOption("key");
  const promptText =
    getOption("prompt") || "Explain Redis Streams in 6 bullets";
  const baseUrl = routerUrl.replace(/\/+$/, "");

  banner("HEALTH CHECK");
  try {
    const healthRes = await fetch(`${baseUrl}/health`);
    const health = (await healthRes.json()) as Record<string, any>;
    if (health.status === "ok") {
      console.log(`  \u2705 Router is healthy`);
      console.log(`  Workers:     ${health.workers}`);
      console.log(`  Total slots: ${health.total_slots}`);
      console.log(`  Active jobs: ${health.active_jobs}`);
      console.log(`  Available:   ${health.available}`);
    } else {
      console.log(`  \u26A0\uFE0F  Router returned: ${JSON.stringify(health)}`);
    }
    if (Number(health.workers) === 0) {
      console.log("");
      console.log("  \u26A0\uFE0F  No GPU workers connected. Connect one first:");
      console.log(
        `  npx --yes @auxot/worker-cli --gpu-key <GPU_KEY> --router-url ${baseUrl.replace("http", "ws")}/ws`
      );
    }
  } catch (err: any) {
    console.log(`  \u274C Cannot reach router at ${baseUrl}/health`);
    console.log(`     ${err.message || err}`);
    process.exit(1);
  }

  if (!apiKey) {
    console.log("");
    console.log(
      "  Skipping model list and inference test (no --api-key provided)"
    );
    return;
  }

  banner("MODELS");
  try {
    const modelsRes = await fetch(`${baseUrl}/api/openai/models`, {
      headers: { Authorization: `Bearer ${apiKey}` },
    });
    const models = (await modelsRes.json()) as {
      data?: { id: string; owned_by: string }[];
    };
    if (models.data && models.data.length > 0) {
      for (const m of models.data) {
        console.log(`  \u2705 ${m.id} (${m.owned_by})`);
      }
    } else {
      console.log(`  \u26A0\uFE0F  No models returned`);
    }
  } catch (err: any) {
    console.log(`  \u274C Failed to list models: ${err.message || err}`);
  }

  banner("INFERENCE TEST");
  console.log(`  Prompt: "${promptText}"`);
  console.log(`  Streaming...`);
  console.log("");

  const wallStart = performance.now();
  let firstTokenTime: number | null = null;
  let fullContent = "";
  let cacheTokens = 0;
  let promptTokens = 0;
  let completionTokens = 0;

  try {
    const res = await fetch(`${baseUrl}/api/openai/chat/completions`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${apiKey}`,
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        model: "any",
        messages: [{ role: "user", content: promptText }],
        stream: true,
      }),
    });
    if (!res.ok) {
      const body = await res.text();
      console.log(`  \u274C HTTP ${res.status}: ${body}`);
      process.exit(1);
    }
    const reader = res.body?.getReader();
    if (!reader) {
      console.log("  \u274C No response body");
      process.exit(1);
    }
    const decoder = new TextDecoder();
    let buffer = "";
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split("\n");
      buffer = lines.pop() || "";
      for (const line of lines) {
        if (!line.startsWith("data: ")) continue;
        const data = line.slice(6).trim();
        if (data === "[DONE]") continue;
        try {
          const chunk = JSON.parse(data) as any;
          if (chunk.choices?.[0]?.delta?.content) {
            if (firstTokenTime === null) firstTokenTime = performance.now();
            const token = chunk.choices[0].delta.content;
            fullContent += token;
            process.stdout.write(token);
          }
          if (chunk.usage) {
            cacheTokens = chunk.usage.cache_tokens || 0;
            promptTokens = chunk.usage.prompt_tokens || 0;
            completionTokens = chunk.usage.completion_tokens || 0;
          }
        } catch {}
      }
    }
  } catch (err: any) {
    console.log(`\n  \u274C Inference failed: ${err.message || err}`);
    process.exit(1);
  }

  const wallEnd = performance.now();
  const wallMs = wallEnd - wallStart;
  const ttftMs = firstTokenTime !== null ? firstTokenTime - wallStart : 0;
  const genTimeMs =
    firstTokenTime !== null ? wallEnd - firstTokenTime : wallMs;
  const tokensPerSec =
    completionTokens > 0
      ? (completionTokens / (genTimeMs / 1e3)).toFixed(1)
      : "n/a";

  banner("RESULTS");
  if (cacheTokens > 0) {
    console.log(`  Cache tokens:      ${cacheTokens} (KV cache hit)`);
  }
  console.log(`  Prompt tokens:     ${promptTokens || "n/a"}`);
  console.log(`  Output tokens:     ${completionTokens || "n/a"}`);
  console.log(`  Time to first token: ${ttftMs.toFixed(0)}ms`);
  console.log(`  Generation time:   ${(genTimeMs / 1e3).toFixed(2)}s`);
  console.log(`  Tokens/sec:        ${tokensPerSec}`);
  console.log(`  Wall time:         ${(wallMs / 1e3).toFixed(2)}s`);
  console.log("");
  console.log("  \u2705 Everything looks good!");
  console.log("");
}

async function runDeploy() {
  try {
    execFileSync("fly", ["version"], { stdio: "pipe" });
  } catch {
    console.error("Error: Fly CLI not found.\n");
    console.error("Install it:");
    console.error("  curl -L https://fly.io/install.sh | sh\n");
    console.error("Then authenticate:");
    console.error("  fly auth login\n");
    process.exit(1);
  }

  const appName =
    getOption("app") || (await prompt("App name", "auxot-router"));
  const region = getOption("region") || (await prompt("Region", "iad"));
  const model =
    getOption("model") || (await prompt("Model", DEFAULT_MODEL));

  console.log("\nGenerating keys...");
  const adminKey = await generateAdminKey();
  const apiKey = await generateAPIKey();

  const workDir = join(tmpdir(), `auxot-deploy-${Date.now()}`);
  mkdirSync(workDir, { recursive: true });

  const flyToml =
    `
app = '${appName}'
primary_region = '${region}'

[http_service]
  internal_port = 8080
  force_https = true
  auto_stop_machines = 'off'
  auto_start_machines = true
  min_machines_running = 1

  [[http_service.checks]]
    grace_period = '10s'
    interval = '30s'
    method = 'GET'
    timeout = '5s'
    path = '/health'

[vm]
  cpu_kind = 'shared'
  cpus = 1
  memory_mb = 256
`.trim() + "\n";
  writeFileSync(join(workDir, "fly.toml"), flyToml);

  console.log(`\nCreating Fly app "${appName}" in ${region}...`);
  const createResult = spawnSync(
    "fly",
    ["apps", "create", appName, "--machines"],
    { cwd: workDir, stdio: "inherit" }
  );
  if (createResult.status !== 0) {
    console.log("  (app may already exist, continuing...)\n");
  }

  console.log("\nSetting secrets...");
  const secretsResult = spawnSync(
    "fly",
    [
      "secrets",
      "set",
      `AUXOT_ADMIN_KEY_HASH=${adminKey.hash}`,
      `AUXOT_API_KEY_HASH=${apiKey.hash}`,
      `AUXOT_MODEL=${model}`,
      "-a",
      appName,
    ],
    { cwd: workDir, stdio: "inherit" }
  );
  if (secretsResult.status !== 0) {
    console.error("Failed to set secrets. You can set them manually:");
    console.error(
      `  fly secrets set AUXOT_ADMIN_KEY_HASH='${adminKey.hash}' AUXOT_API_KEY_HASH='${apiKey.hash}' AUXOT_MODEL='${model}' -a ${appName}`
    );
    process.exit(1);
  }

  console.log("\nDeploying...");
  const deployResult = spawnSync(
    "fly",
    ["deploy", "--image", `${DOCKER_IMAGE}:latest`, "--ha=false", "-a", appName],
    { cwd: workDir, stdio: "inherit" }
  );
  if (deployResult.status !== 0) {
    console.error("\nDeploy failed. You can retry manually:");
    console.error(
      `  fly deploy --image ${DOCKER_IMAGE}:latest --ha=false -a ${appName}`
    );
    process.exit(1);
  }

  banner("DEPLOYED SUCCESSFULLY");
  console.log(`  Router URL:  https://${appName}.fly.dev`);
  console.log(`  WebSocket:   wss://${appName}.fly.dev/ws`);

  banner(
    "YOUR SECRET KEYS \u2014 SAVE THESE NOW\nThey will NOT be shown again."
  );
  console.log(`  GPU Key:     ${adminKey.key}`);
  console.log(`  API Key:     ${apiKey.key}`);

  banner("CONNECT YOUR GPU");
  console.log(
    `npx --yes @auxot/worker-cli --gpu-key ${adminKey.key} --router-url wss://${appName}.fly.dev/ws`
  );

  banner("CONFIGURE YOUR TOOLS");
  console.log(
    `  OpenAI base URL:     https://${appName}.fly.dev/api/openai`
  );
  console.log(
    `  Anthropic base URL:  https://${appName}.fly.dev/api/anthropic`
  );
  console.log(`  API Key:             ${apiKey.key}`);

  banner("VERIFY YOUR SETUP");
  console.log(`npx --yes @auxot/cli verify \\`);
  console.log(`  --router-url https://${appName}.fly.dev \\`);
  console.log(`  --api-key ${apiKey.key}`);
  console.log("");
}

function printHelp() {
  console.log(`auxot \u2014 open-source GPU inference router

Usage:
  npx --yes @auxot/cli setup                 Generate keys and print env vars
  npx --yes @auxot/cli setup --fly           Print complete Fly.io deploy steps
  npx --yes @auxot/cli setup --fly --app X   Use custom app name (default: auxot-<random>)
  npx --yes @auxot/cli setup --model NAME    Use a specific model (default: ${DEFAULT_MODEL})
  npx --yes @auxot/cli deploy                Deploy auxot-router to Fly.io (interactive)
  npx --yes @auxot/cli verify                Health check + test inference
  npx --yes @auxot/cli verify --router-url URL --api-key KEY
  npx --yes @auxot/cli verify --prompt "..."  Custom test prompt
  npx --yes @auxot/cli help                  Print this help
  npx --yes @auxot/cli version               Print version

Examples:

  # Quick local setup
  npx --yes @auxot/cli setup

  # Print step-by-step Fly.io deploy commands (copy-paste friendly)
  npx --yes @auxot/cli setup --fly

  # Deploy to Fly.io (fully interactive)
  npx --yes @auxot/cli deploy

  # Verify everything works
  npx --yes @auxot/cli verify --router-url https://my-router.fly.dev --api-key rtr_xxx

More info: ${GITHUB_REPO}`);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
