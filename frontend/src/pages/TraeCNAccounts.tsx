import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  CheckCircle2,
  ChevronDown,
  CircleAlert,
  Copy,
  Edit3,
  Layers,
  CalendarCheck,
  ListRestart,
  Loader2,
  Plus,
  RefreshCw,
  Search,
  Trash2,
  Power,
  PowerOff,
  Zap,
  X,
} from "lucide-react";
import { api, getAdminKey } from "../api";
import type {
  AccountGroup,
  AccountRow,
  AddTraeCNAccountsResponse,
  TraeCNImportItem,
} from "../types";
import PageHeader from "../components/PageHeader";
import StateShell from "../components/StateShell";
import Pagination from "../components/Pagination";
import StatusBadge from "../components/StatusBadge";
import AccountGroupMultiSelect from "../components/AccountGroupMultiSelect";
import ModelLogo from "../components/ModelLogo";
import Modal from "../components/Modal";
import { CompactStat } from "../components/CompactStat";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { Badge } from "@/components/ui/badge";
import { useToast } from "../hooks/useToast";
import { useConfirmDialog } from "../hooks/useConfirmDialog";
import { usePersistedPageSize, DEFAULT_PAGE_SIZE_OPTIONS } from "../hooks/usePersistedPageSize";
import { getErrorMessage } from "../utils/error";
import { cn } from "@/lib/utils";

const DEFAULT_HOST = "https://trae-api-cn.mchost.guru";

const DEFAULT_TRAE_MODELS = [
	"claude-3.5-sonnet",
	"claude-3.7-sonnet",
	"claude-haiku-4-5",
	"claude-haiku-4-5-20251001",
	"claude-opus-4-5",
	"claude-opus-4-5-20251101",
	"claude-opus-4-7",
	"claude-opus-4-6",
	"claude-sonnet-4",
	"claude-sonnet-4-5",
	"claude-sonnet-4-5-20250929",
	"claude-sonnet-4-6",
	"deepseek-r1",
	"deepseek-v3",
	"deepseek-v3-1",
	"deepseek-v4-flash",
	"deepseek-v4-pro",
	"doubao-1-6",
	"doubao-1.8",
	"doubao-seed-2-1-pro",
	"doubao-seed-2-1-turbo",
	"doubao-seed-2.0-code",
	"doubao-seed-code",
	"gemini-2.0-flash",
	"gemini-2.5-pro",
	"glm-4.6",
	"glm-4.7",
	"glm-5",
	"glm-5.1",
	"glm-5.2",
	"glm-5v-turbo",
	"gpt-4o",
	"gpt-4o-mini",
	"kimi-k2",
	"kimi-k2-5",
	"kimi-k2-7-code",
	"kimi-k2.6",
	"minimax-m2.7",
	"minimax-m3",
	"qwen-3-5",
	"qwen3-coder",
	"qwen3.6-plus",
	"qwen3.7-plus",
	"auto",
];

type StatusFilter = "all" | "active" | "disabled" | "error";

function parseLines(value: string): string[] {
  const seen = new Set<string>();
  const result: string[] = [];
  for (const raw of value.split(/[\n,\r\t]+/)) {
    const item = raw.trim();
    if (!item || seen.has(item.toLowerCase())) continue;
    seen.add(item.toLowerCase());
    result.push(item);
  }
  return result;
}

function accountLabel(account: AccountRow): string {
  return account.name?.trim() || account.email?.trim() || account.traecn_host?.trim() || `ID ${account.id}`;
}

function ImportResult({ result }: { result: AddTraeCNAccountsResponse }) {
  const { t } = useTranslation();
  return (
    <div className="space-y-2 rounded-lg border border-border bg-muted/25 p-3">
      <div className="flex flex-wrap items-center gap-2 text-sm font-semibold">
        <CheckCircle2 className="size-4 text-emerald-600" />
        {t("traecn.importSummary", { success: result.success, failed: result.failed })}
      </div>
      <div className="max-h-44 space-y-1 overflow-y-auto pr-1">
        {result.items.map((item: TraeCNImportItem) => (
          <div key={`${item.index}-${item.id ?? "error"}`} className="flex items-start gap-2 text-xs">
            {item.stored ? (
              <CheckCircle2 className="mt-0.5 size-3.5 shrink-0 text-emerald-600" />
            ) : (
              <CircleAlert className="mt-0.5 size-3.5 shrink-0 text-destructive" />
            )}
            <span className="min-w-0 break-all text-muted-foreground">
              #{item.index} {item.name || ""}
              {item.user_id ? ` · ${item.user_id}` : ""}
              {item.error ? ` · ${item.error}` : ""}
              {item.warning ? ` · ${item.warning}` : ""}
            </span>
          </div>
        ))}
      </div>
    </div>
  );
}

interface TraeCNTestEvent {
  type: "test_start" | "content" | "test_complete" | "error";
  text?: string;
  model?: string;
  success?: boolean;
  error?: string;
}

