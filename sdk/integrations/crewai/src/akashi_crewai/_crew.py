"""High-level proxy that wraps a CrewAI Crew with Akashi tracing."""

from __future__ import annotations

import logging
from typing import Any, Callable

from akashi import AkashiSyncClient
from akashi.types import TraceRequest

from akashi_crewai._hooks import (
    AkashiCrewCallbacks,
    _MAX_OUTCOME,
    _MAX_QUERY,
    _trunc,
)

logger = logging.getLogger(__name__)


def _chain_callback(
    existing: Callable[..., Any] | None,
    akashi_cb: Callable[..., Any],
) -> Callable[..., Any]:
    """Return a callback that calls *existing* first, then *akashi_cb*.

    If *existing* is ``None``, returns *akashi_cb* directly.  Both
    callbacks receive the same positional argument (the CrewAI task or
    step output).  Exceptions from *existing* propagate normally;
    exceptions from *akashi_cb* are swallowed (fire-and-forget).
    """
    if existing is None:
        return akashi_cb

    def chained(output: Any) -> None:
        existing(output)
        try:
            akashi_cb(output)
        except Exception as exc:  # noqa: BLE001
            logger.debug("akashi chained callback failed (non-fatal): %s", exc)

    return chained


class AkashiCrew:
    """Transparent proxy that adds Akashi tracing to a CrewAI ``Crew``.

    Wraps a ``Crew`` instance and:

    1. Installs ``task_callback`` and ``step_callback`` that compose with
       any existing callbacks on the crew (user's callback fires first,
       then the Akashi callback).
    2. Wraps ``kickoff()`` with ``check()`` before and ``trace()`` after,
       recording the crew's overall output as a single decision.
    3. Delegates all other attribute access to the underlying crew via
       ``__getattr__``.

    Usage::

        traced = AkashiCrew(crew, client, decision_type="research")
        result = traced.kickoff(inputs={"topic": "AI"})

    This is equivalent to combining ``make_hooks`` + ``run_with_akashi``
    but in a single, composable object.
    """

    def __init__(
        self,
        crew: Any,
        client: AkashiSyncClient,
        *,
        decision_type: str = "crew_task",
        confidence: float = 0.7,
        check_before_step: bool = True,
        trace_task_output: bool = True,
    ) -> None:
        self._crew = crew
        self._client = client
        self._decision_type = decision_type
        self._confidence = confidence

        self._hooks = AkashiCrewCallbacks(
            client,
            decision_type=decision_type,
            confidence=confidence,
            check_before_step=check_before_step,
            trace_task_output=trace_task_output,
        )

        self._install_callbacks()

    def _install_callbacks(self) -> None:
        """Read existing callbacks from the crew and chain Akashi's."""
        existing_task_cb = getattr(self._crew, "task_callback", None)
        existing_step_cb = getattr(self._crew, "step_callback", None)

        self._crew.task_callback = _chain_callback(
            existing_task_cb, self._hooks.on_task_complete
        )
        self._crew.step_callback = _chain_callback(
            existing_step_cb, self._hooks.on_step
        )

    def kickoff(self, inputs: dict[str, Any] | None = None) -> Any:
        """Run the crew with Akashi check-before / trace-after.

        Args:
            inputs: Optional keyword inputs forwarded to
                ``crew.kickoff(inputs=...)``.

        Returns:
            The ``CrewOutput`` returned by ``crew.kickoff()``.
        """
        query = _trunc(str(inputs), _MAX_QUERY) if inputs else None
        try:
            self._client.check(self._decision_type, query)
        except Exception as exc:  # noqa: BLE001
            logger.debug("akashi check failed (non-fatal): %s", exc)

        result = self._crew.kickoff(inputs=inputs)

        try:
            self._client.trace(
                TraceRequest(
                    decision_type=self._decision_type,
                    outcome=_trunc(str(result), _MAX_OUTCOME),
                    confidence=self._confidence,
                    metadata={"inputs": str(inputs)[:200] if inputs else ""},
                )
            )
        except Exception as exc:  # noqa: BLE001
            logger.debug("akashi trace failed (non-fatal): %s", exc)

        return result

    def __getattr__(self, name: str) -> Any:
        """Delegate attribute access to the underlying crew."""
        return getattr(self._crew, name)
