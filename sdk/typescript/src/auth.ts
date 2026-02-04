/** Token management for Kyoyu API authentication. */

export class TokenManager {
  private token = "";
  private expiresAt = 0;
  private readonly refreshMarginMs = 30_000;

  constructor(
    private readonly baseUrl: string,
    private readonly agentId: string,
    private readonly apiKey: string,
    private readonly timeoutMs: number,
  ) {}

  /** Return a valid token, refreshing if necessary. */
  async getToken(signal?: AbortSignal): Promise<string> {
    if (this.token && Date.now() < this.expiresAt - this.refreshMarginMs) {
      return this.token;
    }
    await this.refresh(signal);
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
