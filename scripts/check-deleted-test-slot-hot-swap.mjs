#!/usr/bin/env node
import fs from "node:fs";
import path from "node:path";
import process from "node:process";

const root = process.cwd();
const join = (...parts) => parts.join("");
const word = (...parts) => new RegExp(`\\b${join(...parts)}\\b`);

const forbidden = [
  { re: word("apply", "TestSlot", "HotSwap"), label: "retired apply handler" },
  { re: word("TestSlotApply", "HotSwap", "Request"), label: "retired apply request type" },
  { re: word("Dispatch", "HotSwap"), label: "retired dispatch" },
  { re: word("resolve", "Artifact"), label: "retired resolver" },
  { re: word("Fid", "elity", "Command"), label: "retired command plumbing" },
  { re: word("artifact", "_kind"), label: "retired API field" },
  { re: word("fid", "elity", "_classifier"), label: "retired classifier contract" },
  { re: word("test_slot_", "hot_swap"), label: "retired contract key" },
  { re: new RegExp("/v1/test-slots/apply-" + "hot-swap"), label: "retired apply route" },
  { re: word("StartApply", "HotSwap", "JobWatcher"), label: "retired job watcher" },
];

const includeDirs = ["cmd", "internal", "k8s", "scripts"];
const ignored = new Set(["scripts/check-deleted-test-slot-hot-swap.mjs"]);

function* walk(dir) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (entry.name === ".git" || entry.name === "node_modules") continue;
    const full = path.join(dir, entry.name);
    const rel = path.relative(root, full);
    if (entry.isDirectory()) {
      yield* walk(full);
    } else if (!ignored.has(rel)) {
      yield rel;
    }
  }
}

const failures = [];
for (const dir of includeDirs) {
  const abs = path.join(root, dir);
  if (!fs.existsSync(abs)) continue;
  for (const rel of walk(abs)) {
    const text = fs.readFileSync(path.join(root, rel), "utf8");
    for (const check of forbidden) {
      if (check.re.test(text)) {
        failures.push(`${rel}: ${check.label}`);
      }
    }
  }
}

if (failures.length > 0) {
  console.error("retired test-slot hot-swap surface found:");
  for (const failure of failures) console.error(`- ${failure}`);
  process.exit(1);
}

console.log("retired test-slot hot-swap surface is absent");
