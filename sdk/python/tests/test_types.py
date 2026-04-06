"""Tests for Pydantic model validation in akashi.types."""

from __future__ import annotations

import pytest
from pydantic import ValidationError

from akashi.types import TraceEvidence


class TestTraceEvidenceValidation:
    """TraceEvidence.content is conditionally required based on source_type."""

    @pytest.mark.parametrize(
        "source_type",
        ["document", "api_response", "user_input", "search_result", "tool_output", "memory"],
    )
    def test_non_metrics_requires_content(self, source_type: str) -> None:
        with pytest.raises(ValidationError, match="content is required"):
            TraceEvidence(source_type=source_type)

    @pytest.mark.parametrize(
        "source_type",
        ["document", "api_response", "user_input", "search_result", "tool_output", "memory"],
    )
    def test_non_metrics_rejects_empty_content(self, source_type: str) -> None:
        with pytest.raises(ValidationError, match="content is required"):
            TraceEvidence(source_type=source_type, content="")

    @pytest.mark.parametrize(
        "source_type",
        ["document", "api_response", "user_input", "search_result", "tool_output", "memory"],
    )
    def test_non_metrics_accepts_content(self, source_type: str) -> None:
        ev = TraceEvidence(source_type=source_type, content="some evidence text")
        assert ev.content == "some evidence text"
        assert ev.source_type == source_type

    def test_non_metrics_rejects_metrics_field(self) -> None:
        with pytest.raises(ValidationError, match="metrics field is only allowed"):
            TraceEvidence(
                source_type="document",
                content="has content",
                metrics={"latency_ms": 42.0},
            )

    def test_metrics_requires_metrics_field(self) -> None:
        with pytest.raises(ValidationError, match="metrics field is required"):
            TraceEvidence(source_type="metrics")

    def test_metrics_rejects_empty_metrics(self) -> None:
        with pytest.raises(ValidationError, match="metrics field is required"):
            TraceEvidence(source_type="metrics", metrics={})

    def test_metrics_allows_empty_content(self) -> None:
        ev = TraceEvidence(source_type="metrics", metrics={"latency_ms": 42.0})
        assert ev.content == ""
        assert ev.metrics == {"latency_ms": 42.0}

    def test_metrics_allows_content(self) -> None:
        ev = TraceEvidence(
            source_type="metrics",
            content="optional description",
            metrics={"latency_ms": 42.0},
        )
        assert ev.content == "optional description"

    def test_optional_fields_default(self) -> None:
        ev = TraceEvidence(source_type="document", content="text")
        assert ev.source_uri is None
        assert ev.relevance_score is None
        assert ev.metrics is None
