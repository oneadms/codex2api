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

test("Trae CN add dialog offers OAuth, RT and JSON import paths", () => {
  // 三种添加方式必须同屏可切换（与 Codex 账号页一致的 Tab 结构）。
  assert.match(source, /addMethod === "oauth"/);
  assert.match(source, /addMethod === "rt"/);
  assert.match(source, /addMethod === "json"/);
  assert.match(source, /traecn\.addMethodOAuth/);
  assert.match(source, /traecn\.addMethodRT/);
  assert.match(source, /traecn\.addMethodJSON/);
});

test("Trae CN OAuth panel drives start -> poll -> claim", () => {
  assert.match(source, /api\.startTraeCNOAuth/);
  assert.match(source, /api\.getTraeCNOAuthStatus/);
  assert.match(source, /api\.claimTraeCNOAuthAccount/);
  // 自动回调不通时的兜底：粘贴回调链接。
  assert.match(source, /api\.completeTraeCNOAuth/);
  assert.match(source, /traecn\.oauthSubmitCallback/);
  // 授权链接只提供复制：绝不自动打开浏览器（避免真实浏览器指纹被记录），
  // 管理员要在自己的指纹浏览器里登录。
  assert.match(source, /traecn\.oauthLinkLabel/);
  assert.match(source, /traecn\.oauthCopyLink/);
  assert.doesNotMatch(source, /window\.open/);
});

test("Trae CN page can export and import credentials as JSON", () => {
  assert.match(source, /api\.exportTraeCNAccounts/);
  assert.match(source, /api\.importTraeCNJSON/);
  assert.match(source, /downloadBlob\(blob/);
  assert.match(source, /traecn\.exportAccounts/);
  assert.match(source, /traecn\.jsonChooseFile/);
});

test("Trae CN list supports exporting only the selected accounts", () => {
  // 勾选行 -> 导出选中；不勾选才导出全部（后端 ids 过滤）。
  assert.match(source, /const \[selectedIDs, setSelectedIDs\] = useState<Set<number>>\(new Set\(\)\)/);
  assert.match(source, /toggleSelectPage/);
  assert.match(source, /const downloadAccounts = async \(ids\?: number\[\]\) => \{/);
  assert.match(source, /api\.exportTraeCNAccounts\(ids\)/);
  assert.match(source, /const exportAccountsSelected = \(\) => downloadAccounts\(Array\.from\(selectedIDs\)\)/);
  assert.match(source, /traecn\.exportSelected/);
});

test("Trae CN selection bar offers batch delete with confirmation", () => {
  assert.match(source, /api\.batchDeleteAccounts\(ids\)/);
  assert.match(source, /traecn\.batchDeleteTitle/);
  assert.match(source, /traecn\.batchDeleteDesc/);
  assert.match(source, /traecn\.cancelSelection/);
  // 删除必须先走确认弹窗，并且清空选择后刷新列表。
  assert.match(source, /setSelectedIDs\(new Set\(\)\);\s*\n\s*await reload\(true\);/);
});

test("Trae CN JSON import previews parsed entries so specific ones can be picked", () => {
  assert.match(source, /parseTraeCNImportJSON/);
  assert.match(source, /selectTraeCNImportEntries\(jsonText, picked\)/);
  assert.match(source, /traecn\.jsonPreviewTitle/);
  assert.match(source, /traecn\.jsonSelectedCount/);
  assert.match(source, /toast|showToast/);
});

test("Trae CN account row shows the per-account device code", () => {
  // 一个账号绑定一个设备码：列表里要能直接看到，便于核对有没有串号。
  assert.match(source, /traecn\.deviceCode/);
  assert.match(source, /account\.traecn_machine_id \|\| account\.traecn_device_id/);
  assert.match(source, /function shortDeviceCode/);
});

test("Trae CN OAuth panel leads with the paste-back flow", () => {
  // Trae 只接受 http://127.0.0.1:<port>/authorize，远端部署时回调不可达，
  // 所以粘贴回调链接是主路径，必须排在回调地址说明前面。
  assert.match(source, /traecn\.oauthPasteTitle/);
  assert.match(source, /traecn\.oauthPasteHint/);
  assert.ok(
    source.indexOf("traecn.oauthPasteTitle") < source.indexOf("traecn.oauthCallbackLabel"),
    "paste panel must come before the raw callback address block",
  );
});
