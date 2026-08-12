/** Token management for Akashi API authentication. */
export declare class TokenManager {
    private readonly baseUrl;
    private readonly agentId;
    private readonly apiKey;
    private readonly timeoutMs;
    private token;
    private expiresAt;
    private readonly refreshMarginMs;
    constructor(baseUrl: string, agentId: string, apiKey: string, timeoutMs: number);
    /** Return a valid token, refreshing if necessary. */
    getToken(signal?: AbortSignal): Promise<string>;
    private refresh;
}
//# sourceMappingURL=auth.d.ts.map