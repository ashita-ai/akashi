/** Token management for Akashi API authentication. */

export class TokenManager {
  private token = "";
  private expiresAt = 0;
  private readonly refreshMarginMs = 30_000;
  private refreshPromise: Promise<void> | null = null;

  constructor(
    private readonly baseUrl: string,
    private readonly agentId: string,
    private readonly apiKey: string,
    private readonly timeoutMs: number,
  ) {}

  /** Return a valid token, refreshing if necessary.
   *  Concurrent callers share a single in-flight refresh to avoid redundant
   *  token requests. If `signal` is provided, it races against the shared
   *  refresh — aborting the caller's wait without canceling the refresh
   *  itself (which other callers depend on). */
  async getToken(signal?: AbortSignal): Promise<string> {
    if (this.token && Date.now() < this.expiresAt - this.refreshMarginMs) {
      return this.token;
    }
    if (!this.refreshPromise) {
      this.refreshPromise = this.refresh().finally(() => {
        this.refreshPromise = null;
      });
    }
    // Race the shared refresh against the caller's abort signal so that
    // individual callers can still cancel their wait without breaking
    // concurrent callers who share the same refresh promise.
    if (signal) {
      await Promise.race([
        this.refreshPromise,
        new Promise<never>((_, reject) => {
          if (signal.aborted) {
            reject(signal.reason ?? new DOMException("Aborted", "AbortError"));
            return;
          }
          signal.addEventListener("abort", () => {
            reject(signal.reason ?? new DOMException("Aborted", "AbortError"));
          }, { once: true });
        }),
      ]);
    } else {
      await this.refreshPromise;
    }
    return this.token;
  }

  private async refresh(): Promise<void> {
    const resp = await fetch(`${this.baseUrl}/auth/token`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        agent_id: this.agentId,
        api_key: this.apiKey,
      }),
      signal: AbortSignal.timeout(this.timeoutMs),
    });

    if (!resp.ok) {
      throw new Error(`Token refresh failed: ${resp.status}`);
    }

    const body = (await resp.json()) as {
      data: { token: string; expires_at: string };
    };
    this.token = body.data.token;
    this.expiresAt = new Date(body.data.expires_at).getTime();
  }
}
