import {
  createContext,
  useContext,
  useState,
  useCallback,
  useEffect,
  type ReactNode,
} from "react";
import { createElement } from "react";
import { login as apiLogin } from "@/lib/api";

const STORAGE_KEY_TOKEN = "akashi_token";
const STORAGE_KEY_AGENT = "akashi_agent_id";
const STORAGE_KEY_EXPIRES = "akashi_expires_at";

interface AuthState {
  token: string | null;
  agentId: string | null;
  expiresAt: Date | null;
}

interface AuthContextValue {
  isAuthenticated: boolean;
  agentId: string | null;
  token: string | null;
  login: (agentId: string, apiKey: string) => Promise<void>;
  logout: () => void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error("useAuth must be used within AuthProvider");
  }
  return ctx;
}

// Normalize RFC3339Nano (9 fractional digits) to millisecond precision.
// Go's time.Time JSON marshals with nanosecond precision; JS Date only supports milliseconds.
function parseExpiresAt(raw: string): Date | null {
  const normalized = raw.replace(/(\.\d{3})\d*/, "$1");
  const d = new Date(normalized);
  return Number.isFinite(d.getTime()) ? d : null;
}

function loadPersistedAuth(): AuthState {
  try {
    const token = localStorage.getItem(STORAGE_KEY_TOKEN);
    const agentId = localStorage.getItem(STORAGE_KEY_AGENT);
    const expiresRaw = localStorage.getItem(STORAGE_KEY_EXPIRES);
    if (!token || !agentId || !expiresRaw) {
      return { token: null, agentId: null, expiresAt: null };
    }
    const expiresAt = parseExpiresAt(expiresRaw);
    if (!expiresAt || expiresAt.getTime() <= Date.now()) {
      clearPersistedAuth();
      return { token: null, agentId: null, expiresAt: null };
    }
    return { token, agentId, expiresAt };
  } catch {
    return { token: null, agentId: null, expiresAt: null };
  }
}

function persistAuth(token: string, agentId: string, expiresAt: Date): void {
  try {
    localStorage.setItem(STORAGE_KEY_TOKEN, token);
    localStorage.setItem(STORAGE_KEY_AGENT, agentId);
    localStorage.setItem(STORAGE_KEY_EXPIRES, expiresAt.toISOString());
  } catch {
    // localStorage may be unavailable (private browsing, quota exceeded).
  }
}

function clearPersistedAuth(): void {
  try {
    localStorage.removeItem(STORAGE_KEY_TOKEN);
    localStorage.removeItem(STORAGE_KEY_AGENT);
    localStorage.removeItem(STORAGE_KEY_EXPIRES);
  } catch {
    // Ignore.
  }
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>(loadPersistedAuth);

  // Auto-logout when token expires.
  useEffect(() => {
    if (!state.expiresAt || !Number.isFinite(state.expiresAt.getTime())) return;
    const ms = state.expiresAt.getTime() - Date.now();
    if (ms <= 0) {
      clearPersistedAuth();
      setState({ token: null, agentId: null, expiresAt: null });
      return;
    }
    // setTimeout delay is a 32-bit signed integer internally; values above
    // 2^31-1 ms (~24.8 days) wrap around and fire immediately. Cap the delay
    // so long-lived tokens don't instantly log the user out. If the cap fires
    // before actual expiry, the callback re-checks before clearing.
    const MAX_TIMEOUT_MS = 2_147_483_647; // 2^31 - 1
    const delay = Math.min(ms, MAX_TIMEOUT_MS);
    const timer = setTimeout(() => {
      if (!state.expiresAt || state.expiresAt.getTime() <= Date.now()) {
        clearPersistedAuth();
        setState({ token: null, agentId: null, expiresAt: null });
      }
    }, delay);
    return () => clearTimeout(timer);
  }, [state.expiresAt]);

  const login = useCallback(async (agentId: string, apiKey: string) => {
    const result = await apiLogin(agentId, apiKey);
    const expiresAt = parseExpiresAt(result.expires_at);
    if (!expiresAt || !Number.isFinite(expiresAt.getTime())) {
      throw new Error("Invalid token expiration from server");
    }
    persistAuth(result.token, agentId, expiresAt);
    setState({ token: result.token, agentId, expiresAt });
  }, []);

  const logout = useCallback(() => {
    clearPersistedAuth();
    setState({ token: null, agentId: null, expiresAt: null });
  }, []);

  const value: AuthContextValue = {
    isAuthenticated: state.token !== null,
    agentId: state.agentId,
    token: state.token,
    login,
    logout,
  };

  return createElement(AuthContext.Provider, { value }, children);
}
