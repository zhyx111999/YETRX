"use client";

import * as React from "react";
import { toast } from "sonner";

// Ignore late upload responses after navigation, and prevent overlapping picks
// even before React has committed the uploading state.
export function useAttachmentUpload(scope: string) {
  const [uploading, setUploading] = React.useState(false);
  const current = React.useRef({ active: false, pending: false });

  React.useEffect(() => {
    const lifetime = { active: true, pending: false };
    current.current = lifetime;
    setUploading(false);
    return () => { lifetime.active = false; };
  }, [scope]);

  async function upload<T>(files: File[], request: () => Promise<T>, accept: (result: T) => void) {
    const lifetime = current.current;
    if (!files.length || !lifetime.active || lifetime.pending) return;
    lifetime.pending = true;
    setUploading(true);
    try {
      const result = await request();
      if (lifetime.active) accept(result);
    } catch (error) {
      if (lifetime.active) toast.error(`上传失败：${error instanceof Error ? error.message : String(error)}`);
    } finally {
      lifetime.pending = false;
      if (lifetime.active) setUploading(false);
    }
  }

  return { uploading, upload };
}
