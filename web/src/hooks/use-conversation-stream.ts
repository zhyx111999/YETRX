"use client";

import { type RefObject, useEffect } from "react";

import { api, sseUrl } from "@/lib/api";
import type { Activity } from "@/lib/types";

export interface ConversationStreamSnapshot {
  items: Activity[];
  cursor: number;
  running: boolean;
  draft: Activity | null;
}

// History keeps its existing pagination. This connection only tails newer rows
// and the current draft; EventSource reconnects using the last durable cursor.
export function useConversationStream(
  id: number,
  enabled: boolean,
  cursor: RefObject<number>,
  receive: (snapshot: ConversationStreamSnapshot) => void,
) {
  useEffect(() => {
    if (!enabled) return;
    let live = true;
    let connected = false;
    let draft: Activity | null = null;
    let timer: ReturnType<typeof setTimeout>;
    const stream = process.env.NEXT_PUBLIC_MOCK === "1"
      ? null
      : new EventSource(sseUrl(`/api/conversations/${id}/stream?since=${cursor.current}`));

    // Keep the existing incremental API as compensation while SSE is unavailable.
    // A late poll must not overwrite a newer live snapshot after reconnection.
    const poll = async () => {
      try {
        if (!connected) {
          const data = await api.conversationMessages(id, cursor.current);
          if (!live || connected) return;
          if (!data.running || data.items.some((item) => ["text", "thinking", "result"].includes(item.kind))) draft = null;
          cursor.current = Math.max(cursor.current, data.cursor);
          receive({ ...data, draft });
        }
      } catch {
        /* transient — EventSource and the next poll both retry */
      } finally {
        if (live) timer = setTimeout(() => void poll(), 2000);
      }
    };
    timer = setTimeout(() => void poll(), 2000);
    if (stream) {
      stream.onopen = () => { connected = true; };
      stream.onerror = () => { connected = false; };
      stream.addEventListener("snapshot", (event) => {
        if (!live) return;
        try {
          const data = JSON.parse((event as MessageEvent).data) as ConversationStreamSnapshot;
          draft = data.draft;
          cursor.current = Math.max(cursor.current, data.cursor);
          receive(data);
        } catch {
          connected = false;
        }
      });
      stream.addEventListener("cleared", () => {
        stream.close();
        live = false;
        clearTimeout(timer);
        receive({ items: [], cursor: cursor.current, running: false, draft: null });
      });
    }
    return () => {
      live = false;
      clearTimeout(timer);
      stream?.close();
    };
  }, [id, enabled, cursor, receive]);
}
