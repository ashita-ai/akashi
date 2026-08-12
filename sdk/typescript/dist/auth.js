/** Token management for Akashi API authentication. */
export class TokenManager {
    baseUrl;
    agentId;
    apiKey;
    timeoutMs;
    token = "";
    expiresAt = 0;
    refreshMarginMs = 30_000;
    constructor(baseUrl, agentId, apiKey, timeoutMs) {
        this.baseUrl = baseUrl;
        this.agentId = agentId;
        this.apiKey = apiKey;
        this.timeoutMs = timeoutMs;
    }
    /** Return a valid token, refreshing if necessary. */
    async getToken(signal) {
        if (this.token && Date.now() < this.expiresAt - this.refreshMarginMs) {
            return this.token;
        }
        await this.refresh(signal);
        return this.token;
    }
    async refresh(signal) {
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
        const body = (await resp.json());
        this.token = body.data.token;
        this.expiresAt = new Date(body.data.expires_at).getTime();
    }
}
//# sourceMappingURL=auth.js.map