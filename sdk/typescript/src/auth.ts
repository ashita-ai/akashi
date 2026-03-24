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
   *  token requests. */
  async getToken(signal?: AbortSignal): Promise<string> {
    if (this.token && Date.now() < this.expiresAt - this.refreshMarginMs) {
      return this.token;
    }
    if (!this.refreshPromise) {
      this.refreshPromise = this.refresh(signal).finally(() => {
        this.refreshPromise = null;
      });
    }
    await this.refreshPromise;
    return this.token;
  }

  private async refresh(signal?: AbortSignal): Promise<void> {
    const resp = await fetch(`${this.baseUrl}/auth/token`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        agent_id: this.agentId,
        api_key: this.apiKey,
      }),
      signal: signal ?? AbortSignal.timeout(this.timeoutMs),
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
