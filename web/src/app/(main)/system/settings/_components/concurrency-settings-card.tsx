"use client";

import * as React from "react";
import { LayersIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";
import type { Settings } from "@/lib/types";

export function ConcurrencySettingsCard({ settings, loaded, onSaved }: {
  settings: Settings | null;
  loaded: boolean;
  onSaved: (settings: Settings) => void;
}) {
  const [helpers, setHelpers] = React.useState("");
  const [subagents, setSubagents] = React.useState("");
  const [saving, setSaving] = React.useState(false);
  const ready = settings?.resource_max_workers !== undefined && settings?.subagent_concurrency !== undefined;

  React.useEffect(() => {
    if (settings?.resource_max_workers !== undefined) setHelpers(String(settings.resource_max_workers));
    if (settings?.subagent_concurrency !== undefined) setSubagents(String(settings.subagent_concurrency));
  }, [settings?.resource_max_workers, settings?.subagent_concurrency]);

  async function save(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const helperCount = Number(helpers);
    const subagentCount = Number(subagents);
    if ([helperCount, subagentCount].some((value) => !Number.isSafeInteger(value) || value < 1 || value > 1024)) {
      toast.error("并发数必须是 1 到 1024 的整数");
      return;
    }
    setSaving(true);
    try {
      const result = await api.setSettings({ resource_max_workers: helperCount, subagent_concurrency: subagentCount });
      if (result.resource_max_workers !== helperCount || result.subagent_concurrency !== subagentCount) {
        throw new Error("未收到已保存的并发配置");
      }
      onSaved(result);
      toast.success("并发配置已保存，正在运行的工作不会被强停");
    } catch (error) {
      toast.error(`保存失败：${error instanceof Error ? error.message : "请重试"}`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base"><LayersIcon className="size-4" />执行并发</CardTitle>
        <CardDescription>
          不再设置所有 Agent 会话共用的数量上限。下列两项独立管理，仍受 CPU、内存和进程数预算约束。
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form onSubmit={save} className="flex flex-col gap-4">
          <div className="space-y-2">
            <Label htmlFor="resource-max-workers">资源 helper 并发</Label>
            <Input id="resource-max-workers" type="number" min={1} max={1024} step={1} required value={helpers}
              disabled={!ready || saving} onChange={(event) => setHelpers(event.target.value)} aria-describedby="helper-concurrency-description" />
            <p id="helper-concurrency-description" className="text-muted-foreground text-xs">
              默认 64，供 MCP 连接和管理页工具测试共享，不是工具数量。保存后对新 helper 准入即时生效；调低时等待已有进程退出，不强制断开连接。
            </p>
          </div>
          <div className="space-y-2">
            <Label htmlFor="subagent-concurrency">每父对话子代理并发</Label>
            <Input id="subagent-concurrency" type="number" min={1} max={1024} step={1} required value={subagents}
              disabled={!ready || saving} onChange={(event) => setSubagents(event.target.value)} aria-describedby="subagent-concurrency-description" />
            <p id="subagent-concurrency-description" className="text-muted-foreground text-xs">
              默认 16。同一父对话的多批子代理共用，不同父对话各自独立。保存后即时调整排队准入；调低不强停正在执行的子代理。
            </p>
          </div>
          {settings?.resource_execution_enabled === false && (
            <p className="text-amber-600 text-xs dark:text-amber-400">当前通过环境配置关闭了进程隔离；helper 配置仍可保存，在重新启用隔离后使用。</p>
          )}
          <p className="text-muted-foreground text-xs">已保存值优先；未保存时 helper 沿用环境变量或默认值。探索 Worker 使用原有「子任务执行并发（探索）」配置。</p>
          {!ready && <p className="text-muted-foreground text-xs">{loaded ? "未能读取执行配置，请刷新重试" : "正在读取配置…"}</p>}
          <Button type="submit" disabled={!ready || saving} className="self-start">{saving ? "保存中…" : "保存并发配置"}</Button>
        </form>
      </CardContent>
    </Card>
  );
}
