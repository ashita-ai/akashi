import "@testing-library/jest-dom/vitest";
import { afterEach } from "vitest";

// jsdom 28 shipped with vitest 4 provides a Proxy-based localStorage that may
// be missing .getItem/.setItem in some configurations. Provide a standards-
// compliant in-memory shim that works reliably in all cases.
const storage = new Map<string, string>();
const localStorageShim: Storage = {
  getItem: (key: string) => storage.get(key) ?? null,
  setItem: (key: string, value: string) => storage.set(key, String(value)),
  removeItem: (key: string) => storage.delete(key),
  clear: () => storage.clear(),
  key: (index: number) => [...storage.keys()][index] ?? null,
  get length() {
    return storage.size;
  },
};

Object.defineProperty(globalThis, "localStorage", {
  value: localStorageShim,
  writable: true,
  configurable: true,
});

afterEach(() => {
  storage.clear();
});

// Stub matchMedia for jsdom (used by theme.ts and responsive components).
Object.defineProperty(window, "matchMedia", {
  writable: true,
  value: (query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addListener: () => {},
    removeListener: () => {},
    addEventListener: () => {},
    removeEventListener: () => {},
    dispatchEvent: () => false,
  }),
});
