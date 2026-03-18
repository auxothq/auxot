---
name: release-management
description: Manage releases of auxot open-source Go binaries, Docker images, and npm wrapper packages. Use when releasing new versions, tagging releases, publishing to npm, updating Homebrew, or troubleshooting the release pipeline.
---

# Release Management — auxot OSS

How to cut releases for Go binaries, Docker images, npm packages, and Homebrew.

## What Lives Here

This repo (`auxothq/auxot`) is the single source of truth for all open-source releases:

| Directory | What It Produces |
|-----------|-----------------|
| `cmd/auxot-router` | Router binary |
| `cmd/auxot-worker` | GPU worker binary |
| `cmd/auxot-tools` | Tools worker binary |
| `npm/worker-cli` | `@auxot/worker-cli` — thin wrapper that downloads the Go `auxot-worker` binary at runtime |
| `npm/model-registry` | `@auxot/model-registry` — curated GGUF model catalog |
| `npm/cli` | `@auxot/cli` — key generation + Fly.io deploy CLI |

## Release Triggers

| What | Tag Format | Workflow | Output |
|------|-----------|----------|--------|
| Go binaries + Docker | `v0.3.0` | `release.yml` | GitHub Release, Docker images, Homebrew tap |
| `@auxot/worker-cli` | `worker-cli-v1.0.5` | `publish-npm.yml` | npm publish |
| `@auxot/model-registry` | `model-registry-v1.1.5` | `publish-npm.yml` | npm publish |
| `@auxot/cli` | `cli-v0.1.9` | `publish-npm.yml` | npm publish |

---

## 1. Releasing Go Binaries + Docker (`v*` tag)

### What Happens

Pushing a `v*` tag triggers `.github/workflows/release.yml` which runs three parallel jobs:

| Job | Output |
|-----|--------|
| GoReleaser | Cross-compiled binaries to GitHub Release + Homebrew tap update |
| Docker | `ghcr.io/auxothq/auxot-router:{version}` + `:latest` |
| Docker Tools | `ghcr.io/auxothq/auxot-tools:{version}` + `:latest` |

**Binaries:**

| Archive | Contents | Platforms |
|---------|----------|-----------|
| `auxot_{ver}_{os}_{arch}.tar.gz` | `auxot-router` + `auxot-worker` | linux, darwin x amd64, arm64 |
| `auxot_tools_{ver}_{os}_{arch}.tar.gz` | `auxot-tools` | linux, darwin, windows x amd64, arm64 |

**Homebrew:** GoReleaser auto-updates `auxothq/homebrew-tap` via a GitHub App token.

### Steps

```bash
# 1. Sync the model registry
make sync-registry

# 2. Run all checks
make check

# 3. Tag and push
make tag-release V=0.3.0

# 4. Monitor
open https://github.com/auxothq/auxot/actions
```

### Pre-release Checklist

- [ ] `make check` passes (vet, lint, test-race, build)
- [ ] `make sync-registry` pulled latest registry
- [ ] If registry changed: publish `@auxot/model-registry` first (see below)
- [ ] No uncommitted changes on main

### Secrets

| Secret | Purpose |
|--------|---------|
| `GITHUB_TOKEN` | Auto-provided — GitHub Release + GHCR |
| `RELEASEBOT_APP_ID` | GitHub App for `auxothq/homebrew-tap` |
| `RELEASEBOT_PRIVATE_KEY` | GitHub App private key |

---

## 2. Publishing npm Packages (`*-v*` tags)

### How It Works

Pushing a `worker-cli-v*`, `model-registry-v*`, or `cli-v*` tag triggers `.github/workflows/publish-npm.yml`. It installs deps, builds, and publishes the corresponding package under `npm/`.

### Steps

```bash
# Example: publish @auxot/worker-cli v1.0.5
cd npm/worker-cli

# 1. Bump version
npm version 1.0.5 --no-git-tag-version

# 2. Commit
git add package.json
git commit -m "chore: bump worker-cli to 1.0.5"

# 3. Tag and push
git tag worker-cli-v1.0.5
git push origin main
git push origin worker-cli-v1.0.5
```

Same pattern for model-registry and cli — just change the directory and tag prefix.

### When to Publish Each Package

| Package | Publish When |
|---------|-------------|
| `@auxot/worker-cli` | Download logic, CLI args, or help text changes. NOT needed for new Go binary releases (it auto-fetches latest). |
| `@auxot/model-registry` | Model catalog changes (new models, quant updates, vision metadata). Publish BEFORE tagging `auxothq/auxot`. |
| `@auxot/cli` | Setup/deploy logic changes (key generation, Fly.io template, verify command). |

### How @auxot/worker-cli Works

The npm package is a thin Node.js script that:
1. Calls `GET /repos/auxothq/auxot/releases/latest` on GitHub API
2. Downloads `auxot_{version}_{os}_{arch}.tar.gz` from the release
3. Extracts `auxot-worker` binary to `~/.auxot/bin/auxot-worker-{version}`
4. Spawns the binary with all CLI args passed through

The binary is cached per-version. Tagging a new Go release automatically makes it available via `npx @auxot/worker-cli`.

### Secrets

| Secret | Purpose |
|--------|---------|
| `NPM_TOKEN` | Automation token from npmjs.com for `@auxot` org |

### npm Account

- Organization: `@auxot` on npmjs.com
- Maintainer: `keith301`

---

## 3. Recommended Release Order

When making a coordinated release:

1. **`@auxot/model-registry`** — if model catalog changed
2. **Go binaries** — `make tag-release V=x.y.z` (pulls fresh registry)
3. **`@auxot/worker-cli`** — only if the wrapper itself changed
4. **`@auxot/cli`** — only if setup/deploy logic changed

---

## 4. Version Embedding

GoReleaser sets ldflags: `-X main.version={{.Version}} -X main.commit={{.ShortCommit}}`

**Known issue**: The main packages in `cmd/` have hardcoded version strings. The ldflags only work if the files declare `var version string` and use it. Fix by adding:

```go
var version = "dev"
```

---

## 5. Package Source Layout

```
npm/
├── worker-cli/          # @auxot/worker-cli
│   ├── src/index.ts     # Binary downloader + launcher
│   ├── build.mjs        # esbuild bundler
│   └── package.json
├── model-registry/      # @auxot/model-registry
│   ├── src/             # Types, schemas, loader, query
│   ├── registry.json    # Model catalog (also at pkg/registry/registry.json)
│   └── package.json
└── cli/                 # @auxot/cli
    ├── src/index.ts     # Setup, deploy, verify commands
    ├── src/keygen.ts    # Argon2id key generation
    ├── build.mjs        # esbuild bundler
    └── package.json
```

---

## Troubleshooting

### GoReleaser fails on "sync-registry"

The `before.hooks` runs `make sync-registry`. If neither the monorepo nor npm is accessible, it fails. Ensure `@auxot/model-registry` is on npm.

### Homebrew tap update fails

Needs the GitHub App token. Check `RELEASEBOT_APP_ID` / `RELEASEBOT_PRIVATE_KEY` secrets.

### npm publish "version already published"

Bump the version. npm does not allow republishing the same version.

### `npx @auxot/worker-cli` downloads old binary

The wrapper fetches `releases/latest` from GitHub API. Prereleases and drafts are excluded from this endpoint. Ensure the release is published (not draft/prerelease).
