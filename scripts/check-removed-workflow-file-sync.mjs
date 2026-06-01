#!/usr/bin/env node
// Repo-wide guard for the retired repo-backed workflow file sync path.
// Per docs/migration-policy.md, workflow shape lives in durable Glimmung
// registration; project repositories must not carry workflow manifests.

import fs from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

const ignoredDirs = new Set([
  ".git",
  ".terraform",
  ".venv",
  "__pycache__",
  "build",
  "coverage",
  "dist",
  "node_modules",
  "venv",
]);

const ignoredFiles = new Set([
  "package-lock.json",
  "pnpm-lock.yaml",
  "yarn.lock",
]);

const allowedRelativePaths = new Set([
  "scripts/check-removed-workflow-file-sync.mjs",
]);

const blockedContent = [
  { name: "repo workflow manifest path", pattern: /\.glimmung\/workflows/ },
  { name: "workflow sync MCP check", pattern: /\bcheck_workflow_updates\b/ },
  { name: "workflow sync MCP apply", pattern: /\bsync_workflow\b/ },
  { name: "workflow sync API source", pattern: /\bworkflow_sync_api\b/ },
  { name: "workflow sync client", pattern: /\bWorkflowSyncClient\b/ },
  { name: "workflow file fetcher", pattern: /\bFetchWorkflowFile\b/ },
  { name: "workflow upstream handler", pattern: /\bgetWorkflowUpstream\b/ },
  { name: "workflow sync handler", pattern: /\bsyncWorkflow\b/ },
  { name: "workflow YAML parser", pattern: /\bparseWorkflowYAML\b/ },
  { name: "workflow upstream comparator", pattern: /\bworkflowsInSync\b/ },
  { name: "workflow upstream route", pattern: /\/v1\/projects\/\{project\}\/workflows\/\{name\}\/upstream/ },
  { name: "workflow sync route", pattern: /\/v1\/projects\/\{project\}\/workflows\/\{name\}\/sync/ },
];

async function walk(dir) {
  const out = [];
  const entries = await fs.readdir(dir, { withFileTypes: true });
  for (const entry of entries) {
    if (ignoredDirs.has(entry.name)) continue;
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      out.push(...(await walk(full)));
    } else if (entry.isFile()) {
      if (ignoredFiles.has(entry.name)) continue;
      out.push(full);
    }
  }
  return out;
}

const files = await walk(repoRoot);
const failures = [];

for (const file of files) {
  const rel = path.relative(repoRoot, file).replaceAll("\\", "/");
  if (allowedRelativePaths.has(rel)) continue;

  let content;
  try {
    content = await fs.readFile(file, "utf8");
  } catch {
    continue;
  }

  for (const rule of blockedContent) {
    const match = content.match(rule.pattern);
    if (match) {
      failures.push({ file: rel, rule: rule.name, match: match[0] });
    }
  }
}

if (failures.length > 0) {
  console.error("check-removed-workflow-file-sync: retired workflow file sync references found:");
  for (const f of failures) {
    console.error(`  ${f.file}: ${f.rule} (${f.match})`);
  }
  console.error("\nUse durable Glimmung workflow registration. Repo-backed workflow manifests are retired.");
  process.exit(1);
}

console.log(`check-removed-workflow-file-sync: clean (${files.length} files scanned)`);
