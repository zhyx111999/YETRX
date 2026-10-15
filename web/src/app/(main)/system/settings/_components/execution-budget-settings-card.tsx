"use client";

import * as React from "react";
import { ShieldIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { ExecutionBudget, ProcessBudget, Settings } from "@/lib/types";

type BudgetInput = Record<keyof ExecutionBudget, Record<keyof ProcessBudget, string>>;
const groups = [
  { key: "agent", label: "单个 Agent 执行树", defaults: "默认 512 MiB / 1 CPU / 128 进程" },
  { key: "helper", label: "单个资源 helper 执行树", defaults: "默认 384 MiB / 1 CPU / 128 进程" },
  { key: "total", label: "全部执行进程总预算", defaults: "默认 3072 MiB / 4 CPU / 1024 进程" },
] as const;
const fields = [
  { key: "memory_mib", label: "内存（MiB）", max: 1048576 },
  { key: "cpus", label: "CPU（核）", max: 1024 },
  { key: "processes", label: "进程数", max: 1048576 },
] as const;

function budgetInput(budget?: ExecutionBudget): BudgetInput {
  const groupInput = (group?: ProcessBudget) => ({
    memory_mib: group ? String(group.memory_mib) : "",
    cpus: group ? String(group.cpus) : "",
    processes: group ? String(group.processes) : "",
  });
  return { agent: groupInput(budget?.agent), helper: groupInput(budget?.helper), total: groupInput(budget?.total) };
}

export function ExecutionBudgetSettingsCard({ settings, loaded, onSaved }: {
  settings: Settings | null;
  loaded: boolean;
  onSaved: (settings: Settings) => void;
}) {
  const [input, setInput] = React.useState<BudgetInput>(() => budgetInput());
  const [saving, setSaving] = React.useState(false);
  const budget = settings?.execution_budget;
  const previousBudget = React.useRef<ExecutionBudget | undefined>(undefined);
  const ready = budget !== undefined;

  React.useEffect(() => {
    const previous = previousBudget.current;
    previousBudget.current = budget;
    // Saving another card must not discard an unfinished budget edit.
    if (groups.every((group) => fields.every((field) =>
      previous?.[group.key][field.key] === budget?.[group.key][field.key]))) return;
    setInput(budgetInput(budget));
  }, [budget]);

  async function save(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const values = {} as ExecutionBudget;
    for (const group of groups) {
      const value = {} as ProcessBudget;
      for (const field of fields) {
        const raw = input[group.key][field.key];
        const number = Number(raw);
        if (!raw.trim() || !Number.isFinite(number) || number <= 0 || number > field.max ||
          (field.key !== "cpus" && !Number.isSafeInteger(number))) {
          toast.error(`${group.label}：${field.label}需大于 0 且不超过 ${field.max}${field.key === "cpus" ? "" : "，必须为整数"}`);
          return;
        }
        value[field.key] = number;
      }
      values[group.key] = value;
    }
    setSaving(true);
    try {
      const result = await api.setSettings({ execution_budget: values });
      if (!result.execution_budget) throw new Error("未收到已保存的安全预算");
      onSaved(result);
      setInput(budgetInput(result.execution_budget));
      toast.success(result.execution_budget_restart_required ? "安全预算已保存，重启服务后生效" : "安全预算已保存");
    } catch (error) {
      toast.error(`保存失败：${error instanceof Error ? error.message : "请重试"}`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base"><ShieldIcon className="size-4" />执行安全预算</CardTitle>
        <CardDescription>
          预算有项目默认值，也支持环境变量。此处保存后优先使用系统配置，<b>重启服务后生效</b>；保存不会自动重启或中断现有任务。
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form onSubmit={save} className="flex flex-col gap-4">
          {groups.map((group) => {
            const active = settings?.execution_budget_active?.[group.key];
            return (
              <fieldset key={group.key} className="min-w-0 space-y-2 rounded-lg border p-3" disabled={!ready || saving}>
                <legend className="px-1 text-sm font-medium">{group.label}</legend>
                <p className="text-muted-foreground text-xs">{group.defaults}</p>
                <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
                  {fields.map((field) => {
                    const id = `execution-${group.key}-${field.key}`;
                    return (
                      <div key={field.key} className="min-w-0 space-y-1.5">
                        <Label htmlFor={id}>{field.label}</Label>
                        <Input id={id} type="number" min={field.key === "cpus" ? 0 : 1} max={field.max}
                          step={field.key === "cpus" ? "any" : 1} required value={input[group.key][field.key]}
                          onChange={(event) => setInput((current) => ({ ...current,
                            [group.key]: { ...current[group.key], [field.key]: event.target.value },
                          }))} />
                      </div>
                    );
                  })}
                </div>
                {active && <p className="text-muted-foreground text-xs">当前生效：{active.memory_mib} MiB / {active.cpus} CPU / {active.processes} 进程</p>}
              </fieldset>
            );
          })}
          <p className="text-muted-foreground text-xs">
            单树预算包含其后代；总预算由所有 Agent/helper 共享，不包含网站主进程。CPU 是逻辑核心额度，Linux 进程数也包含线程。
            这些是硬上限，不是预分配资源；调得过低可能导致进程或服务启动失败。
          </p>
          {settings?.resource_execution_enabled === false && <p className="text-amber-600 text-xs dark:text-amber-400">当前进程隔离已关闭，预算未启用；重新启用隔离并重启后应用保存值。</p>}
          <p className="text-xs" aria-live="polite">
            {!ready ? (loaded ? "未能读取安全预算，请刷新重试" : "正在读取配置…") :
              settings?.execution_budget_restart_required ? "存在待生效预算：请在合适时机重启服务。" : "没有待重启应用的预算变更。"}
          </p>
          <Button type="submit" className="self-start" disabled={!ready || saving}>{saving ? "保存中…" : "保存安全预算"}</Button>
        </form>
      </CardContent>
    </Card>
  );
}
