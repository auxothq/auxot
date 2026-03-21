# How to Deploy the GPU Worker for Businesses

This guide is for business teams who want a practical, plain-language walkthrough of GPU worker deployment.

If you want a deeper technical reference, use the main deployment docs:
[https://auxot.com/docs/self-hosting/deployment](https://auxot.com/docs/self-hosting/deployment)

## Who This Guide Is For

- Business or operations owners who need to run AI on company-managed infrastructure
- IT teams supporting non-technical users
- Teams that need a "what to do next" guide without removing technical accuracy

## What the GPU Worker Is (Plain Language)

The GPU worker is the service that does the heavy AI processing on your GPU machine.

- Think of the router as traffic control (it receives requests)
- Think of the GPU worker as the engine (it runs the model)
- The worker connects to the router using a one-time-issued GPU key

In technical terms, the worker authenticates with `--gpu-key` and connects to the router WebSocket URL (`--router-url .../ws`).

## Before You Start

You need:

1. A running Auxot router
2. A machine with a supported GPU
3. Node.js on the GPU machine (for `@auxot/worker-cli`)
4. Network access from the GPU machine to your router's `/ws` endpoint

## Important Security Note (Read First)

The GPU key may be shown only once in onboarding.

- Store it in your approved secrets manager (for example: 1Password, Vault, AWS Secrets Manager)
- If it is lost, rotate/regenerate the key
- Rotating a key invalidates the old key

## Deployment Flow (Business-Friendly + Technical)

### Step 1) Create the GPU policy and key in onboarding

In the onboarding flow:

1. Choose **Connect a GPU Worker**
2. Create the policy
3. Copy the GPU key immediately and store it securely

### Step 2) Run the worker on your GPU machine

Use the command from onboarding, for example:

```bash
npx --yes @auxot/worker-cli --gpu-key <YOUR_GPU_KEY> --router-url wss://<YOUR_ROUTER_HOST>/ws
```

If your deployment is local/non-TLS, you may use `ws://.../ws` instead of `wss://.../ws`.

### Step 3) Confirm connection

After startup, the worker should authenticate and connect.

In technical terms, the system emits a `gpu.connected` event when the worker is online.

### Step 4) Validate end-to-end behavior

- Send a test request through Auxot
- Verify model responses return successfully
- Confirm latency and throughput are acceptable for your business use case

## When to Use This Guide vs Main Deployment Docs

Use **this guide** when:

- You need a clear, business-friendly checklist
- Your audience includes non-technical stakeholders
- You want a secure operational process around one-time keys

Use the **main deployment docs** when:

- You need full platform-specific deployment options
- You need advanced infrastructure details
- You need the latest canonical command and environment guidance

Main deployment docs:
[https://auxot.com/docs/self-hosting/deployment](https://auxot.com/docs/self-hosting/deployment)

## Troubleshooting Basics

- Worker does not connect: verify GPU key and router URL (must include `/ws`)
- TLS mismatch: use `wss://` for HTTPS deployments, `ws://` for local/non-TLS
- Lost key: regenerate in onboarding and update the worker command

## Operational Recommendations

- Keep GPU keys in managed secrets, not personal notes
- Define who can rotate keys and when
- Add a short runbook for support teams (start, stop, verify, rotate)
- Test key rotation in a non-production environment before production rollout