function TraeCNTestConnectionModal({
  account,
  availableModels,
  preferredModel,
  onClose,
  onSettled,
}: {
  account: AccountRow;
  availableModels: string[];
  preferredModel?: string;
  onClose: () => void;
  onSettled: () => void;
}) {
  const { t } = useTranslation();
  const [output, setOutput] = useState<string[]>([]);
  const [status, setStatus] = useState<"idle" | "connecting" | "streaming" | "success" | "error">("idle");
  const [model, setModel] = useState("");
  const testModels = useMemo(() => {
    const seen = new Set<string>();
    const result: string[] = [];
    for (const raw of availableModels.length ? availableModels : DEFAULT_TRAE_MODELS) {
      const value = raw.trim();
      const key = value.toLowerCase();
      if (!value || key.includes("image") || seen.has(key)) continue;
      seen.add(key);
      result.push(value);
    }
    return result;
  }, [availableModels]);
  const [selectedModel, setSelectedModel] = useState("");
  const [errorMessage, setErrorMessage] = useState("");
  const abortRef = useRef<AbortController | null>(null);
  const outputEndRef = useRef<HTMLDivElement>(null);
  const settledRef = useRef(false);
  const onSettledRef = useRef(onSettled);
  onSettledRef.current = onSettled;

  const markSettled = useCallback(() => {
    if (settledRef.current) return;
    settledRef.current = true;
    onSettledRef.current();
  }, []);

  useEffect(() => {
    const isSelectedModelAvailable = testModels.some((candidate) => candidate.toLowerCase() === selectedModel.toLowerCase());
    if (isSelectedModelAvailable) return;
    const preferred = preferredModel?.trim();
    const preferredModelInCatalog = preferred && testModels.find((candidate) => candidate.toLowerCase() === preferred.toLowerCase());
    setSelectedModel(preferredModelInCatalog || testModels[0] || "auto");
  }, [preferredModel, selectedModel, testModels]);

  const runTest = useCallback(async () => {
    if (!selectedModel || status === "connecting" || status === "streaming") return;
    const controller = new AbortController();
    abortRef.current = controller;
    settledRef.current = false;
    setOutput([]);
    setModel(selectedModel);
    setErrorMessage("");
    setStatus("connecting");

    try {
      const adminKey = getAdminKey();
      const query = new URLSearchParams({ model: selectedModel });
      const response = await fetch(`/api/admin/accounts/${account.id}/test?${query.toString()}`, {
        signal: controller.signal,
        headers: adminKey ? { "X-Admin-Key": adminKey } : undefined,
      });

      if (!response.ok) {
        const body = await response.text();
        let message = `HTTP ${response.status}`;
        try {
          const parsed = JSON.parse(body) as { error?: string };
          if (parsed.error) message = parsed.error;
        } catch {
          if (body.trim()) message = body.trim();
        }
        setStatus("error");
        setErrorMessage(message);
        markSettled();
        return;
      }

      const reader = response.body?.getReader();
      if (!reader) {
        setStatus("error");
        setErrorMessage(t("accounts.browserStreamingUnsupported"));
        markSettled();
        return;
      }

      const decoder = new TextDecoder();
      let buffer = "";
      let receivedTerminalEvent = false;

      const processLines = (lines: string[]) => {
        for (const line of lines) {
          const trimmed = line.trim();
          if (!trimmed.startsWith("data: ")) continue;
          try {
            const event = JSON.parse(trimmed.slice(6)) as TraeCNTestEvent;
            switch (event.type) {
              case "test_start":
                setModel(event.model || selectedModel);
                setStatus("streaming");
                break;
              case "content":
                if (event.text) setOutput((previous) => [...previous, event.text!]);
                break;
              case "test_complete":
                receivedTerminalEvent = true;
                setStatus(event.success ? "success" : "error");
                break;
              case "error":
                receivedTerminalEvent = true;
                setStatus("error");
                setErrorMessage(event.error || t("accounts.unknownError"));
                break;
            }
          } catch {
            // Ignore incomplete or non-JSON SSE lines.
          }
        }
      };

      while (true) {
        const { done, value } = await reader.read();
        if (done) {
          buffer += decoder.decode();
          break;
        }
        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split("\n");
        buffer = lines.pop() || "";
        processLines(lines);
      }
      if (buffer.trim()) processLines([buffer]);

      if (receivedTerminalEvent) {
        markSettled();
      } else {
        setStatus("error");
        setErrorMessage(t("accounts.connectionEndedUnexpectedly"));
        markSettled();
      }
    } catch (error: unknown) {
      if (error instanceof DOMException && error.name === "AbortError") return;
      setStatus("error");
      setErrorMessage(error instanceof Error ? error.message : t("accounts.connectionFailed"));
      markSettled();
    }
  }, [account.id, markSettled, selectedModel, status, t]);

  useEffect(() => () => {
    abortRef.current?.abort();
  }, []);

  useEffect(() => {
    outputEndRef.current?.scrollIntoView({ behavior: "smooth" });
  }, [output]);

  const statusText = {
    idle: t("traecn.selectTestModel"),
    connecting: t("accounts.connecting"),
    streaming: t("accounts.receivingResponse"),
    success: t("accounts.testSuccess"),
    error: t("accounts.testFailed"),
  }[status];
  const StatusIcon = status === "success" ? CheckCircle2 : status === "error" ? CircleAlert : Loader2;
  const statusColor = status === "success" ? "text-emerald-600" : status === "error" ? "text-destructive" : "text-muted-foreground";

  return (
    <Modal
      show
      title={t("accounts.testConnectionTitle", { account: accountLabel(account) })}
      onClose={() => {
        abortRef.current?.abort();
        onClose();
      }}
      footer={
        <div className="flex w-full items-center justify-end gap-2">
          <Button
            variant="outline"
            onClick={() => {
              abortRef.current?.abort();
              onClose();
            }}
          >
            {t("common.close")}
          </Button>
          <Button onClick={() => void runTest()} disabled={!selectedModel || status === "connecting" || status === "streaming"}>
            {status === "connecting" || status === "streaming" ? <Loader2 className="size-4 animate-spin" /> : <Zap className="size-4" />}
            {status === "connecting" || status === "streaming" ? t("accounts.testing") : t("traecn.startTest")}
          </Button>
        </div>
      }
      contentClassName="sm:max-w-[680px]"
    >
      <div className="space-y-4">
        <label className="block space-y-1.5">
          <span className="text-xs font-semibold text-muted-foreground">{t("traecn.selectTestModel")}</span>
          <Select
            value={selectedModel}
            onValueChange={setSelectedModel}
            options={testModels.map((value) => ({ label: value, value }))}
            disabled={status === "connecting" || status === "streaming"}
          />
        </label>

        <div className={`flex items-center gap-1.5 text-sm font-semibold ${statusColor}`}>
          <StatusIcon className={cn("size-4", (status === "connecting" || status === "streaming") && "animate-spin")} />
          <span>{statusText}</span>
          {model ? <span className="font-mono text-xs font-normal text-muted-foreground">· {model}</span> : null}
        </div>

        {(output.length > 0 || status === "connecting" || status === "streaming") && (
          <div className="min-h-[80px] max-h-[240px] overflow-auto whitespace-pre-wrap break-all rounded-lg border border-border bg-muted/30 p-3 text-[13px] leading-relaxed" style={{ fontFamily: "var(--font-geist-mono)" }}>
            {output.length === 0 && status === "connecting" ? <span className="animate-pulse text-muted-foreground">{t("accounts.sendingTestRequest")}</span> : null}
            {output.join("")}
            <div ref={outputEndRef} />
          </div>
        )}

        {errorMessage ? (
          <div className="rounded-xl border border-red-200 bg-red-50 p-3.5 text-red-600 dark:border-red-900/50 dark:bg-red-950/30 dark:text-red-400">
            <div className="mb-2 text-sm font-semibold">{t("accounts.failureDetails")}</div>
            <pre className="max-h-[34vh] overflow-auto whitespace-pre-wrap break-all text-[13px] leading-relaxed" style={{ fontFamily: "var(--font-geist-mono)" }}>{errorMessage}</pre>
          </div>
        ) : null}

        {status === "success" ? (
          <div className="flex items-center gap-2 rounded-xl border border-emerald-200 bg-emerald-50 px-4 py-2.5 text-sm text-emerald-700 dark:border-emerald-900/50 dark:bg-emerald-950/30 dark:text-emerald-400">
            <CheckCircle2 className="size-4 shrink-0" />
            {t("accounts.testAutoReset")}
          </div>
        ) : null}
      </div>
    </Modal>
  );
}

