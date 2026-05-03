#!/usr/bin/env node
//
// patch-playwright-utils.js
//
// Patches waitForCompletion in playwright-core/lib/tools/backend/utils.js to
// correctly wait for navigation to commit before checking load state.
//
// Root cause of the stale-snapshot bug:
//
// 1. waitForTimeout(500) calls page.evaluate(() => setTimeout(f, 1e3)) in the
//    browser context.  When the click triggers navigation the old page context
//    is destroyed, page.evaluate rejects immediately via .catch(()=>{}), so the
//    500 ms pause becomes ~0 ms.  disposeListeners() may then run before the CDP
//    "request" event fires, causing requestedNavigation to be false even when a
//    navigation did start.
//
// 2. Even when requestedNavigation IS true, waitForLoadState("load") returns
//    immediately because the frame's _firedLifecycleEvents still contains the
//    OLD "load" entry — the navigation has been *requested* (HTTP request sent)
//    but hasn't *committed* yet (new document hasn't started loading).
//    waitForLoadState only blocks if "load" is NOT already in the set, so it
//    returns at once and the snapshot is taken against the still-alive old page.
//
// Fix:
//   1. Add setImmediate() after waitForTimeout and before disposeListeners() so
//      any already-queued CDP events get dispatched before the listener is torn
//      down.
//   2. When requestedNavigation is true, poll tab.page.url() every 100 ms (up to
//      3 s) until the URL changes — that signals navigation commit (Playwright
//      updates page.url() when the new document starts loading).  Only THEN call
//      waitForLoadState("load"), which now properly blocks until the new page
//      finishes loading because the lifecycle was cleared during commit.
//
// This script is run once during the Docker image build (see Dockerfile.tools,
// Stage 3: node-source) after `npm install -g @playwright/mcp@latest`.

'use strict';

const fs = require('fs');
const path = require('path');

// Resolve playwright-core's utils.js. npm global installs may hoist
// playwright-core to the top-level node_modules (npm >=9) or nest it under
// @playwright/mcp/node_modules (npm <=8). Try both.
//
// NOTE: only @playwright/mcp <=0.0.70 ships a separate utils.js; versions
// 0.0.71+ bundle everything into playwright-core/lib/coreBundle.js.
// Dockerfile.tools pins to 0.0.70 to keep this patch viable.
function resolveUtilsPath() {
  const candidates = [
    // hoisted (npm >=9 default)
    '/usr/local/lib/node_modules/playwright-core/lib/tools/backend/utils.js',
    // nested (npm <=8)
    '/usr/local/lib/node_modules/@playwright/mcp/node_modules/playwright-core/lib/tools/backend/utils.js',
  ];
  for (const p of candidates) {
    if (fs.existsSync(p)) return p;
  }
  throw new Error(
    '[patch-playwright-utils] Cannot locate playwright-core/lib/tools/backend/utils.js.\n' +
    'Searched:\n  ' + candidates.join('\n  ') + '\n' +
    'Ensure @playwright/mcp is pinned to <=0.0.70 in Dockerfile.tools.'
  );
}

const UTILS_PATH = resolveUtilsPath();
console.log('[patch-playwright-utils] Resolved utils path:', UTILS_PATH);

let src = fs.readFileSync(UTILS_PATH, 'utf8');

// ── Patch 1: setImmediate before disposeListeners ──────────────────────────
// Original:
//   } finally {
//     disposeListeners();
//   }
//
// Patched:
//   } finally {
//     await new Promise((f) => setImmediate(f));
//     disposeListeners();
//   }
const PATCH1_OLD = `  } finally {
    disposeListeners();
  }`;
const PATCH1_NEW = `  } finally {
    await new Promise((f) => setImmediate(f));
    disposeListeners();
  }`;

if (!src.includes(PATCH1_OLD)) {
  // Already patched or file changed — skip rather than double-apply.
  if (src.includes('setImmediate')) {
    console.log('[patch-playwright-utils] Patch 1 already applied, skipping.');
  } else {
    throw new Error(
      `[patch-playwright-utils] PATCH 1 FAILED: target string not found in ${UTILS_PATH}\n` +
      'The playwright-core version may have changed. Update the patch.'
    );
  }
} else {
  src = src.replace(PATCH1_OLD, PATCH1_NEW);
  console.log('[patch-playwright-utils] Patch 1 applied: setImmediate before disposeListeners.');
}

// ── Patch 2: poll for URL change before waitForLoadState, then networkidle ──
// Original:
//   if (requestedNavigation) {
//     await tab.page.mainFrame().waitForLoadState("load", { timeout: 1e4 }).catch(() => {
//     });
//     return result;
//   }
//
// Patched:
//   if (requestedNavigation) {
//     const _navStartUrl = tab.page.url();
//     const _navDeadline = Date.now() + 3e3;
//     while (Date.now() < _navDeadline && tab.page.url() === _navStartUrl) {
//       await new Promise((f) => setTimeout(f, 100));
//     }
//     await tab.page.mainFrame().waitForLoadState("load", { timeout: 1e4 }).catch(() => {
//     });
//     // Wait for networkidle so JS-rendered content (lazy sections, etc.) is
//     // fully painted before a screenshot can be taken.  5 s timeout is generous
//     // but capped so a chatty page never blocks indefinitely.
//     await tab.page.mainFrame().waitForLoadState("networkidle", { timeout: 5e3 }).catch(() => {
//     });
//     return result;
//   }
const PATCH2_OLD = `  if (requestedNavigation) {
    await tab.page.mainFrame().waitForLoadState("load", { timeout: 1e4 }).catch(() => {
    });
    return result;
  }`;
const PATCH2_NEW = `  if (requestedNavigation) {
    const _navStartUrl = tab.page.url();
    const _navDeadline = Date.now() + 3e3;
    while (Date.now() < _navDeadline && tab.page.url() === _navStartUrl) {
      await new Promise((f) => setTimeout(f, 100));
    }
    await tab.page.mainFrame().waitForLoadState("load", { timeout: 1e4 }).catch(() => {
    });
    await tab.page.mainFrame().waitForLoadState("networkidle", { timeout: 5e3 }).catch(() => {
    });
    return result;
  }`;

if (!src.includes(PATCH2_OLD)) {
  if (src.includes('_navStartUrl')) {
    console.log('[patch-playwright-utils] Patch 2 already applied, skipping.');
  } else {
    throw new Error(
      `[patch-playwright-utils] PATCH 2 FAILED: target string not found in ${UTILS_PATH}\n` +
      'The playwright-core version may have changed. Update the patch.'
    );
  }
} else {
  src = src.replace(PATCH2_OLD, PATCH2_NEW);
  console.log('[patch-playwright-utils] Patch 2 applied: URL-change poll before waitForLoadState.');
}

fs.writeFileSync(UTILS_PATH, src, 'utf8');
console.log('[patch-playwright-utils] Done. utils.js patched successfully.');
