"use client";

import { useEffect, useRef, useState } from "react";

// Smooth provider chunks only while they are arriving. History and completed
// replies render immediately; a large chunk catches up instead of typing for minutes.
export function useStreamingText(text: string, streaming: boolean) {
  const [shown, setShown] = useState(streaming ? "" : text);
  const visible = useRef(streaming ? "" : text);

  useEffect(() => {
    if (!streaming || window.matchMedia("(prefers-reduced-motion: reduce)").matches) {
      visible.current = text;
      setShown(text);
      return;
    }
    if (!text.startsWith(visible.current)) visible.current = "";
    let frame: number;
    let previous = performance.now();
    const advance = (now: number) => {
      const next = text;
      if (!next.startsWith(visible.current)) visible.current = "";
      const remaining = Array.from(next.slice(visible.current.length));
      if (remaining.length) {
        const count = Math.max(1, Math.ceil(remaining.length * Math.min(1, (now - previous) / 100)));
        visible.current += remaining.slice(0, count).join("");
        setShown(visible.current);
      }
      previous = now;
      if (visible.current !== next) frame = requestAnimationFrame(advance);
    };
    frame = requestAnimationFrame(advance);
    return () => cancelAnimationFrame(frame);
  }, [text, streaming]);

  return streaming ? shown : text;
}
