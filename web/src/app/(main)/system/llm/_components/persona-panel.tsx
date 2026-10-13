"use client";

import * as React from "react";

import { Loader2Icon, PlusIcon, SaveIcon, StarIcon, Trash2Icon, UserRoundIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { Textarea } from "@/components/ui/textarea";
import { api } from "@/lib/api";
import type { Persona } from "@/lib/types";
import { cn } from "@/lib/utils";

function PersonaSheet({
  persona,
  open,
  onOpenChange,
  onSaved,
}: {
  persona: Persona | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onSaved: () => Promise<void>;
}) {
  const isNew = persona === null;
  const [name, setName] = React.useState("");
  const [prompt, setPrompt] = React.useState("");
  const [saving, setSaving] = React.useState(false);

  React.useEffect(() => {
    if (!open) return;
    setName(persona?.name ?? "");
    setPrompt(persona?.prompt ?? "");
  }, [open, persona]);

  async function save() {
    const nextName = name.trim();
    if (!nextName || !prompt.trim()) {
      toast.error("请填写人格名称和人格设定");
      return;
    }
    if (saving) return;
    setSaving(true);
    try {
      await api.savePersona({
        ...(persona ? { id: Number(persona.id) } : {}),
        name: nextName,
        prompt,
      });
      toast.success(isNew ? `已新建人格：${nextName}` : "人格已保存");
      await onSaved();
      onOpenChange(false);
    } catch (e) {
      toast.error(`保存人格失败：${(e as Error).message}`);
    } finally {
      setSaving(false);
    }
  }

  let actionIcon = <SaveIcon />;
  if (saving) {
    actionIcon = <Loader2Icon className="animate-spin" />;
  } else if (isNew) {
    actionIcon = <PlusIcon />;
  }

  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        className="flex flex-col gap-0 p-0 data-[side=right]:min-w-[420px] data-[side=right]:sm:max-w-xl"
      >
        <SheetHeader className="px-4">
          <SheetTitle className="flex items-center gap-2">
            <UserRoundIcon className="size-4" />
            {isNew ? "新建全局人格" : `编辑：${persona.name}`}
            {persona?.is_active && (
              <Badge variant="outline" className="border-amber-400/50 text-amber-500">
                激活中
              </Badge>
            )}
          </SheetTitle>
          <SheetDescription>
            人格只在每次 ChatAgent 请求的 system prompt 边界注入，不会改写用户消息、活动记录或历史 transcript。
          </SheetDescription>
        </SheetHeader>

        <div className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto px-4 pb-4">
          <div className="grid gap-2">
            <Label htmlFor="persona-name">名称</Label>
            <Input
              id="persona-name"
              placeholder="例如：严谨的代码审查员"
              value={name}
              onChange={(e) => setName(e.target.value)}
              maxLength={120}
            />
          </div>
          <div className="grid min-h-0 gap-2">
            <Label htmlFor="persona-prompt">人格设定</Label>
            <Textarea
              id="persona-prompt"
              className="min-h-64 resize-y font-mono text-sm"
              placeholder="描述回答风格、角色边界、表达习惯和需要遵守的长期要求。"
              value={prompt}
              onChange={(e) => setPrompt(e.target.value)}
              maxLength={32000}
            />
            <p className="text-muted-foreground text-xs">
              保存后，下一轮主会话和原生后台子代理会使用当前激活人格。未激活的人格只作为可选配置保存。
            </p>
          </div>
        </div>

        <div className="flex gap-2 border-t px-4 py-3">
          <Button onClick={() => void save()} disabled={saving} className="flex-1">
            {actionIcon}
            {isNew ? "新建" : "保存"}
          </Button>
        </div>
      </SheetContent>
    </Sheet>
  );
}

