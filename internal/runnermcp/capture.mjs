// Glimmung-owned browser capture helper. It is embedded into the runner MCP
// binary and invoked ONLY by the capture_video / capture_screenshot tools — it
// is never a per-repo script an agent can copy or call directly.
//
// Unlike the retired capture-video.mjs, this connects to the leased slot
// browser (the canonical surface) and, for video, records with page.screencast
// which starts AFTER the page is visible. recordVideo started at context
// creation and baked in the about:blank white first frame; screencast does not.
import { mkdir } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

const playwrightModule = process.env.PLAYWRIGHT_PACKAGE_PATH
  ? pathToFileURL(process.env.PLAYWRIGHT_PACKAGE_PATH).href
  : "playwright";
const { chromium } = await import(playwrightModule);

function arg(flag, fallback = "") {
  const i = process.argv.indexOf(flag);
  return i === -1 || i === process.argv.length - 1 ? fallback : process.argv[i + 1];
}

const kind = arg("--kind", "video"); // "video" | "screenshot"
const url = arg("--url");
const output = arg("--output");
const waitMs = Number(arg("--wait-ms", "3000"));
const width = Number(arg("--width", "1280"));
const height = Number(arg("--height", "720"));
const click = arg("--click");
const triggerUrl = arg("--trigger-url");
const fullPage = arg("--full-page", "true") !== "false";

if (!url) throw new Error("--url is required");
if (!output) throw new Error("--output is required");

const wsEndpoint =
  process.env.PLAYWRIGHT_WS_ENDPOINT || process.env.GLIMMUNG_PLAYWRIGHT_WS_ENDPOINT;
if (!wsEndpoint) {
  throw new Error("PLAYWRIGHT_WS_ENDPOINT is required (no leased slot browser)");
}

const out = path.resolve(output);
await mkdir(path.dirname(out), { recursive: true });

const browser = await chromium.connect(wsEndpoint);
try {
  const context = await browser.newContext({ viewport: { width, height } });
  const page = await context.newPage();

  // Navigate and wait for the page to be visible BEFORE any recording starts,
  // so the first captured frame is real content — never a white about:blank.
  await page.goto(url, { waitUntil: "domcontentloaded" });
  await page.locator("body").waitFor({ state: "visible", timeout: 30000 });

  async function interact() {
    if (click) await page.locator(click).click();
    if (triggerUrl) {
      const res = await page.request.post(triggerUrl);
      if (!res.ok()) {
        throw new Error(`trigger POST failed ${res.status()}: ${triggerUrl}`);
      }
    }
  }

  if (kind === "screenshot") {
    await interact();
    if (waitMs > 0) await page.waitForTimeout(waitMs);
    await page.screenshot({ path: out, fullPage });
  } else {
    // Record only once the page has actually PAINTED non-blank pixels. `body`
    // becoming visible fires before a canvas/WASM effect draws its first frame,
    // so starting the screencast then captures the empty pre-paint page — the
    // blank "flashbang" frame the server-side first-frame gate rejects (it flags
    // near-uniformity, white OR dark). A blank page compresses to a tiny PNG, so
    // poll a screenshot until it carries real content, bounded by a deadline; the
    // gate stays the backstop if the page never paints.
    const paintDeadline = Date.now() + 10000;
    for (;;) {
      const probe = await page.screenshot();
      if (probe.length > 6000 || Date.now() > paintDeadline) break;
      await page.waitForTimeout(400);
    }
    await page.screencast.start({ path: out, size: { width, height } });
    await interact();
    await page.waitForTimeout(waitMs);
    await page.screencast.stop();
  }

  await context.close();
  console.log(JSON.stringify({ kind, output: out, url }));
} finally {
  await browser.close();
}
