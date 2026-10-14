"use client";

import * as React from "react";

import { FileOutputIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { api } from "@/lib/api";

const maxLimitMiB = 8796093022207;

export function ChatFileSettingsCard({
  limit,
  loaded,
  onSaved,
}: {
  limit: number | undefined;
  loaded: boolean;
  onSaved: (limit: number) => void;
}) {
  const [input, setInput] = React.useState("");
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    if (limit !== undefined) setInput(String(limit));
  }, [limit]);

  async function save(event: React.FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const value = Number(input);
    if (!input.trim() || !Number.isSafeInteger(value) || value < 0 || value > maxLimitMiB) {
      toast.error("请输入有效的非负整数，0 表示不限制大小");
      return;
    }
    setSaving(true);
    try {
      const result = await api.setSettings({ chat_file_max_mib: value });
      if (typeof result.chat_file_max_mib !== "number") throw new Error("未收到已保存的文件大小配置");
      onSaved(result.chat_file_max_mib);
      setInput(String(result.chat_file_max_mib));
      toast.success("已保存文件大小上限，对后续发送生效");
    } catch (error) {
      toast.error(`保存失败：${error instanceof Error ? error.message : "请重试"}`);
    } finally {
      setSaving(false);
    }
  }

  return (
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <FileOutputIcon className="size-4" />
          Agent 文件发送
        </CardTitle>
        <CardDescription id="chat-file-limit-description">
          设置 Agent 发到聊天中的单个文件大小上限。默认 128 MiB，0 表示不限制大小。
          保存后对下一次发送生效，无需重启；已发送的附件仍可下载。
        </CardDescription>
      </CardHeader>
      <CardContent>
        <form className="flex flex-col gap-2" onSubmit={save}>
          <Label htmlFor="chat-file-max-mib">最大文件大小（MiB）</Label>
          <div className="flex items-center gap-2">
            <Input
              id="chat-file-max-mib"
              type="number"
              min={0}
              max={maxLimitMiB}
              step={1}
              required
              value={input}
              disabled={!loaded || saving}
              onChange={(event) => setInput(event.target.value)}
              aria-describedby="chat-file-limit-description chat-file-limit-status"
            />
            <Button type="submit" disabled={!loaded || saving}>
              {saving ? "保存中…" : "保存"}
            </Button>
          </div>
          <p id="chat-file-limit-status" className="text-muted-foreground text-xs" aria-live="polite">
            {limit !== undefined && (limit === 0 ? "当前：不限制大小" : `当前：${limit} MiB`)}
            {limit === undefined && (loaded ? "未能读取当前值，可刷新重试或输入新上限保存" : "正在读取配置…")}
          </p>
        </form>
      </CardContent>
    </Card>
  );
}