export function PersonaPanel() {
  const [personas, setPersonas] = React.useState<Persona[]>([]);
  const [editing, setEditing] = React.useState<Persona | null>(null);
  const [editOpen, setEditOpen] = React.useState(false);
  const [loading, setLoading] = React.useState(true);

  const load = React.useCallback(async () => {
    try {
      setPersonas(await api.personas());
    } catch (e) {
      toast.error(`加载人格失败：${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    void load();
  }, [load]);

  const openEditor = React.useCallback((persona: Persona | null) => {
    setEditing(persona);
    setEditOpen(true);
  }, []);

  async function activate(persona: Persona) {
    try {
      await api.activatePersona(persona.id);
      toast.success(`已激活人格：${persona.name}`);
      await load();
    } catch (e) {
      toast.error(`激活人格失败：${(e as Error).message}`);
    }
  }

  async function remove(persona: Persona) {
    try {
      await api.deletePersona(persona.id);
      toast.success(
        persona.is_active ? `已删除人格：${persona.name}，当前已无激活人格` : `已删除人格：${persona.name}`,
      );
      await load();
    } catch (e) {
      toast.error(`删除人格失败：${(e as Error).message}`);
    }
  }

  return (
    <section className="grid gap-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="flex items-center gap-2 font-semibold text-xl tracking-tight">
            <UserRoundIcon className="size-5" /> 全局人格
          </h2>
          <p className="text-muted-foreground text-sm">
            当前激活人格会随主会话和原生后台子代理共享。点击卡片编辑，星标为当前激活人格。
          </p>
        </div>
        <Button size="sm" variant="outline" onClick={() => openEditor(null)}>
          <PlusIcon /> 新建人格
        </Button>
      </div>

      {loading ? (
        <div className="flex items-center justify-center rounded-lg border border-dashed p-8 text-muted-foreground text-sm">
          <Loader2Icon className="mr-2 size-4 animate-spin" /> 正在加载人格…
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
          {personas.map((persona) => (
            <Card
              key={persona.id}
              role="button"
              tabIndex={0}
              onClick={() => openEditor(persona)}
              onKeyDown={(e) => {
                if (e.key === "Enter" || e.key === " ") {
                  e.preventDefault();
                  openEditor(persona);
                }
              }}
              className={cn(
                "cursor-pointer gap-0 py-4 outline-none transition-colors hover:border-foreground/30",
                persona.is_active && "border-amber-400/50 bg-amber-400/5",
              )}
            >
              <CardContent className="grid gap-2 px-4">
                <div className="flex items-start gap-2">
                  <StarIcon
                    className={cn(
                      "mt-0.5 size-4 shrink-0",
                      persona.is_active ? "fill-amber-400 text-amber-400" : "text-muted-foreground",
                    )}
                  />
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="truncate font-medium text-sm">{persona.name}</span>
                      {persona.is_active && (
                        <Badge variant="outline" className="ml-auto border-amber-400/50 text-amber-500">
                          激活中
                        </Badge>
                      )}
                    </div>
                    <p className="mt-2 line-clamp-4 whitespace-pre-wrap text-muted-foreground text-xs">
                      {persona.prompt}
                    </p>
                  </div>
                </div>

                <div className="mt-1 flex gap-2">
                  <Button
                    size="sm"
                    variant="outline"
                    className="flex-1"
                    disabled={persona.is_active}
                    onClick={(e) => {
                      e.stopPropagation();
                      void activate(persona);
                    }}
                  >
                    {persona.is_active ? "已激活" : "设为激活"}
                  </Button>
                  <Button
                    size="icon"
                    variant="outline"
                    aria-label="删除人格"
                    onClick={(e) => {
                      e.stopPropagation();
                      void remove(persona);
                    }}
                  >
                    <Trash2Icon className="text-destructive" />
                  </Button>
                </div>
              </CardContent>
            </Card>
          ))}
          {personas.length === 0 && (
            <div className="col-span-full rounded-lg border border-dashed p-10 text-center text-muted-foreground text-sm">
              还没有全局人格，点击右上角「新建人格」创建一个。
            </div>
          )}
        </div>
      )}

      <PersonaSheet persona={editing} open={editOpen} onOpenChange={setEditOpen} onSaved={load} />
    </section>
  );
}