function GroupChips({ account, groups }: { account: AccountRow; groups: AccountGroup[] }) {
  const selected = (account.group_ids ?? [])
    .map((id) => groups.find((group) => group.id === id))
    .filter((group): group is AccountGroup => Boolean(group));
  if (selected.length === 0) return <span className="text-xs text-muted-foreground">-</span>;
  return (
    <div className="flex max-w-52 flex-wrap gap-1">
      {selected.slice(0, 3).map((group) => (
        <span
          key={group.id}
          className="inline-flex max-w-36 items-center gap-1 rounded-md px-1.5 py-0.5 text-[10px] font-semibold"
          style={{ color: group.color || "#2563eb", backgroundColor: `${group.color || "#2563eb"}14` }}
          title={group.description || group.name}
        >
          <span className="size-1.5 shrink-0 rounded-full bg-current" />
          <span className="truncate">{group.name}</span>
        </span>
      ))}
      {selected.length > 3 ? <span className="text-[10px] text-muted-foreground">+{selected.length - 3}</span> : null}
    </div>
  );
}

// 模型列：Trae CN 账号的模型目录动辄几十个（截图里是 44 个），单元格里只放前几个
// 可读的，剩下的通过「全部 N」打开完整列表——以前只有一个 "+41" 的纯文本，压根看不出
// 到底有哪些模型。
const MODEL_CELL_LIMIT = 3;

function ModelCatalogChip({ model, tone = "solid" }: { model: string; tone?: "solid" | "muted" }) {
  return (
    <span
      title={model}
      className={cn(
        "inline-flex max-w-[11rem] items-center truncate rounded-md border px-1.5 py-0.5 font-mono text-[10px] leading-4",
        tone === "solid"
          ? "border-border/70 bg-muted/60 text-foreground/85"
          : "border-dashed border-border/70 bg-transparent text-muted-foreground",
      )}
    >
      {model}
    </span>
  );
}

