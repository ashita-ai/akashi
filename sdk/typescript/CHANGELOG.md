# Changelog

## [Unreleased]

### Breaking Changes

- `TokenManager.getToken()` no longer accepts an `AbortSignal` parameter.
  Cancellation is now handled internally via `AbortSignal.timeout()` using
  the `timeoutMs` constructor parameter. Callers passing a signal should
  remove the argument — the method signature is now `getToken(): Promise<string>`.
  Concurrent callers now share a single in-flight refresh to avoid redundant
  token requests.
