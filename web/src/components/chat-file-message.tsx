"use client";

import * as React from "react";

import { Download, FileText, ImageIcon, Loader2, Paperclip } from "lucide-react";
import { toast } from "sonner";

import {
  Attachment,
  AttachmentActions,
  AttachmentContent,
  AttachmentDescription,
  AttachmentMedia,
  AttachmentTitle,
} from "@/components/ui/attachment";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import type { Activity, ChatFile } from "@/lib/types";

type FileMessage = { text: string; attachments: ChatFile[] };

function parseFileMessage(body: string): FileMessage {
  const value: unknown = JSON.parse(body);
  if (!value || typeof value !== "object" || !("attachments" in value) || !Array.isArray(value.attachments)) {
    throw new Error("附件消息格式无效");
  }
  const attachments = value.attachments.filter((file: unknown): file is ChatFile => {
    if (!file || typeof file !== "object") return false;
    const item = file as Partial<ChatFile>;
    return (
      typeof item.id === "string" &&
      /^[a-f0-9]{32}$/.test(item.id) &&
      Number.isSafeInteger(item.conversation_id) &&
      (item.conversation_id ?? 0) > 0 &&
      typeof item.name === "string" &&
      typeof item.size === "number" &&
      item.size >= 0 &&
      typeof item.mime_type === "string"
    );
  });
  if (attachments.length === 0) throw new Error("附件信息不可用");
  return { text: "text" in value && typeof value.text === "string" ? value.text : "", attachments };
}

function formatFileSize(size: number): string {
  if (size >= 1 << 20) return `${(size / (1 << 20)).toFixed(1)} MB`;
  if (size >= 1 << 10) return `${(size / (1 << 10)).toFixed(1)} KB`;
  return `${size} B`;
}

function FileCard({ file }: { file: ChatFile }) {
  const [downloading, setDownloading] = React.useState(false);
  const size = formatFileSize(file.size);
  const Icon = file.mime_type.startsWith("image/") ? ImageIcon : FileText;

  async function download() {
    if (downloading) return;
    setDownloading(true);
    try {
      await api.downloadChatFile(file);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "下载失败，请重试");
    } finally {
      setDownloading(false);
    }
  }

  return (
    <Attachment state="done" className="w-full max-w-md flex-nowrap" data-testid="agent-file-card">
      <AttachmentMedia>
        <Icon aria-hidden="true" />
      </AttachmentMedia>
      <AttachmentContent>
        <AttachmentTitle title={file.name}>{file.name}</AttachmentTitle>
        <AttachmentDescription>{size} · 可下载文件</AttachmentDescription>
      </AttachmentContent>
      <AttachmentActions>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="size-11 shrink-0"
          onClick={download}
          disabled={downloading}
          aria-label={`下载 ${file.name}`}
          title={`下载 ${file.name}`}
        >
          {downloading ? (
            <Loader2 className="size-4 animate-spin motion-reduce:animate-none" />
          ) : (
            <Download className="size-4" />
          )}
        </Button>
      </AttachmentActions>
    </Attachment>
  );
}

export function ChatFileMessage({ step, getDetail }: { step: Activity; getDetail: (seq: number) => Promise<string> }) {
  const [message, setMessage] = React.useState<FileMessage | null>(null);
  const [error, setError] = React.useState("");
  const [attempt, setAttempt] = React.useState(0);
  React.useEffect(() => {
    let live = true;
    setError("");
    const detail = attempt === 0 && step.detail ? Promise.resolve(step.detail) : getDetail(step.seq);
    void detail
      .then((body) => {
        const parsed = parseFileMessage(body);
        if (live) setMessage(parsed);
      })
      .catch((reason: unknown) => {
        if (live) setError(reason instanceof Error ? reason.message : "附件加载失败");
      });
    return () => {
      live = false;
    };
  }, [step.seq, step.detail, getDetail, attempt]);

  return (
    <section className="my-2 flex min-w-0 flex-col gap-2" aria-label="Agent 发送的文件">
      {message ? (
        <>
          {message.text && <p className="whitespace-pre-wrap break-words text-sm">{message.text}</p>}
          {message.attachments.map((file) => (
            <FileCard key={file.id} file={file} />
          ))}
        </>
      ) : (
        <div className="flex items-center gap-2 text-muted-foreground text-sm">
          <Paperclip className="size-4 shrink-0" />
          <span className="min-w-0 break-words">{error || `正在加载附件：${step.summary}`}</span>
          {error && (
            <Button type="button" variant="ghost" size="sm" onClick={() => setAttempt((value) => value + 1)}>
              重试
            </Button>
          )}
        </div>
      )}
    </section>
  );
}