function TraeCNModelsCell({
  account,
  catalog,
  onOpen,
}: {
  account: AccountRow;
  catalog: string[];
  onOpen: () => void;
}) {
  const { t } = useTranslation();
  const own = (account.models ?? []).filter(Boolean);
  const inherited = own.length === 0;
  const resolved = inherited ? catalog.filter(Boolean) : own;
  if (resolved.length === 0) {
    return <span className="text-xs text-muted-foreground">{t("common.noData")}</span>;
  }
  return (
    <div className="flex max-w-[19rem] flex-col gap-1">
      <div className="flex flex-wrap items-center gap-1">
        {inherited ? (
          <span
            title={t("traecn.modelsInheritedHint")}
            className="inline-flex items-center gap-1 rounded-md bg-primary/10 px-1.5 py-0.5 text-[10px] font-semibold text-primary"
          >
            <Layers className="size-3" />
            {t("traecn.modelsInherited")}
          </span>
        ) : null}
        {resolved.slice(0, MODEL_CELL_LIMIT).map((model) => (
          <ModelCatalogChip key={model} model={model} tone={inherited ? "muted" : "solid"} />
        ))}
      </div>
      <button
        type="button"
        onClick={onOpen}
        title={t("traecn.modelsViewAllHint")}
        className="group inline-flex w-fit items-center gap-1 rounded-md border border-border/70 bg-background px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground transition-colors hover:border-primary/40 hover:bg-primary/5 hover:text-primary"
      >
        {resolved.length > MODEL_CELL_LIMIT
          ? t("traecn.modelsViewAll", { count: resolved.length })
          : t("traecn.modelsViewList")}
        <ChevronDown className="size-3 transition-transform group-hover:translate-y-px" />
      </button>
    </div>
  );
}

