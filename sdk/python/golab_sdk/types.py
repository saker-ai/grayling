"""Wire types mirroring the GoLab evaluation contract (pkg/model)."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict, Optional


@dataclass(frozen=True)
class EvaluationContext:
    """Randomization unit + targeting attributes for one evaluation.

    ``targeting_key`` must be a stable, authenticated or server-generated
    identity (§6); the SDK never invents one. Attribute keys must be
    registered in the flag's ``fields`` allowlist to take part in
    targeting.
    """

    targeting_key: Optional[str] = None
    attributes: Dict[str, Any] = field(default_factory=dict)

    def to_wire(self) -> Dict[str, Any]:
        ctx: Dict[str, Any] = {}
        if self.targeting_key:
            ctx["targetingKey"] = self.targeting_key
        if self.attributes:
            ctx["attributes"] = self.attributes
        return ctx


@dataclass(frozen=True)
class Exposure:
    """Experiment attribution of a result (mirrors model.Exposure)."""

    experiment_id: str
    epoch: int
    variant: str
    source: str  # split | targeting | default

    @classmethod
    def from_wire(cls, raw: Dict[str, Any]) -> "Exposure":
        return cls(
            experiment_id=raw.get("experimentId", ""),
            epoch=int(raw.get("epoch", 0)),
            variant=raw.get("variant", ""),
            source=raw.get("source", ""),
        )


@dataclass(frozen=True)
class Result:
    """One evaluation outcome (mirrors model.Result).

    ``failed`` results carry the caller-provided default in the typed
    variation APIs and must never be interpreted as an assignment.
    """

    flag_key: str = ""
    value: Any = None
    variation_type: str = ""
    reason: str = ""
    error_code: str = ""
    error_details: str = ""
    failed: bool = False
    flag_version: str = ""
    track_events: bool = False
    experiment: Optional[Exposure] = None
    bucket: int = -1
    rule: str = ""
    decision_id: str = ""
    manifest_revision: int = 0
    snapshot_age_millis: int = 0
    exposure_event_id: str = ""

    @classmethod
    def from_wire(cls, raw: Dict[str, Any]) -> "Result":
        exp = raw.get("experiment")
        return cls(
            flag_key=raw.get("flagKey", ""),
            value=raw.get("value"),
            variation_type=raw.get("variationType", ""),
            reason=raw.get("reason", ""),
            error_code=raw.get("errorCode", "") or "",
            error_details=raw.get("errorDetails", "") or "",
            failed=bool(raw.get("failed", False)),
            flag_version=raw.get("flagVersion", "") or "",
            track_events=bool(raw.get("trackEvents", False)),
            experiment=Exposure.from_wire(exp) if exp else None,
            bucket=int(raw.get("bucket", -1)),
            rule=raw.get("rule", "") or "",
            decision_id=raw.get("decisionId", "") or "",
            manifest_revision=int(raw.get("manifestRevision", 0)),
            snapshot_age_millis=int(raw.get("snapshotAgeMillis", 0)),
            exposure_event_id=raw.get("exposureEventId", "") or "",
        )

    @classmethod
    def error(cls, flag_key: str, details: str, error_code: str = "GENERAL") -> "Result":
        """Synthetic failure result for transport/protocol errors — same
        semantics as the Go SDK's cold-start behaviour (§3: 冷启动取快照失败
        时返回调用方默认值/安全变体，并报告原因)."""
        return cls(
            flag_key=flag_key, reason="ERROR", error_code=error_code,
            error_details=details, failed=True, bucket=-1,
        )


@dataclass(frozen=True)
class EvaluationDetails:
    """Value + full result + server-side exposure metadata of one eval."""

    value: Any
    result: Result
    manifest_revision: int = 0
    exposure_event_id: str = ""
    randomization_unit: str = ""
    from_cache: bool = False
