"use client";

import * as React from "react";
import { FileIcon, FileTextIcon, ImageIcon, XIcon } from "lucide-react";

import {
  Attachment,
  AttachmentAction,
  AttachmentActions,
  AttachmentContent,
  AttachmentDescription,
  AttachmentGroup,
  AttachmentMedia,
  AttachmentTitle,
} from "@/components/ui/attachment";
import { attachmentKind, localAttachmentFile } from "@/lib/chat-attachments";
import type { ChatAttachment } from "@/lib/types";

function fileSize(bytes: number) {
  if (bytes >= 1 << 20) return `${(bytes / (1 << 20)).toFixed(1)} MB`;
  if (bytes >= 1 << 10) return `${(bytes / (1 << 10)).toFixed(1)} KB`;
  return `${bytes} B`;
}

function AttachmentThumbnail({ attachment }: { attachment: ChatAttachment }) {
  const file = localAttachmentFile(attachment);
  const kind = attachmentKind(attachment.name, file?.type);
  const [preview, setPreview] = React.useState<{ file: File; url: string } | null>(null);
  const [failedURL, setFailedURL] = React.useState<string>();

  // The mounted thumbnail owns its URL. Draft handoff and failed sends only
  // transfer File references, so unmount/replay always revokes the correct URL.
  React.useEffect(() => {
    if (!file || kind !== "image") return;
    let url: string;
    try {
      url = URL.createObjectURL(file);
    } catch {
      return;
    }
    setPreview({ file, url });
    return () => URL.revokeObjectURL(url);
  }, [file, kind]);

  const src = preview?.file === file ? preview?.url : undefined;
  if (src && src !== failedURL) {
    return (
      <AttachmentMedia variant="image">
        {/* Browser-local blobs need no Next.js image optimization or server route. */}
        {/* eslint-disable-next-line @next/next/no-img-element */}
        <img src={src} alt={attachment.name} className="size-full object-contain!" onError={() => setFailedURL(src)} />
      </AttachmentMedia>
    );
  }

  const Icon = kind === "pdf" ? FileTextIcon : kind === "image" ? ImageIcon : FileIcon;
  const extension = attachment.name.includes(".") ? attachment.name.split(".").pop()?.slice(0, 8).toUpperCase() : "";
  const label = kind === "pdf" ? "PDF" : kind === "image" ? "图片" : extension || "文件";
  return (
    <AttachmentMedia className={kind === "pdf" ? "flex-col gap-1 bg-red-500/10 text-red-600 dark:text-red-400" : "flex-col gap-1"}>
      <Icon aria-hidden="true" className="size-7" />
      <span className="text-[10px] font-semibold tracking-wide">{label}</span>
    </AttachmentMedia>
  );
}

export function ChatAttachmentPreviews({ attachments, onRemove, disabled = false }: {
  attachments: ChatAttachment[];
  onRemove?: (path: string) => void;
  disabled?: boolean;
}) {
  if (!attachments.length) return null;
  return (
    <AttachmentGroup className="mb-2 max-w-full gap-2" aria-label="待发送附件">
      {attachments.map((attachment) => (
        <Attachment key={attachment.path} orientation="vertical" className="w-28 has-data-[slot=attachment-content]:w-28" data-attachment-path={attachment.path}>
          <AttachmentThumbnail attachment={attachment} />
          <AttachmentContent>
            <AttachmentTitle title={attachment.name}>{attachment.name}</AttachmentTitle>
            <AttachmentDescription>{fileSize(attachment.size)}</AttachmentDescription>
          </AttachmentContent>
          {onRemove && (
            <AttachmentActions className="group-data-[orientation=vertical]/attachment:top-1 group-data-[orientation=vertical]/attachment:right-1">
              <AttachmentAction
                type="button"
                className="size-11 rounded-full bg-background/90 shadow-sm hover:bg-background"
                onClick={() => onRemove(attachment.path)}
                disabled={disabled}
                aria-label={`移除附件 ${attachment.name}`}
                title="移除附件"
              >
                <XIcon className="size-4" />
              </AttachmentAction>
            </AttachmentActions>
          )}
        </Attachment>
      ))}
    </AttachmentGroup>
  );
}
