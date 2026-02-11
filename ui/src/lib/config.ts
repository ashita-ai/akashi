import {
  createContext,
  useContext,
  useState,
  useEffect,
  type ReactNode,
} from "react";
import { createElement } from "react";

interface AppConfig {
  billing_enabled: boolean;
  search_enabled: boolean;
}

const defaultConfig: AppConfig = {
  billing_enabled: false,
  search_enabled: false,
};

const ConfigContext = createContext<AppConfig>(defaultConfig);

export function useConfig(): AppConfig {
  return useContext(ConfigContext);
}

export function ConfigProvider({ children }: { children: ReactNode }) {
  const [config, setConfig] = useState<AppConfig>(defaultConfig);

  useEffect(() => {
    fetch("/config")
      .then((res) => {
        if (!res.ok) return defaultConfig;
        return res.json();
      })
      .then((json) => {
        // The backend wraps in { data: {...}, meta: {...} }.
        if (json?.data) {
          setConfig({
            billing_enabled: Boolean(json.data.billing_enabled),
            search_enabled: Boolean(json.data.search_enabled),
          });
        }
      })
      .catch(() => {
        // Fail-safe: OSS users never see broken billing.
      });
  }, []);

  return createElement(ConfigContext.Provider, { value: config }, children);
}
