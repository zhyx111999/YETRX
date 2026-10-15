import type { ChatAttachment } from "./types";

// File references stay local: JSON sent to the server contains metadata only.
const attachmentFiles = new WeakMap<ChatAttachment, File>();

export function withLocalAttachmentFiles(attachments: ChatAttachment[], files: File[]): ChatAttachment[] {
  if (attachments.length !== files.length) return attachments;
  return attachments.map((attachment, index) => {
    const file = files[index];
    const dot = file.name.lastIndexOf(".");
    const stem = dot < 0 ? file.name : file.name.slice(0, dot);
    const extension = dot < 0 ? "" : file.name.slice(dot);
    const suffix = attachment.name.slice(stem.length, extension ? -extension.length : undefined);
    const renamed = attachment.name.startsWith(stem) && attachment.name.endsWith(extension) && /^-[1-9]\d*$/.test(suffix);
    if (attachment.size !== file.size || (attachment.name !== file.name && !renamed)) return attachment;
    const local = { ...attachment };
    attachmentFiles.set(local, file);
    return local;
  });
}

export function localAttachmentFile(attachment: ChatAttachment): File | undefined {
  return attachmentFiles.get(attachment);
}

export function attachmentKind(name: string, mime = ""): "image" | "pdf" | "file" {
  if (mime.toLowerCase() === "application/pdf" || /\.pdf$/i.test(name)) return "pdf";
  if (mime.toLowerCase().startsWith("image/") || /\.(png|jpe?g|gif|webp|avif|bmp|svg|ico|heic|heif|tiff?)$/i.test(name)) return "image";
  return "file";
}

// Snapshot clipboard files during the paste event; the transfer becomes
// inaccessible afterwards. Prefer FileList so mirrored items aren't uploaded twice.
export function filesFromClipboard(data: DataTransfer): File[] {
  const files = Array.from(data.files);
  if (files.length > 0) return files;
  return Array.from(data.items).flatMap((item) => {
    const file = item.kind === "file" ? item.getAsFile() : null;
    return file ? [file] : [];
  });
}
