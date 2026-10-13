"use client";

import * as React from "react";

import { FileUpIcon, ImageIcon, Loader2Icon, PlusIcon } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { cn } from "@/lib/utils";

export function ChatAttachmentMenu({
  disabled = false,
  uploading = false,
  onPickFiles,
}: {
  disabled?: boolean;
  uploading?: boolean;
  onPickFiles: (files: File[]) => void;
}) {
  const [open, setOpen] = React.useState(false);
  const fileInputRef = React.useRef<HTMLInputElement>(null);
  const imageInputRef = React.useRef<HTMLInputElement>(null);
  const unavailable = disabled || uploading;
  const isOpen = open && !unavailable;

  React.useEffect(() => {
    if (unavailable) setOpen(false);
  }, [unavailable]);

  function openPicker(input: HTMLInputElement | null) {
    if (unavailable) return;
    // Keep the native picker inside the click's synchronous user activation.
    input?.click();
    setOpen(false);
  }

  function handleFileChange(event: React.ChangeEvent<HTMLInputElement>) {
    // Snapshot the live FileList before resetting for same-file reselection.
    const files = Array.from(event.currentTarget.files ?? []);
    event.currentTarget.value = "";
    if (files.length > 0 && !unavailable) onPickFiles(files);
  }

  return (
    <>
      <Popover open={isOpen} onOpenChange={setOpen}>
        <PopoverTrigger asChild>
          <Button
            type="button"
            variant="ghost"
            size="icon"
            className="size-11 text-muted-foreground motion-reduce:transition-none"
            disabled={unavailable}
            aria-label="添加附件"
            aria-busy={uploading}
            title={uploading ? "正在上传附件" : "添加附件"}
          >
            {uploading ? (
              <Loader2Icon aria-hidden="true" className="size-5 animate-spin motion-reduce:animate-none" />
            ) : (
              <PlusIcon
                aria-hidden="true"
                className={cn(
                  "size-5 transition-transform duration-150 ease-out motion-reduce:transition-none",
                  isOpen && "rotate-90",
                )}
              />
            )}
          </Button>
        </PopoverTrigger>
        <PopoverContent
          side="top"
          align="start"
          sideOffset={8}
          collisionPadding={12}
          aria-label="选择附件类型"
          className="max-h-(--radix-popover-content-available-height) w-64 max-w-[calc(100vw-1.5rem)] gap-2 overflow-y-auto p-2 data-open:[animation-delay:150ms] data-open:[animation-fill-mode:both] motion-reduce:data-open:animate-none motion-reduce:data-closed:animate-none"
        >
          <p className="px-1 text-xs text-muted-foreground">选择附件类型</p>
          <Button
            type="button"
            variant="outline"
            className="h-auto min-h-14 w-full min-w-0 justify-start gap-3 px-3 py-3 text-left whitespace-normal motion-reduce:transition-none"
            disabled={unavailable}
            onClick={() => openPicker(fileInputRef.current)}
          >
            <FileUpIcon aria-hidden="true" className="size-5 text-muted-foreground" />
            上传文件
          </Button>
          <Button
            type="button"
            variant="outline"
            className="h-auto min-h-14 w-full min-w-0 justify-start gap-3 px-3 py-3 text-left whitespace-normal motion-reduce:transition-none"
            disabled={unavailable}
            onClick={() => openPicker(imageInputRef.current)}
          >
            <ImageIcon aria-hidden="true" className="size-5 text-muted-foreground" />
            上传图片
          </Button>
        </PopoverContent>
      </Popover>
      <span role="status" className="sr-only">
        {uploading ? "正在上传附件" : ""}
      </span>
      <input
        ref={fileInputRef}
        type="file"
        hidden
        multiple
        disabled={unavailable}
        aria-label="上传文件"
        onChange={handleFileChange}
      />
      <input
        ref={imageInputRef}
        type="file"
        accept="image/*"
        hidden
        multiple
        disabled={unavailable}
        aria-label="上传图片"
        onChange={handleFileChange}
      />
    </>
  );
}
