import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const cell = readFileSync(
  new URL("../components/TraeCNCreditsCell.tsx", import.meta.url),
  "utf8",
);
const types = readFileSync(new URL("../types.ts", import.meta.url), "utf8");

const locales = ["zh", "en", "zh-TW"].map((name) => [
  name,
  JSON.parse(readFileSync(new URL(`../locales/${name}.json`, import.meta.url), "utf8")),
]);

test("Trae CN credits are typed as per-client pools", () => {
  assert.match(types, /export type TraeCNCreditsPoolKind = 'code' \| 'work'/);
  assert.match(types, /export interface TraeCNCreditsPool \{[\s\S]*?kind: TraeCNCreditsPoolKind/);
  assert.match(types, /export interface TraeCNCreditsSnapshot \{\s*\n\s*pools: TraeCNCreditsPool\[\]/);
});

test("Trae CN credit cell renders one block per pool instead of a merged balance", () => {
  // 两个池分开消耗，单元格必须遍历 pools，并且不能回落到旧的合并字段。
  assert.match(cell, /const pools = data\?\.pools \?\? \[\]/);
  assert.match(cell, /pools\.map\(\(pool, index\)/);
  assert.doesNotMatch(cell, /data\.total|data\.used_percent/);
  // 进度条与数字都取当前池的值。
  assert.match(cell, /Math\.min\(100, pool\.used_percent\)/);
  assert.match(cell, /format\(pool\.remaining\)/);
});

test("Trae CN credit cell labels the two pools and keeps one refresh control", () => {
  assert.match(cell, /code: "traecn\.creditsPoolCode"/);
  assert.match(cell, /work: "traecn\.creditsPoolWork"/);
  assert.match(cell, /t\("traecn\.creditsPoolUnavailable"\)/);
  // 刷新按钮只出现在第一块，避免每行都挂一个重复控件。
  assert.match(cell, /refreshButton=\{index === 0 \? refreshButton : null\}/);
});

test("a failed pool shows its own error without hiding the other pool", () => {
  assert.match(cell, /if \(pool\.error\) \{/);
  assert.match(cell, /title=\{pool\.error\}/);
  // 只有两个池都没有数据时才显示整列失败。
  assert.match(cell, /if \(pools\.length === 0\) \{[\s\S]*?traecn\.creditsUnavailable/);
});

test("Trae CN account dialog can pin the credit pool per account", () => {
  const page = readFileSync(new URL("../pages/TraeCNAccounts.tsx", import.meta.url), "utf8");
  assert.match(page, /creditsPool: "auto" as TraeCNCreditsPoolMode/);
  assert.match(page, /creditsPool: account\.traecn_credits_pool \?\? "auto"/);
  assert.match(page, /credits_pool: editForm\.creditsPool/);
  for (const mode of ["auto", "code", "work"]) {
    assert.match(page, new RegExp(`value: "${mode}"`), `${mode} 选项缺失`);
  }
  // 仓库约定：下拉必须用共享 Select，而不是裸 <select>。
  assert.doesNotMatch(page, /<select[\s>]/);
});

test("every locale names both pools and states they are spent separately", () => {
  for (const [name, locale] of locales) {
    const traecn = locale.traecn;
    assert.equal(traecn.creditsPoolCode, "Code", `${name} Code 池名称`);
    assert.equal(traecn.creditsPoolWork, "Work", `${name} Work 池名称`);
    assert.ok(traecn.creditsPoolUnavailable, `${name} 缺少单池失败文案`);
    assert.ok(traecn.creditsPoolProgress.includes("{{pool}}"), `${name} 进度条 label 缺少池名`);
    assert.ok(traecn.creditsPoolProgressValue.includes("{{pool}}"), `${name} 进度条 value 缺少池名`);
    assert.match(traecn.creditsScope, /Code[\s\S]*Work/, `${name} 说明未区分两个池`);
  }
});

test("credit availability follows the pool split and the account pool mode", async () => {
  const { traeCNCreditsAvailability, traeCNCreditsBadgeKey } = await import("./traecnCredits.ts");
  const pools = (code, work) => ([
    { kind: "code", total: 100, used: 100 - code, remaining: code, used_percent: 100 - code },
    { kind: "work", total: 100, used: 100 - work, remaining: work, used_percent: 100 - work },
  ]);

  // Code 池还有额度：正常状态，不加徽章。
  assert.equal(traeCNCreditsAvailability("auto", pools(20, 0)), "ok");
  assert.equal(traeCNCreditsBadgeKey("ok"), "");
  // Code 用尽、Work 还有：auto 会切池，属于可用但要标注。
  assert.equal(traeCNCreditsAvailability("auto", pools(0, 50)), "work_only");
  assert.match(traeCNCreditsBadgeKey("work_only"), /creditsWorkOnlyBadge/);
  // 两个池都空：这才是"额度用尽"。
  assert.equal(traeCNCreditsAvailability("auto", pools(0, 0)), "exhausted");
  assert.match(traeCNCreditsBadgeKey("exhausted"), /creditsExhaustedBadge/);
  // 强制只用 Code：Code 空就是耗尽，即使 Work 还有额度。
  assert.equal(traeCNCreditsAvailability("code", pools(0, 50)), "exhausted");
  // 强制走 Work：只看 Work 池。
  assert.equal(traeCNCreditsAvailability("work", pools(50, 0)), "exhausted");
  assert.equal(traeCNCreditsAvailability("work", pools(0, 50)), "work_only");
  // 查不到（查询失败 / 还没查）时不做判断，绝不显示"已用尽"。
  assert.equal(traeCNCreditsAvailability("auto", undefined), "unknown");
  assert.equal(traeCNCreditsAvailability("auto", [
    { kind: "code", total: 0, used: 0, remaining: 0, used_percent: 0, error: "上游超时" },
  ]), "unknown");
  assert.equal(traeCNCreditsBadgeKey("unknown"), "");
});

test("the account list renders the credit state next to the status badge", () => {
  const page = readFileSync(new URL("../pages/TraeCNAccounts.tsx", import.meta.url), "utf8");
  assert.match(page, /traeCNCreditsAvailability\(account\.traecn_credits_pool/);
  assert.match(page, /account\.traecn_credits_state/);
  assert.match(page, /title=\{t\("traecn\.creditsStateHint"\)\}/);
  // 单元格在 100% 时要明说已用尽，而不是只显示"剩余 0"。
  assert.match(cell, /traecn\.creditsPoolExhausted/);
});