function TraeCNModelsModal({
  account,
  catalog,
  onClose,
}: {
  account: AccountRow | null;
  catalog: string[];
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const [query, setQuery] = useState("");
  useEffect(() => setQuery(""), [account?.id]);
  const own = (account?.models ?? []).filter(Boolean);
  const inherited = own.length === 0;
  const resolved = useMemo(() => {
    const source = inherited ? catalog : own;
    const unique = Array.from(new Set(source.filter(Boolean)));
    return inherited ? unique : unique.sort((a, b) => a.localeCompare(b));
  }, [catalog, own, inherited]);
  const keyword = query.trim().toLowerCase();
  const filtered = useMemo(
    () => (keyword ? resolved.filter((model) => model.toLowerCase().includes(keyword)) : resolved),
    [keyword, resolved],
  );
  if (!account) return null;
  const copy = async (value: string) => {
    try {
      await navigator.clipboard.writeText(value);
      showToast(t("common.copied"), "success");
    } catch {
      showToast(t("common.copyFailed"), "error");
    }
  };
  return (
    <Modal
      show
      title={
        <span className="flex items-center gap-2">
          {t("traecn.modelsModalTitle")}
          <Badge variant="secondary" className="font-mono text-[10px]">{resolved.length}</Badge>
        </span>
      }
      onClose={onClose}
      contentClassName="sm:max-w-[720px]"
      footer={
        <>
          <Button variant="outline" onClick={onClose}>{t("common.close")}</Button>
          <Button onClick={() => void copy(resolved.join("\n"))} disabled={resolved.length === 0}>
            <Copy className="size-4" />
            {t("traecn.modelsCopyAll")}
          </Button>
        </>
      }
    >
      <div className="space-y-3">
        <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
          <span className="truncate font-semibold text-foreground">{accountLabel(account)}</span>
          <span
            className={cn(
              "inline-flex items-center gap-1 rounded-md px-1.5 py-0.5 text-[10px] font-semibold",
              inherited ? "bg-primary/10 text-primary" : "bg-muted text-foreground/80",
            )}
          >
            {inherited ? <Layers className="size-3" /> : null}
            {inherited ? t("traecn.modelsInherited") : t("traecn.modelsOwn")}
          </span>
          <span className="ml-auto font-mono text-[11px]">
            {t("traecn.modelsShowing", { shown: filtered.length, total: resolved.length })}
          </span>
        </div>
        <div className="relative">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
          <Input
            autoFocus
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder={t("traecn.modelsSearchPlaceholder")}
            className="pl-8"
          />
        </div>
        {filtered.length === 0 ? (
          <div className="rounded-lg border border-dashed border-border px-3 py-6 text-center text-xs text-muted-foreground">
            {t("traecn.modelsNoMatch")}
          </div>
        ) : (
          <div className="flex max-h-[46vh] flex-wrap gap-1.5 overflow-y-auto rounded-lg border border-border/70 bg-muted/20 p-2">
            {filtered.map((model) => (
              <button
                key={model}
                type="button"
                onClick={() => void copy(model)}
                title={t("traecn.modelsCopyOne", { model })}
                className="group inline-flex max-w-full items-center gap-1 rounded-md border border-border/70 bg-background px-2 py-1 font-mono text-[11px] text-foreground/85 transition-colors hover:border-primary/40 hover:bg-primary/5 hover:text-primary"
              >
                <ModelLogo model={model} size={16} className="shrink-0" />
                <span className="truncate">{model}</span>
                <Copy className="size-3 shrink-0 opacity-0 transition-opacity group-hover:opacity-60" />
              </button>
            ))}
          </div>
        )}
      </div>
    </Modal>
  );
}

function AccountActions({
  account,
  busy,
  onTest,
  onRefresh,
  onSyncModels,
  onCheckin,
  onEdit,
  onToggle,
  onDelete,
}: {
  account: AccountRow;
  busy: string | null;
  onTest: () => void;
  onRefresh: () => void;
  onSyncModels: () => void;
  onCheckin: () => void;
  onEdit: () => void;
  onToggle: () => void;
  onDelete: () => void;
}) {
  const { t } = useTranslation();
  const isBusy = Boolean(busy);
  return (
    <div className="flex items-center justify-end gap-1">
      <Button variant="ghost" size="icon-xs" title={t("accounts.testConnection")} aria-label={t("accounts.testConnection")} disabled={isBusy} onClick={onTest}>
        <Zap className={cn("size-3.5", busy === "test" && "animate-pulse")} />
      </Button>
      <Button variant="ghost" size="icon-xs" title={t("traecn.refreshAccount")} disabled={isBusy} onClick={onRefresh}>
        <RefreshCw className={cn("size-3.5", busy === "refresh" && "animate-spin")} />
      </Button>
      <Button variant="ghost" size="icon-xs" title={t("traecn.checkin")} aria-label={t("traecn.checkin")} disabled={isBusy} onClick={onCheckin}>
        <CalendarCheck className={cn("size-3.5", busy === "checkin" && "animate-pulse")} />
      </Button>
      <Button variant="ghost" size="icon-xs" title={t("traecn.syncModels")} aria-label={t("traecn.syncModels")} disabled={isBusy} onClick={onSyncModels}>
        <ListRestart className={cn("size-3.5", busy === "syncModels" && "animate-spin")} />
      </Button>
      <Button variant="ghost" size="icon-xs" title={t("traecn.editAccount")} disabled={isBusy} onClick={onEdit}>
        <Edit3 className="size-3.5" />
      </Button>
      <Button variant="ghost" size="icon-xs" title={account.enabled === false ? t("traecn.enableAccount") : t("traecn.disableAccount")} disabled={isBusy} onClick={onToggle}>
        {account.enabled === false ? <Power className="size-3.5" /> : <PowerOff className="size-3.5" />}
      </Button>
      <Button variant="ghost" size="icon-xs" title={t("traecn.deleteAccount")} disabled={isBusy} onClick={onDelete}>
        <Trash2 className="size-3.5 text-destructive" />
      </Button>
    </div>
  );
}

export default function TraeCNAccounts({ headerSlot }: { headerSlot?: ReactNode } = {}) {
  const { t } = useTranslation();
  const { showToast } = useToast();
  const { confirm, confirmDialog } = useConfirmDialog();
  const requestAbortRef = useRef<AbortController | null>(null);
  const [accounts, setAccounts] = useState<AccountRow[]>([]);
  const [groups, setGroups] = useState<AccountGroup[]>([]);
  const traeGroups = useMemo(() => groups.filter((group) => group.channel === "traecn"), [groups]);
  const [models, setModels] = useState<string[]>(DEFAULT_TRAE_MODELS);
  const [traecnTestModel, setTraeCNTestModel] = useState("auto");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [total, setTotal] = useState(0);
  const [summary, setSummary] = useState<{ active: number; disabled: number; error: number } | null>(null);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = usePersistedPageSize("traecn-accounts", 20, DEFAULT_PAGE_SIZE_OPTIONS);
  const [search, setSearch] = useState("");
  const [debouncedSearch, setDebouncedSearch] = useState("");
  const [status, setStatus] = useState<StatusFilter>("all");
  const [busy, setBusy] = useState<{ id: number; action: string } | null>(null);
  const [testingAccount, setTestingAccount] = useState<AccountRow | null>(null);
  const [modelsAccount, setModelsAccount] = useState<AccountRow | null>(null);
  const [showAdd, setShowAdd] = useState(false);
  const [addForm, setAddForm] = useState({ name: "", refreshTokens: "", host: DEFAULT_HOST, proxyURL: "", groupIDs: [] as number[], enabled: true });
  const [addResult, setAddResult] = useState<AddTraeCNAccountsResponse | null>(null);
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<AccountRow | null>(null);
  const [editForm, setEditForm] = useState({ name: "", host: DEFAULT_HOST, proxyURL: "", groupIDs: [] as number[] });
  const [saving, setSaving] = useState(false);

  const reloadGroups = useCallback(async () => {
    try {
      const response = await api.listAccountGroups();
      setGroups(response.groups ?? []);
    } catch {
      // Group filtering is optional; account loading should remain usable.
    }
  }, []);

  const reload = useCallback(async (silent = false) => {
    requestAbortRef.current?.abort();
    const controller = new AbortController();
    requestAbortRef.current = controller;
    if (!silent) setLoading(true);
    try {
      const response = await api.getAccountsPage({
        channel: "traecn",
        page,
        pageSize,
        search: debouncedSearch,
        status,
        sort: "updated_at",
        order: "desc",
      }, controller.signal);
      if (controller.signal.aborted) return;
      setAccounts((response.accounts ?? []).filter((account) => account.traecn_api !== false));
      setTotal(response.total ?? 0);
      setSummary({
        active: response.summary?.active ?? 0,
        disabled: response.summary?.disabled ?? 0,
        error: response.summary?.error ?? 0,
      });
      if (response.page !== page) setPage(response.page);
      setError(null);
    } catch (loadError) {
      if (controller.signal.aborted) return;
      const message = getErrorMessage(loadError);
      setError(message);
      showToast(message, "error");
    } finally {
      if (requestAbortRef.current === controller) setLoading(false);
    }
  }, [debouncedSearch, page, pageSize, showToast, status]);

  useEffect(() => { void reloadGroups(); }, [reloadGroups]);
  useEffect(() => { void reload(); }, [reload]);
  useEffect(() => () => requestAbortRef.current?.abort(), []);
  useEffect(() => {
    const timer = window.setTimeout(() => { setDebouncedSearch(search.trim()); setPage(1); }, 250);
    return () => window.clearTimeout(timer);
  }, [search]);
  useEffect(() => { setPage(1); }, [status]);
  useEffect(() => {
    void api.getModels().then((response) => {
      if (response.traecn_models?.length) setModels(response.traecn_models);
    }).catch(() => undefined);
  }, []);
  useEffect(() => {
    void api.getSettings().then((response) => {
      const configuredModel = response.traecn_test_model?.trim();
      if (configuredModel) setTraeCNTestModel(configuredModel);
    }).catch(() => undefined);
  }, []);

  const openAdd = () => {
    setAddResult(null);
    setAddForm({ name: "", refreshTokens: "", host: DEFAULT_HOST, proxyURL: "", groupIDs: [], enabled: true });
    setShowAdd(true);
  };

  const submitAdd = async () => {
    const tokens = parseLines(addForm.refreshTokens);
    if (tokens.length === 0) { showToast(t("traecn.refreshTokenRequired"), "error"); return; }
    setAdding(true);
    try {
      const result = await api.addTraeCNAccounts({
        name: addForm.name.trim() || undefined,
        refresh_tokens: tokens,
        host: addForm.host.trim() || DEFAULT_HOST,
        proxy_url: addForm.proxyURL.trim(),
        group_ids: addForm.groupIDs,
        enabled: addForm.enabled,
      });
      setAddResult(result);
      showToast(t("traecn.importFinished", { success: result.success, failed: result.failed }), result.failed ? "warning" : "success");
      await reload(true);
    } catch (submitError) {
      showToast(getErrorMessage(submitError), "error");
    } finally { setAdding(false); }
  };

  const openEdit = (account: AccountRow) => {
    setEditing(account);
    setEditForm({
      name: account.name ?? "",
      host: account.traecn_host || DEFAULT_HOST,
      proxyURL: account.proxy_url ?? "",
      groupIDs: account.group_ids ?? [],
    });
  };

  const submitEdit = async () => {
    if (!editing) return;
    setSaving(true);
    try {
      await api.updateTraeCNAccount(editing.id, {
        name: editForm.name.trim(),
        host: editForm.host.trim() || DEFAULT_HOST,
        proxy_url: editForm.proxyURL.trim(),
        group_ids: editForm.groupIDs,
      });
      showToast(t("traecn.editSuccess"), "success");
      setEditing(null);
      await reload(true);
    } catch (saveError) { showToast(getErrorMessage(saveError), "error"); }
    finally { setSaving(false); }
  };

  const runAccountAction = async (account: AccountRow, action: "refresh" | "syncModels" | "toggle" | "delete" | "checkin") => {
    if (action === "delete") {
      const ok = await confirm({ title: t("traecn.deleteTitle"), description: t("traecn.deleteDescription", { account: accountLabel(account) }), tone: "destructive", confirmVariant: "destructive", confirmText: t("common.confirm") });
      if (!ok) return;
    }
    setBusy({ id: account.id, action });
    try {
      if (action === "refresh") await api.refreshTraeCNAccount(account.id);
      else if (action === "syncModels") await api.syncAccountModelsUpstream(account.id);
      else if (action === "checkin") {
        const result = await api.checkinTraeCNAccount(account.id);
        showToast(result.message || t("traecn.checkinSuccess"), "success");
        await reload(true);
        return;
      }
      else if (action === "toggle") await api.toggleAccountEnabled(account.id, account.enabled === false);
      else await api.deleteAccount(account.id);
      showToast(t(`traecn.${action === "refresh" ? "refreshSuccess" : action === "syncModels" ? "syncModelsSuccess" : action === "toggle" ? (account.enabled === false ? "enableSuccess" : "disableSuccess") : "deleteSuccess"}`), "success");
      await reload(true);
    } catch (actionError) { showToast(getErrorMessage(actionError), "error"); }
    finally { setBusy(null); }
  };

  const totalPages = Math.max(1, Math.ceil(total / pageSize));
  const activeCount = summary?.active ?? accounts.filter((account) => account.enabled !== false && account.status !== "error").length;
  const disabledCount = summary?.disabled ?? accounts.filter((account) => account.enabled === false).length;
  const errorCount = summary?.error ?? accounts.filter((account) => account.status === "error").length;

  return (
    <div className="space-y-4">
      <PageHeader
        title={t("traecn.pageTitle")}
        description={t("traecn.pageDescription")}
        titleAdornment={headerSlot}
        onRefresh={() => void reload()}
        actions={<Button onClick={openAdd}><Plus className="size-4" />{t("traecn.addAccount")}</Button>}
      />

      <div className="grid grid-cols-2 gap-2.5 sm:grid-cols-4">
        <CompactStat label={t("traecn.statTotal")} value={total} tone="neutral" />
        <CompactStat label={t("traecn.statActive")} value={activeCount} tone="success" active={status === "active"} onClick={() => setStatus(status === "active" ? "all" : "active")} />
        <CompactStat label={t("traecn.statDisabled")} value={disabledCount} tone="warning" active={status === "disabled"} onClick={() => setStatus(status === "disabled" ? "all" : "disabled")} />
        <CompactStat label={t("traecn.statError")} value={errorCount} tone="danger" active={status === "error"} onClick={() => setStatus(status === "error" ? "all" : "error")} />
      </div>

      <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between">
        <div className="relative min-w-0 flex-1 sm:max-w-sm">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
          <Input className="pl-9" value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t("traecn.searchPlaceholder")} />
        </div>
        <Select value={status} onValueChange={(value) => setStatus(value as StatusFilter)} options={[
          { value: "all", label: t("traecn.filterAll") },
          { value: "active", label: t("traecn.filterActive") },
          { value: "disabled", label: t("traecn.filterDisabled") },
          { value: "error", label: t("traecn.filterError") },
        ]} className="sm:w-36" compact />
      </div>

      <StateShell
        variant="section"
        loading={loading}
        error={error}
        onRetry={() => void reload()}
        loadingTitle={t("traecn.loadingTitle")}
        loadingDescription={t("traecn.loadingDescription")}
        errorTitle={t("traecn.errorTitle")}
        isEmpty={!loading && accounts.length === 0}
        emptyTitle={total === 0 ? t("traecn.emptyTitle") : t("traecn.noMatchesTitle")}
        emptyDescription={total === 0 ? t("traecn.emptyDescription") : t("traecn.noMatchesDescription")}
      >
        <div className="w-full overflow-hidden rounded-xl border border-border bg-card shadow-sm">
          <div className="overflow-x-auto">
            <table className="w-full min-w-[860px] text-sm">
              <thead className="border-b border-border bg-muted/30">
                <tr className="text-left text-xs font-semibold uppercase text-muted-foreground">
                  <th className="px-3 py-3">{t("traecn.columnAccount")}</th>
                  <th className="px-3 py-3">{t("traecn.columnModels")}</th>
                  <th className="px-3 py-3">{t("traecn.columnEndpoint")}</th>
                  <th className="px-3 py-3">{t("traecn.columnGroups")}</th>
                  <th className="px-3 py-3">{t("traecn.columnStatus")}</th>
                  <th className="px-3 py-3 text-right">{t("traecn.columnActions")}</th>
                </tr>
              </thead>
              <tbody>
                {accounts.map((account) => {
                  const accountBusy = busy?.id === account.id
                    ? busy.action
                    : testingAccount?.id === account.id
                      ? "test"
                      : null;
                  return (
                    <tr key={account.id} className={cn("border-b border-border/70 last:border-0", account.enabled === false && "opacity-60")}>
                      <td className="max-w-[220px] px-3 py-3">
                        <div className="truncate font-semibold" title={accountLabel(account)}>{accountLabel(account)}</div>
                        <div
                          className="mt-0.5 truncate font-mono text-[11px] text-muted-foreground"
                          title={account.email || `ID ${account.id}`}
                        >
                          {account.email || `ID ${account.id}`}
                        </div>
                        {account.traecn_checkin_date ? (
                          <div className="mt-0.5 truncate text-[10px] text-muted-foreground" title={account.traecn_checkin_result || ""}>
                            {t("traecn.checkinLast", { date: account.traecn_checkin_date, credits: account.traecn_checkin_credits ?? 0 })}
                          </div>
                        ) : null}
                      </td>
                      <td className="px-3 py-3 align-top">
                        <TraeCNModelsCell account={account} catalog={models} onOpen={() => setModelsAccount(account)} />
                      </td>
                      <td className="max-w-[240px] px-3 py-3">
                        <div className="truncate font-mono text-xs" title={account.traecn_host || DEFAULT_HOST}>{account.traecn_host || DEFAULT_HOST}</div>
                        {account.proxy_url ? <div className="mt-0.5 truncate text-[11px] text-muted-foreground" title={account.proxy_url}>{account.proxy_url}</div> : <div className="mt-0.5 text-[11px] text-muted-foreground">{t("traecn.noProxy")}</div>}
                      </td>
                      <td className="px-3 py-3"><GroupChips account={account} groups={traeGroups} /></td>
                      <td className="px-3 py-3"><div className="flex items-center gap-1.5"><StatusBadge status={account.status} />{account.enabled === false ? <Badge variant="outline">{t("traecn.disabledBadge")}</Badge> : null}</div></td>
                      <td className="px-3 py-3"><AccountActions account={account} busy={accountBusy} onTest={() => setTestingAccount(account)} onRefresh={() => void runAccountAction(account, "refresh")} onSyncModels={() => void runAccountAction(account, "syncModels")} onCheckin={() => void runAccountAction(account, "checkin")} onEdit={() => openEdit(account)} onToggle={() => void runAccountAction(account, "toggle")} onDelete={() => void runAccountAction(account, "delete")} /></td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          <div className="px-3 pb-3"><Pagination page={page} totalPages={totalPages} onPageChange={setPage} totalItems={total} pageSize={pageSize} pageSizeOptions={DEFAULT_PAGE_SIZE_OPTIONS} onPageSizeChange={(next) => { setPageSize(next); setPage(1); }} /></div>
        </div>
      </StateShell>

      <Modal show={showAdd} title={t("traecn.addTitle")} onClose={() => { if (!adding) setShowAdd(false); }} contentClassName="sm:max-w-[640px]" footer={<><Button variant="outline" onClick={() => setShowAdd(false)} disabled={adding}>{t("common.cancel")}</Button><Button onClick={() => void submitAdd()} disabled={adding}>{adding ? <Loader2 className="size-4 animate-spin" /> : <Plus className="size-4" />}{adding ? t("traecn.adding") : t("traecn.submit")}</Button></>}>
        <div className="space-y-4">
          <div className="rounded-lg border border-primary/20 bg-primary/5 px-3 py-2 text-xs leading-relaxed text-muted-foreground">{t("traecn.refreshTokenHint")}</div>
          <label className="block space-y-1.5"><span className="text-xs font-semibold text-muted-foreground">{t("traecn.refreshTokensLabel")} *</span><textarea value={addForm.refreshTokens} onChange={(event) => setAddForm((form) => ({ ...form, refreshTokens: event.target.value }))} placeholder={t("traecn.refreshTokensPlaceholder")} className="min-h-36 w-full resize-y rounded-md border border-input bg-transparent px-3 py-2 font-mono text-xs outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50" /></label>
          <div className="grid gap-3 sm:grid-cols-2">
            <label className="block space-y-1.5"><span className="text-xs font-semibold text-muted-foreground">{t("traecn.nameLabel")}</span><Input value={addForm.name} onChange={(event) => setAddForm((form) => ({ ...form, name: event.target.value }))} placeholder={t("traecn.namePlaceholder")} /></label>
            <label className="block space-y-1.5"><span className="text-xs font-semibold text-muted-foreground">{t("traecn.hostLabel")}</span><Input value={addForm.host} onChange={(event) => setAddForm((form) => ({ ...form, host: event.target.value }))} placeholder={DEFAULT_HOST} /></label>
          </div>
          <label className="block space-y-1.5"><span className="text-xs font-semibold text-muted-foreground">{t("traecn.proxyLabel")}</span><Input value={addForm.proxyURL} onChange={(event) => setAddForm((form) => ({ ...form, proxyURL: event.target.value }))} placeholder="http://127.0.0.1:7890" /></label>
          <div className="rounded-lg border border-border bg-muted/25 px-3 py-2 text-xs leading-relaxed text-muted-foreground">{t("traecn.modelsFromUpstreamHint")}</div>
          <div className="space-y-1.5"><span className="text-xs font-semibold text-muted-foreground">{t("accounts.importGroupsLabel")}</span><AccountGroupMultiSelect groups={traeGroups} value={addForm.groupIDs} onChange={(value) => setAddForm((form) => ({ ...form, groupIDs: value }))} placeholder={t("accounts.importGroupsPlaceholder")} emptyLabel={t("accounts.groupsNone")} selectedLabel={t("accounts.groupsSelected", { count: addForm.groupIDs.length })} /></div>
          <label className="flex items-center gap-2 text-sm"><input type="checkbox" checked={addForm.enabled} onChange={(event) => setAddForm((form) => ({ ...form, enabled: event.target.checked }))} className="size-4 accent-primary" />{t("traecn.enableOnImport")}</label>
          {addResult ? <ImportResult result={addResult} /> : null}
        </div>
      </Modal>

      <Modal show={Boolean(editing)} title={t("traecn.editTitle")} onClose={() => { if (!saving) setEditing(null); }} contentClassName="sm:max-w-[560px]" footer={<><Button variant="outline" onClick={() => setEditing(null)} disabled={saving}>{t("common.cancel")}</Button><Button onClick={() => void submitEdit()} disabled={saving}>{saving ? <Loader2 className="size-4 animate-spin" /> : null}{saving ? t("common.saving") : t("common.save")}</Button></>}>
        <div className="space-y-4">
          <label className="block space-y-1.5"><span className="text-xs font-semibold text-muted-foreground">{t("traecn.nameLabel")}</span><Input value={editForm.name} onChange={(event) => setEditForm((form) => ({ ...form, name: event.target.value }))} /></label>
          <label className="block space-y-1.5"><span className="text-xs font-semibold text-muted-foreground">{t("traecn.hostLabel")}</span><Input value={editForm.host} onChange={(event) => setEditForm((form) => ({ ...form, host: event.target.value }))} /></label>
          <label className="block space-y-1.5"><span className="text-xs font-semibold text-muted-foreground">{t("traecn.proxyLabel")}</span><Input value={editForm.proxyURL} onChange={(event) => setEditForm((form) => ({ ...form, proxyURL: event.target.value }))} /></label>
          <div className="rounded-lg border border-border bg-muted/25 px-3 py-2 text-xs leading-relaxed text-muted-foreground">{t("traecn.modelsFromUpstreamHint")}</div>
          <div className="space-y-1.5"><span className="text-xs font-semibold text-muted-foreground">{t("accounts.groupsLabel")}</span><AccountGroupMultiSelect groups={traeGroups} value={editForm.groupIDs} onChange={(value) => setEditForm((form) => ({ ...form, groupIDs: value }))} placeholder={t("accounts.groupsPlaceholder")} emptyLabel={t("accounts.groupsNone")} selectedLabel={t("accounts.groupsSelected", { count: editForm.groupIDs.length })} /></div>
        </div>
      </Modal>
      {testingAccount ? (
        <TraeCNTestConnectionModal
          account={testingAccount}
          availableModels={testingAccount.models?.length ? testingAccount.models : models}
          preferredModel={traecnTestModel}
          onSettled={() => void reload(true)}
          onClose={() => setTestingAccount(null)}
        />
      ) : null}
      <TraeCNModelsModal account={modelsAccount} catalog={models} onClose={() => setModelsAccount(null)} />
      {confirmDialog}
    </div>
  );
}
