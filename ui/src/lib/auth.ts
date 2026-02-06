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

interface AuthState {
  token: string | null;
  agentId: string | null;
  expiresAt: Date | null;
}

interface AuthContextValue {
  isAuthenticated: boolean;
  agentId: string | null;
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

export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<AuthState>({
    token: null,
    agentId: null,
    expiresAt: null,
  });

  // Register the token provider for the API client.
  useEffect(() => {
    setTokenProvider(() => state.token);
  }, [state.token]);

  // Auto-logout when token expires.
  useEffect(() => {
    if (!state.expiresAt) return;
    const ms = state.expiresAt.getTime() - Date.now();
    if (ms <= 0) {
      setState({ token: null, agentId: null, expiresAt: null });
      return;
    }
    const timer = setTimeout(() => {
      setState({ token: null, agentId: null, expiresAt: null });
    }, ms);
    return () => clearTimeout(timer);
  }, [state.expiresAt]);

  const login = useCallback(async (agentId: string, apiKey: string) => {
    const result = await apiLogin(agentId, apiKey);
    setState({
      token: result.token,
      agentId,
      expiresAt: new Date(result.expires_at),
    });
  }, []);

  const logout = useCallback(() => {
    setState({ token: null, agentId: null, expiresAt: null });
  }, []);

  const value: AuthContextValue = {
    isAuthenticated: state.token !== null,
    agentId: state.agentId,
    login,
    logout,
  };

  return createElement(AuthContext.Provider, { value }, children);
}
