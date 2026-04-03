export const DEFAULT_MAX_RETRIES = 3;
export const DEFAULT_RETRY_BASE_DELAY_MS = 500;
const MAX_RETRY_DELAY_MS = 30_000;
const MAX_RESPONSE_BYTES = 10 * 1024 * 1024; // 10 MiB

/** Returns true for status codes that should trigger a retry (429, 5xx). */
export function isRetryableStatus(code: number): boolean {
  return code === 429 || code >= 500;
}

/**
 * Compute the retry delay with exponential backoff and ±25% jitter.
 * Respects Retry-After when it exceeds the calculated delay.
 */
export function retryDelayMs(
  attempt: number,
  baseDelayMs: number,
  retryAfterMs = 0,
): number {
  let delay = baseDelayMs * Math.pow(2, attempt);
  if (delay > MAX_RETRY_DELAY_MS) delay = MAX_RETRY_DELAY_MS;

  // ±25% jitter
  const quarter = delay / 4;
  const jitter = Math.random() * 2 * quarter - quarter;
  delay += jitter;

  // Honour Retry-After if it's longer
  if (retryAfterMs > 0 && retryAfterMs > delay) {
    delay = Math.min(retryAfterMs, MAX_RETRY_DELAY_MS);
  }

  return Math.max(0, delay);
}

/** Parse integer seconds from a Retry-After header value. Returns 0 on failure. */
export function parseRetryAfter(header: string | null | undefined): number {
  if (!header) return 0;
  const secs = parseInt(header, 10);
  if (isNaN(secs) || secs <= 0) return 0;
  return secs * 1000; // convert to ms
}

/** Throw if the response body exceeds 10 MiB. */
export function checkResponseSize(contentLength: string | null): void {
  if (contentLength) {
    const len = parseInt(contentLength, 10);
    if (!isNaN(len) && len > MAX_RESPONSE_BYTES) {
      throw new Error(`Response body exceeds ${MAX_RESPONSE_BYTES} bytes limit`);
    }
  }
}

/** Sleep for the given number of milliseconds. Respects AbortSignal. */
export function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(signal.reason);
      return;
    }
    const timer = setTimeout(resolve, ms);
    signal?.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        reject(signal.reason);
      },
      { once: true },
    );
  });
}
