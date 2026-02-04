"""Middleware for enforcing the check-before/record-after pattern."""

from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Awaitable, Callable, Protocol, TypeVar

from kyoyu.client import KyoyuClient, KyoyuSyncClient
from kyoyu.types import CheckResponse, TraceRequest


class Traceable(Protocol):
    """Protocol for results that can be converted to trace requests."""

    def to_trace(self) -> TraceRequest: ...


T = TypeVar("T", bound=Traceable)


@dataclass
class KyoyuMiddleware:
    """Wraps async decision-making callables with automatic check-before/record-after.

    Usage::

        middleware = KyoyuMiddleware(client)

        async def choose_model(precedents: CheckResponse, **kwargs):
            # ... decision logic using precedents ...
            return result  # must have a to_trace() method

        result = await middleware.wrap("model_selection", choose_model)
    """

    client: KyoyuClient

    async def wrap(
        self,
        decision_type: str,
        func: Callable[..., Awaitable[T]],
        *args: Any,
        **kwargs: Any,
    ) -> T:
        """Execute *func* with the check-before/record-after pattern.

        1. Calls ``kyoyu_check`` for the given decision type.
        2. Invokes *func* with ``precedents`` as a keyword argument.
        3. Calls ``kyoyu_trace`` with the result's trace representation.

        *func* must accept a ``precedents`` keyword argument of type
        :class:`CheckResponse` and return an object implementing the
        :class:`Traceable` protocol.
        """
        precedents: CheckResponse = await self.client.check(decision_type)
        result: T = await func(*args, precedents=precedents, **kwargs)
        await self.client.trace(result.to_trace())
        return result


@dataclass
class KyoyuSyncMiddleware:
    """Wraps synchronous decision-making callables with automatic check-before/record-after.

    Usage::

        middleware = KyoyuSyncMiddleware(client)

        def choose_model(precedents: CheckResponse, **kwargs):
            # ... decision logic using precedents ...
            return result  # must have a to_trace() method

        result = middleware.wrap("model_selection", choose_model)
    """

    client: KyoyuSyncClient

    def wrap(
        self,
        decision_type: str,
        func: Callable[..., T],
        *args: Any,
        **kwargs: Any,
    ) -> T:
        """Execute *func* with the check-before/record-after pattern (synchronous)."""
        precedents: CheckResponse = self.client.check(decision_type)
        result: T = func(*args, precedents=precedents, **kwargs)
        self.client.trace(result.to_trace())
        return result
