"""Token management for Akashi API authentication."""

from __future__ import annotations

import asyncio
import time
from dataclasses import dataclass, field
from datetime import datetime, timezone

import httpx


@dataclass
class TokenManager:
    """Manages JWT token lifecycle with automatic refresh.

    Uses an asyncio.Lock to prevent concurrent refresh calls from
    racing. For the synchronous path, concurrent access is not
    expected (httpx.Client is not thread-safe either), but the
    logic is structured identically for consistency.
    """

    base_url: str
    agent_id: str
    api_key: str
    _token: str = field(default="", init=False, repr=False)
    _expires_at: float = field(default=0.0, init=False, repr=False)
    _refresh_margin_seconds: float = field(default=30.0, init=False)
    _lock: asyncio.Lock = field(default_factory=asyncio.Lock, init=False, repr=False)

    def _is_valid(self) -> bool:
        return bool(self._token) and time.time() < self._expires_at - self._refresh_margin_seconds

    async def get_token(self, client: httpx.AsyncClient) -> str:
        """Return a valid token, refreshing if necessary.

        Uses an asyncio.Lock so concurrent coroutines don't
        each trigger their own refresh.
        """
        if self._is_valid():
            return self._token
        async with self._lock:
            # Double-check after acquiring the lock.
            if self._is_valid():
                return self._token
            await self._refresh(client)
        return self._token

    def get_token_sync(self, client: httpx.Client) -> str:
        """Synchronous version of get_token."""
        if self._is_valid():
            return self._token
        self._refresh_sync(client)
        return self._token

    def _apply_token_response(self, data: dict) -> None:
        """Parse and store a token response from the server."""
        self._token = data["token"]
        self._expires_at = (
            datetime.fromisoformat(data["expires_at"].replace("Z", "+00:00"))
            .replace(tzinfo=timezone.utc)
            .timestamp()
        )

    async def _refresh(self, client: httpx.AsyncClient) -> None:
        resp = await client.post(
            f"{self.base_url}/auth/token",
            json={"agent_id": self.agent_id, "api_key": self.api_key},
        )
        resp.raise_for_status()
        self._apply_token_response(resp.json()["data"])

    def _refresh_sync(self, client: httpx.Client) -> None:
        resp = client.post(
            f"{self.base_url}/auth/token",
            json={"agent_id": self.agent_id, "api_key": self.api_key},
        )
        resp.raise_for_status()
        self._apply_token_response(resp.json()["data"])
