"use client";

import * as React from "react";

import { CheckCircle2Icon, DownloadIcon, ExternalLinkIcon, RefreshCwIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { api } from "@/lib/api";
import type { UpdateCheck } from "@/lib/types";

const SOURCE_URL = "https://github.com/zhyx111999/YETRX";

export function UpdateCard() {
  const [info, setInfo] = React.useState<UpdateCheck | null>(null);
  const [checking, setChecking] = React.useState(true);
  const [error, setError] = React.useState("");

  const check = React.useCallback(async (force = false) => {
    setChecking(true);
    setError("");
    try {
      const result = await api.checkUpdate(force);
      setInfo(result);
      setError(result.error ?? "");
      if (force && !result.error) {
        if (result.has_update) toast.success(`发现新版本 ${result.latest}`);
        else if (result.comparable) toast.success("当前已是最新版本");
      }
    } catch (cause) {
      setError((cause as Error).message);
    } finally {
      setChecking(false);
    }
  }, []);

  React.useEffect(() => {
    void check();
  }, [check]);

  return (
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <DownloadIcon className="size-4" />
          版本与更新
        </CardTitle>
        <CardDescription>从 YETRX 源码仓库检查新版本。更新时一起部署程序和 Agent 运行时。</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">当前版本</span>
          <Badge variant="secondary" className="font-mono">{info?.current ?? "…"}</Badge>
          {info && <Badge variant="outline">{info.os}/{info.arch}</Badge>}
          {info?.latest && (
            <>
              <span className="text-muted-foreground">最新版本</span>
              <Badge variant={info.has_update ? "default" : "secondary"}>{info.latest}</Badge>
            </>
          )}
        </div>
        {error && <p className="text-xs text-destructive">版本检查未完成：{error}</p>}
        {info?.boot_notice && <p className="text-xs text-muted-foreground">{info.boot_notice}</p>}
        {info?.comparable && !info.has_update && !error && (
          <p className="flex items-center gap-2 text-xs text-muted-foreground">
            <CheckCircle2Icon className="size-3.5 text-emerald-600" />当前已是最新版本。
          </p>
        )}
        <p className="text-xs text-muted-foreground">按仓库中的部署说明更新，并保留现有数据、数据库和登录配置。</p>
        <div className="flex flex-wrap items-center gap-3">
          <Button variant="outline" size="sm" onClick={() => void check(true)} disabled={checking}>
            <RefreshCwIcon className={checking ? "size-4 animate-spin" : "size-4"} />检查更新
          </Button>
          <a href={SOURCE_URL} target="_blank" rel="noreferrer" className="inline-flex items-center gap-1 text-sm text-primary underline-offset-4 hover:underline">
            源码与部署说明 <ExternalLinkIcon className="size-3" />
          </a>
          {info?.html_url && (
            <a href={info.html_url} target="_blank" rel="noreferrer" className="text-sm text-primary underline-offset-4 hover:underline">版本详情</a>
          )}
        </div>
      </CardContent>
    </Card>
  );
}
