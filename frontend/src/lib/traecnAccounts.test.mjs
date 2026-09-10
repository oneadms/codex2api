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

const modelsUi = /TraeCNModelsCell/;
const modelsModal = /TraeCNModelsModal/;

test("Trae CN model column exposes the full model list instead of a bare +N", () => {
  assert.match(source, modelsUi);
  assert.match(source, modelsModal);
  // 单元格里的「全部」入口必须可点击并带上剩余数量。
  assert.match(source, /modelsViewAll", \{ count: resolved\.length \}/);
  // 旧的 "+41" 纯文本截断（看不出有哪些模型）必须已被替换。
  assert.doesNotMatch(source, /\+\{accountModels\.length - 3\}/);
});

test("Trae CN model modal offers search and copy affordances", () => {
  assert.match(source, /modelsSearchPlaceholder/);
  assert.match(source, /modelsCopyAll/);
  assert.match(source, /modelsShowing", \{ shown: filtered\.length, total: resolved\.length \}/);
  // 继承全局目录与账号自有模型必须区分展示。
  assert.match(source, /modelsInherited/);
  assert.match(source, /modelsOwn/);
});
