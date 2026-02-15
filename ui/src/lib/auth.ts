import {
  createContext,
  useContext,
  useState,
  useCallback,
  useEffect,
  type ReactNode,
} from "react";
import { createElement } from "react";
import { login as apiLogin, setTokenProvider } from "@/lib/api";

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

function loadPersistedAuth(): AuthState {
  try {
    const token = localStorage.getItem(STORAGE_KEY_TOKEN);
    const agentId = localStorage.getItem(STORAGE_KEY_AGENT);
    const expiresRaw = localStorage.getItem(STORAGE_KEY_EXPIRES);
    if (!token || !agentId || !expiresRaw) {
      return { token: null, agentId: null, expiresAt: null };
    }
    const expiresAt = new Date(expiresRaw);
    if (expiresAt.getTime() <= Date.now()) {
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

  // Register the token provider for the API client.
  useEffect(() => {
    setTokenProvider(() => state.token);
  }, [state.token]);

  // Auto-logout when token expires.
  useEffect(() => {
    if (!state.expiresAt) return;
    const ms = state.expiresAt.getTime() - Date.now();
    if (ms <= 0) {
      clearPersistedAuth();
      setState({ token: null, agentId: null, expiresAt: null });
      return;
    }
    const timer = setTimeout(() => {
      clearPersistedAuth();
      setState({ token: null, agentId: null, expiresAt: null });
    }, ms);
    return () => clearTimeout(timer);
  }, [state.expiresAt]);

  const login = useCallback(async (agentId: string, apiKey: string) => {
    const result = await apiLogin(agentId, apiKey);
    const expiresAt = new Date(result.expires_at);
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
