import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const source = readFileSync(
  new URL("../pages/TraeCNAccounts.tsx", import.meta.url),
  "utf8",
);

test("Trae CN account fallback identifies the value as a database ID", () => {
  assert.match(source, /title=\{account\.email \|\| `ID \$\{account\.id\}`\}/);
  assert.match(source, /\{account\.email \|\| `ID \$\{account\.id\}`\}/);
  assert.doesNotMatch(source, /`#\$\{account\.id\}`/);
});
