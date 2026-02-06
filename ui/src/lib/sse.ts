import { useEffect, useRef, useState, useCallback } from "react";
import { useQueryClient } from "@tanstack/react-query";

export type SSEStatus = "connecting" | "connected" | "disconnected";

const INITIAL_RETRY_MS = 1000;
const MAX_RETRY_MS = 30000;

/**
 * Custom SSE client using fetch + ReadableStream.
 * EventSource doesn't support custom Authorization headers,
 * so we use the streaming fetch API instead.
 */
export function useSSE(token: string | null) {
  const [status, setStatus] = useState<SSEStatus>("disconnected");
  const queryClient = useQueryClient();
  const abortRef = useRef<AbortController | null>(null);
  const retryMs = useRef(INITIAL_RETRY_MS);

  const connect = useCallback(async () => {
    if (!token) return;

    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;

    setStatus("connecting");

    try {
      const res = await fetch("/v1/subscribe", {
        headers: { Authorization: `Bearer ${token}` },
        signal: controller.signal,
      });

      if (!res.ok || !res.body) {
        throw new Error(`SSE connect failed: ${res.status}`);
      }

      setStatus("connected");
      retryMs.current = INITIAL_RETRY_MS;

      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";

      while (true) {
        const { done, value } = await reader.read();
        if (done) break;

        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split("\n");
        buffer = lines.pop() ?? "";

        for (const line of lines) {
          if (line.startsWith("data:")) {
            const data = line.slice(5).trim();
            if (data) {
              handleSSEEvent(data, queryClient);
            }
          }
        }
      }
    } catch (err) {
      if (controller.signal.aborted) return;
      console.warn("SSE connection lost:", err);
    } finally {
      if (!controller.signal.aborted) {
        setStatus("disconnected");
        // Reconnect with exponential backoff.
        const delay = retryMs.current;
        retryMs.current = Math.min(retryMs.current * 2, MAX_RETRY_MS);
        setTimeout(() => connect(), delay);
      }
    }
  }, [token, queryClient]);

  useEffect(() => {
    connect();
    return () => {
      abortRef.current?.abort();
    };
  }, [connect]);

  return status;
}

function handleSSEEvent(
  data: string,
  queryClient: ReturnType<typeof useQueryClient>,
) {
  try {
    const event = JSON.parse(data) as { event: string };
    // Invalidate relevant queries when new data arrives.
    switch (event.event) {
      case "decision_made":
      case "decision_revised":
        queryClient.invalidateQueries({ queryKey: ["decisions"] });
        queryClient.invalidateQueries({ queryKey: ["dashboard"] });
        queryClient.invalidateQueries({ queryKey: ["usage"] });
        break;
      case "conflict_detected":
        queryClient.invalidateQueries({ queryKey: ["conflicts"] });
        queryClient.invalidateQueries({ queryKey: ["dashboard"] });
        break;
      default:
        // For any other event, refresh the dashboard.
        queryClient.invalidateQueries({ queryKey: ["dashboard"] });
    }
  } catch {
    // Ignore malformed events.
  }
}
