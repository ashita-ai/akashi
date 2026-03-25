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
   *  token requests.
   *  @param _signal - Accepted for backward compatibility but not forwarded to
   *  the shared refresh promise (a caller-specific signal cannot cancel a
   *  shared in-flight request). */
  async getToken(_signal?: AbortSignal): Promise<string> {
    if (this.token && Date.now() < this.expiresAt - this.refreshMarginMs) {
      return this.token;
    }
    if (!this.refreshPromise) {
      this.refreshPromise = this.refresh().finally(() => {
        this.refreshPromise = null;
      });
    }
    await this.refreshPromise;
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
