import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { extractBalancedBody } from "./sourceBoundary.mjs";

const accountsSource = readFileSync(
  new URL("../pages/Accounts.tsx", import.meta.url),
  "utf8",
);

test("account file imports have an imperative in-flight guard", () => {
  assert.match(accountsSource, /const importInFlightRef = useRef\(false\);/);

  const importFiles = extractBalancedBody(accountsSource, "const importFiles = async");
  const guard = "if (files.length === 0 || importInFlightRef.current) return;";
  assert.ok(importFiles.includes(guard), "importFiles must reject duplicate calls");
  assert.ok(
    importFiles.indexOf(guard) < importFiles.indexOf("importInFlightRef.current = true;"),
    "the guard must run before acquiring the lock",
  );
  assert.ok(
    importFiles.includes("importInFlightRef.current = false;"),
    "the lock must be released after success and failure",
  );
  assert.ok(
    importFiles.includes("finally {\n      setImporting(false);\n      importInFlightRef.current = false;"),
    "the lock must be released in the import finally block",
  );
});
