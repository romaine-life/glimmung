#!/usr/bin/env node
// Migration guard for the chart-image-tag drift fix.
//
// The prod chart (k8s/values.yaml) and the per-issue/per-slot chart
// (k8s/issue/values.yaml) MUST share the same `image.tag`. The build
// workflow bumps both files atomically; this guard fails CI if anyone
// reintroduces drift by hand or removes one file's pin.
//
// Drift here is what caused the slot SPA crash investigated on
// 2026-05-28: the slot's image tag was a stale literal that hadn't
// followed prod, so warm slots ran an older glimmung image than prod
// and tripped a bug already fixed upstream. The lockstep guarantee
// belongs in CI, not in tribal knowledge.
//
// Per .tank/docs/migration-policy.md, the retired "per-project Postgres
// `image.tag` override as the slot's source of truth" path stays
// deleted. Slot dispatch now relies on this chart default; if the two
// files disagree, slot installs deploy something different from prod
// without warning. This script is the load-bearing guard.

import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

const files = ["k8s/values.yaml", "k8s/issue/values.yaml"];

// Extract the `image.tag` value from a YAML file via a deliberately
// shallow line scan — we don't want to pull in a YAML dep, and both
// files keep the field at the same two-space indent. If the format
// changes, this guard fails loudly rather than silently miss the
// field, which is the behaviour we want.
function extractImageTag(text, filePath) {
  const lines = text.split(/\r?\n/);
  let inImage = false;
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (/^image:\s*$/.test(line)) {
      inImage = true;
      continue;
    }
    // Image block ends at the next non-indented, non-comment, non-empty line.
    if (inImage && line.length > 0 && !line.startsWith(" ") && !line.startsWith("#")) {
      inImage = false;
    }
    if (!inImage) continue;
    const m = /^  tag:\s*"?([^"\s#]+)"?\s*(#.*)?$/.exec(line);
    if (m) return m[1];
  }
  throw new Error(`${filePath}: could not find image.tag at expected two-space indent under image:`);
}

// Extract livePreview.image.tag — the live-preview-edge image tag, pinned at
// four-space indent under the top-level `livePreview:` block's `  image:`
// sub-block. Same drift risk, same fix: the live-preview-edge build workflow
// bumps it in BOTH chart values files on main, and this guard fails CI if they
// disagree, so the preview provision never points at a floating, maybe-missing
// edge tag. See docs/live-preview-plan.md Stage 4a.
function extractLivePreviewEdgeTag(text, filePath) {
  const lines = text.split(/\r?\n/);
  let inLivePreview = false;
  let inImage = false;
  for (const line of lines) {
    if (/^livePreview:\s*$/.test(line)) {
      inLivePreview = true;
      inImage = false;
      continue;
    }
    // livePreview block ends at the next non-indented, non-comment, non-empty line.
    if (inLivePreview && line.length > 0 && !line.startsWith(" ") && !line.startsWith("#")) {
      inLivePreview = false;
      inImage = false;
    }
    if (!inLivePreview) continue;
    if (/^  image:\s*$/.test(line)) {
      inImage = true;
      continue;
    }
    if (!inImage) continue;
    const m = /^    tag:\s*"?([^"\s#]+)"?\s*(#.*)?$/.exec(line);
    if (m) return m[1];
    // A new two-space key (not a deeper line, not a comment) closes the image block.
    if (/^  \S/.test(line) && !/^ {4}/.test(line) && !/^\s*#/.test(line)) {
      inImage = false;
    }
  }
  throw new Error(
    `${filePath}: could not find livePreview.image.tag at four-space indent under "livePreview:" then "  image:"`,
  );
}

const results = await Promise.all(
  files.map(async (rel) => {
    const abs = path.join(repoRoot, rel);
    const text = await fs.readFile(abs, "utf8");
    return {
      file: rel,
      tag: extractImageTag(text, rel),
      edgeTag: extractLivePreviewEdgeTag(text, rel),
    };
  })
);

// Each lockstep-pinned image tag must be identical across both chart values
// files. `field` names the value; `pick` selects it from a parsed result;
// `workflow` is the build workflow that keeps it in sync.
const checks = [
  { field: "image.tag", pick: (r) => r.tag, workflow: ".github/workflows/build.yaml" },
  {
    field: "livePreview.image.tag",
    pick: (r) => r.edgeTag,
    workflow: ".github/workflows/live-preview-edge-build.yaml",
  },
];

let failed = false;
for (const check of checks) {
  const tags = new Set(results.map(check.pick));
  if (tags.size !== 1) {
    const detail = results.map((r) => `  ${r.file}: ${check.pick(r)}`).join("\n");
    console.error(
      `${check.field} drift between chart values files:\n` +
        detail +
        "\n\nThe build workflow bumps both files atomically on every push.\n" +
        "If you edited one by hand, edit the other to match (or rerun the\n" +
        `build workflow. See ${check.workflow}.\n`,
    );
    failed = true;
    continue;
  }
  console.log(`${check.field} consistent across ${files.length} files: ${[...tags][0]}`);
}

if (failed) {
  process.exit(1);
}
