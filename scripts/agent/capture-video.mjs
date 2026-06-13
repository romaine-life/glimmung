import { mkdir, stat, writeFile } from "node:fs/promises";
import path from "node:path";
import { pathToFileURL } from "node:url";

const playwrightModule =
  process.env.PLAYWRIGHT_PACKAGE_PATH
    ? pathToFileURL(process.env.PLAYWRIGHT_PACKAGE_PATH).href
    : "playwright";
const { chromium } = await import(playwrightModule);

function readArg(flag, fallback = "") {
  const index = process.argv.indexOf(flag);
  if (index === -1 || index === process.argv.length - 1) {
    return fallback;
  }
  return process.argv[index + 1];
}

function hasFlag(flag) {
  return process.argv.includes(flag);
}

function evidenceRefFor(filePath) {
  const evidenceRoot = path.resolve(process.env.EVIDENCE_DIR || "/workspace/evidence");
  const relative = path.relative(evidenceRoot, filePath);
  if (relative && !relative.startsWith("..") && !path.isAbsolute(relative)) {
    return relative.split(path.sep).join("/");
  }
  return filePath;
}

const url = readArg("--url");
const output = readArg("--output");
const waitMs = Number(readArg("--wait-ms", "5000"));
const width = Number(readArg("--width", "1280"));
const height = Number(readArg("--height", "720"));
const click = readArg("--click");
const triggerUrl = readArg("--trigger-url");
const triggerDelayMs = Number(readArg("--trigger-delay-ms", "500"));
const readySelector = readArg("--ready-selector");
const startDelayMs = Number(readArg("--start-delay-ms", "500"));
const manifest = readArg("--manifest");

if (!url) {
  throw new Error("--url is required");
}

if (!output) {
  throw new Error("--output is required");
}

const out = path.resolve(output);
const videoDir = path.dirname(out);
await mkdir(videoDir, { recursive: true });

const browser = await chromium.launch({ headless: true });

try {
  const context = await browser.newContext({
    viewport: { width, height },
  });
  const page = await context.newPage();

  // Navigate and wait for the page to actually render BEFORE recording starts.
  //
  // We deliberately do not use context.recordVideo: it begins recording the
  // instant the context is created — before navigation — so every clip opened
  // with the white about:blank pre-navigation frame (Playwright #27253). For
  // evidence videos that blank lead is the first thing a reviewer sees.
  // page.screencast (Playwright >= 1.59) lets us start recording only once real
  // content is painted, so frame 0 is genuine evidence rather than a white shell.
  //
  // The readiness gate intentionally avoids networkidle: the dashboard holds an
  // open SSE stream, so the page never goes network-idle. Callers that know a
  // content selector pass --ready-selector for the strongest signal; otherwise a
  // short settle (--start-delay-ms) lets the SPA mount and paint.
  await page.goto(url, { waitUntil: "load" });
  await page.locator("body").waitFor({ state: "visible", timeout: 30000 });
  if (readySelector) {
    await page.locator(readySelector).waitFor({ state: "visible", timeout: 30000 });
  }
  if (startDelayMs > 0) {
    await page.waitForTimeout(startDelayMs);
  }

  // Record straight to the output path; screencast.stop() finalizes the webm.
  await page.screencast.start({ path: out, size: { width, height } });
  const startedAt = Date.now();

  if (click) {
    await page.locator(click).click();
  }
  if (triggerUrl) {
    await page.waitForTimeout(triggerDelayMs);
    const response = await page.request.post(triggerUrl);
    if (!response.ok()) {
      throw new Error(`trigger POST failed ${response.status()}: ${triggerUrl}`);
    }
  }
  await page.waitForTimeout(waitMs);

  await page.screencast.stop();
  const durationMs = Date.now() - startedAt;
  await context.close();

  const info = await stat(out);
  if (info.size <= 0) {
    throw new Error(`captured video is empty: ${out}`);
  }
  const payload = {
    kind: "video",
    ref: evidenceRefFor(out),
    label: path.basename(out, path.extname(out)),
    content_type: "video/webm",
    size_bytes: info.size,
    duration_ms: durationMs,
    width,
    height,
    url,
    trigger_url: triggerUrl || undefined,
  };
  if (manifest || hasFlag("--manifest")) {
    const manifestPath = manifest ? path.resolve(manifest) : `${out}.json`;
    await writeFile(manifestPath, JSON.stringify(payload, null, 2));
  }
  console.log(`captured video ${out} (${info.size} bytes)`);
} finally {
  await browser.close();
}
