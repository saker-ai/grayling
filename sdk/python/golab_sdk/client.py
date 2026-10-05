"""GolabClient — remote evaluation with a revision-guarded cache, polling
freshness and batched collect-contract event reporting.

Design (docs/技术方案.md §8, §10.1):

* Every evaluation is a ``POST /api/v1/sdk/eval``. The response carries the
  current ``manifestRevision``; when an evaluation enrolls the unit in a
  running experiment the SERVER records the exposure itself (deterministic,
  idempotent event ID) and echoes ``randomizationUnit`` so conversions land
  on the same unit hash.
* Results are cached per (flagKey, canonical context). A cache entry is
  valid only while it matches the client's current revision view; a
  background poller probes ``GET /api/v1/sdk/revision`` (restricted
  projection — revision number only, never flag content) every
  ``poll_interval`` seconds and invalidates the cache when the substrate
  moved. Eval responses also update the revision view, so an active client
  converges even if polling fails.
* Conversion/manual-exposure events are queued and flushed in batches to
  ``POST /api/v1/collect`` with the separate collect credential (§9 三受众
  凭证分离). Failures retry once, then drop with a counter — event
  reporting must never block or crash evaluation.
* On any transport/protocol error the typed variation methods return the
  CALLER'S DEFAULT with a failed Result (reason=ERROR), never a guess.
"""

from __future__ import annotations

import json
import queue
import threading
import time
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any, Callable, Dict, Optional, Tuple
from urllib import error as urlerror
from urllib import request as urlrequest

from .types import EvaluationContext, EvaluationDetails, Result


class GolabError(Exception):
    """Raised by explicit operations (sync, flush) on transport/HTTP failure.

    Variation methods never raise; they return the caller's default with a
    failed Result instead.
    """


@dataclass
class Options:
    """Client configuration. Durations are seconds (float)."""

    base_url: str = ""
    sdk_token: str = ""
    collect_token: str = ""          # empty → event reporting disabled (counted as dropped)
    poll_interval: float = 30.0      # revision probe cadence
    request_timeout: float = 10.0    # per-request HTTP timeout
    cache_max_entries: int = 10_000  # LRU bound on the evaluation cache
    max_staleness: float = 300.0
    exposure_flush_interval: float = 3.0
    exposure_batch_size: int = 100
    exposure_queue_cap: int = 10_000
    auto_poll: bool = True           # start the poller thread on first use
    on_error: Optional[Callable[[str, Exception], None]] = None


@dataclass
class _CacheEntry:
    details: EvaluationDetails
    revision: int
    created_at: float


@dataclass
class _Stats:
    evals: int = 0
    cache_hits: int = 0
    cache_misses: int = 0
    eval_errors: int = 0
    events_queued: int = 0
    events_flushed: int = 0
    events_dropped: int = 0
    revision_syncs: int = 0
    last_error: str = ""


def _utc_now_rfc3339() -> str:
    return (
        datetime.now(timezone.utc)
        .isoformat(timespec="milliseconds")
        .replace("+00:00", "Z")
    )


def _canonical_context(ctx: EvaluationContext) -> str:
    return json.dumps(ctx.to_wire(), sort_keys=True, separators=(",", ":"),
                      ensure_ascii=False, default=str)


class GolabClient:
    """Thread-safe GoLab remote-evaluation client."""

    def __init__(self, options: Options):
        if not options.base_url or not options.sdk_token:
            raise ValueError("golab: base_url and sdk_token are required")
        self._opts = options
        self._base = options.base_url.rstrip("/")
        self._lock = threading.Lock()
        self._cache: "Dict[Tuple[str, str], _CacheEntry]" = {}
        self._revision: Optional[int] = None  # None = unknown (nothing synced yet)
        self._last_success = 0.0
        self._stats = _Stats()
        self._events: "queue.Queue[Dict[str, Any]]" = queue.Queue(
            maxsize=options.exposure_queue_cap)
        self._stop = threading.Event()
        self._poller: Optional[threading.Thread] = None
        self._flusher: Optional[threading.Thread] = None
        self._closed = False
        if options.auto_poll:
            self._start_poller()
        self._ensure_flusher()

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    def bool_variation(self, flag_key: str, default: bool = False, *,
                       targeting_key: Optional[str] = None,
                       attributes: Optional[Dict[str, Any]] = None
                       ) -> Tuple[bool, EvaluationDetails]:
        details = self._evaluate(flag_key, targeting_key, attributes)
        v = details.value
        if details.result.failed or not isinstance(v, bool):
            if not details.result.failed and not isinstance(v, bool):
                details = self._type_mismatch(flag_key, details, "bool")
            return default, details
        return v, details

    def string_variation(self, flag_key: str, default: str = "", *,
                         targeting_key: Optional[str] = None,
                         attributes: Optional[Dict[str, Any]] = None
                         ) -> Tuple[str, EvaluationDetails]:
        details = self._evaluate(flag_key, targeting_key, attributes)
        v = details.value
        if details.result.failed or not isinstance(v, str):
            if not details.result.failed and not isinstance(v, str):
                details = self._type_mismatch(flag_key, details, "string")
            return default, details
        return v, details

    def number_variation(self, flag_key: str, default: float = 0.0, *,
                         targeting_key: Optional[str] = None,
                         attributes: Optional[Dict[str, Any]] = None
                         ) -> Tuple[float, EvaluationDetails]:
        details = self._evaluate(flag_key, targeting_key, attributes)
        v = details.value
        if details.result.failed or isinstance(v, bool) or not isinstance(v, (int, float)):
            if not details.result.failed and (isinstance(v, bool) or not isinstance(v, (int, float))):
                details = self._type_mismatch(flag_key, details, "number")
            return default, details
        return float(v), details

    def json_variation(self, flag_key: str, default: Any = None, *,
                       targeting_key: Optional[str] = None,
                       attributes: Optional[Dict[str, Any]] = None
                       ) -> Tuple[Any, EvaluationDetails]:
        details = self._evaluate(flag_key, targeting_key, attributes)
        if details.result.failed:
            return default, details
        return details.value, details

    def track_conversion(self, details: EvaluationDetails, metric_key: str,
                         value: float = 1.0, *, session_id: str = "",
                         task_id: str = "") -> None:
        """Report a conversion attributed to the experiment of a prior
        evaluation. The unit is the randomization unit the SERVER echoed for
        the exposure, keeping attribution on the identical unit hash
        (§10.1). Raises GolabError when the result carries no attribution or
        no collect token is configured."""
        res = details.result
        if res.experiment is None:
            raise GolabError(
                f"result carries no experiment attribution (reason {res.reason})")
        if not self._opts.collect_token:
            raise GolabError("collect token not configured")
        unit = details.randomization_unit
        if not unit:
            raise GolabError(
                "cannot derive randomization unit (server recorded no exposure)")
        flag_revision = 0
        if details.result.flag_version.isdigit():
            flag_revision = int(details.result.flag_version)
        self._enqueue({
            "eventId": f"conv-{uuid.uuid4().hex}",
            "decisionId": res.decision_id,
            "type": "conversion",
            "experimentId": res.experiment.experiment_id,
            "epoch": res.experiment.epoch,
            "flagKey": res.flag_key,
            "flagRevision": flag_revision,
            "variant": res.experiment.variant,
            "unit": unit,
            "source": res.experiment.source,
            "metricKey": metric_key,
            "value": float(value),
            "sessionId": session_id,
            "taskId": task_id,
            "clientTime": _utc_now_rfc3339(),
        })

    def track_exposure(self, experiment_id: str, epoch: int, flag_key: str,
                       variant: str, unit: str, *, source: str = "split",
                       flag_revision: int = 0) -> None:
        """Manually report an exposure aligned with the collect contract.

        NOT needed for remote evaluations — the server records those itself
        with an idempotent event ID. Use this only when the assignment came
        from elsewhere (e.g. an edge decision) and the exposure must be
        backfilled; duplicate units never inflate unique-unit analytics.
        """
        if not self._opts.collect_token:
            raise GolabError("collect token not configured")
        if not unit:
            raise GolabError("exposure without a randomization unit is rejected "
                             "(no attribution without a unit)")
        self._enqueue({
            "eventId": f"exp-{uuid.uuid4().hex}",
            "type": "exposure",
            "experimentId": experiment_id,
            "epoch": epoch,
            "flagKey": flag_key,
            "flagRevision": flag_revision,
            "variant": variant,
            "unit": unit,
            "source": source,
            "clientTime": _utc_now_rfc3339(),
        })

    def sync_revision(self) -> int:
        """Probe /api/v1/sdk/revision and invalidate the cache on change.
        Runs automatically on the poll interval; exposed for tests and
        explicit freshness checks. Raises GolabError on failure."""
        try:
            body = self._http_json("GET", "/api/v1/sdk/revision",
                                   token=self._opts.sdk_token)
        except Exception:
            with self._lock:
                self._stats.last_error = "revision probe failed"
            raise
        revision = int(body.get("revision", 0))
        with self._lock:
            changed = self._revision is not None and revision != self._revision
            self._revision = revision
            self._last_success = time.monotonic()
            self._stats.last_error = ""
            self._stats.revision_syncs += 1
            if changed:
                self._invalidate_locked()
        return revision

    def diagnose(self, decision_id: str) -> Dict[str, Any]:
        """Look up a recent decision within this SDK token's scope."""
        return self._http_json("GET", f"/api/v1/sdk/decisions/{decision_id}",
                               token=self._opts.sdk_token)

    def flush(self, timeout: float = 10.0) -> int:
        """Synchronously drain queued events to /api/v1/collect.
        Returns the number of events posted. Raises GolabError on failure."""
        batch = self._drain(self._opts.exposure_batch_size * 16)
        if not batch:
            return 0
        deadline = time.monotonic() + timeout
        while True:
            try:
                self._post_events(batch)
                with self._lock:
                    self._stats.events_flushed += len(batch)
                return len(batch)
            except Exception:
                if time.monotonic() >= deadline:
                    # Re-queue what we could not deliver, then fail loudly.
                    for ev in batch:
                        self._enqueue(ev)
                    raise GolabError("flush failed within timeout")
                time.sleep(0.2)

    def status(self) -> Dict[str, Any]:
        with self._lock:
            st = self._stats
            state = ("STALE" if self._revision is not None and
                     self._last_success and
                     time.monotonic() - self._last_success > self._opts.max_staleness
                     else "ERROR" if st.last_error else
                     "NOT_READY" if self._revision is None else "READY")
            return {
                "state": state,
                "revision": self._revision,
                "cacheSize": len(self._cache),
                "evals": st.evals,
                "cacheHits": st.cache_hits,
                "cacheMisses": st.cache_misses,
                "evalErrors": st.eval_errors,
                "eventsQueued": self._events.qsize(),
                "eventsFlushed": st.events_flushed,
                "eventsDropped": st.events_dropped,
                "revisionSyncs": st.revision_syncs,
                "lastError": st.last_error,
                "closed": self._closed,
            }

    def close(self) -> None:
        """Stop background threads and flush pending events once."""
        if self._closed:
            return
        self._closed = True
        self._stop.set()
        for th in (self._poller, self._flusher):
            if th is not None and th.is_alive():
                th.join(timeout=5)
        try:
            self.flush(timeout=5)
        except GolabError:
            pass

    def __enter__(self) -> "GolabClient":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()

    # ------------------------------------------------------------------
    # Internals
    # ------------------------------------------------------------------

    def _start_poller(self) -> None:
        if self._poller is None:
            self._poller = threading.Thread(
                target=self._poll_loop, name="golab-poll", daemon=True)
            self._poller.start()

    def _ensure_flusher(self) -> None:
        if self._flusher is None and self._opts.collect_token:
            self._flusher = threading.Thread(
                target=self._flush_loop, name="golab-flush", daemon=True)
            self._flusher.start()

    def _evaluate(self, flag_key: str, targeting_key: Optional[str],
                  attributes: Optional[Dict[str, Any]]) -> EvaluationDetails:
        if self._closed:
            return EvaluationDetails(value=None, result=Result.error(
                flag_key, "client is closed"))
        self._ensure_flusher()
        ctx = EvaluationContext(targeting_key=targeting_key,
                                attributes=dict(attributes or {}))
        cache_key = (flag_key, _canonical_context(ctx))

        with self._lock:
            self._stats.evals += 1
            known = self._revision
            entry = self._cache.get(cache_key)
            stale = (self._last_success and time.monotonic() - self._last_success >
                     self._opts.max_staleness)
            if entry is not None and not stale and (known is None or entry.revision == known):
                self._stats.cache_hits += 1
                cached = EvaluationDetails(
                    value=entry.details.value,
                    result=entry.details.result,
                    manifest_revision=entry.details.manifest_revision,
                    exposure_event_id=entry.details.exposure_event_id,
                    randomization_unit=entry.details.randomization_unit,
                    from_cache=True,
                )
                return cached
            self._stats.cache_misses += 1

        try:
            body = self._http_json(
                "POST", "/api/v1/sdk/eval",
                token=self._opts.sdk_token,
                payload={"flagKey": flag_key, "context": ctx.to_wire()})
        except Exception as exc:  # transport/HTTP/parse — caller default
            with self._lock:
                self._stats.eval_errors += 1
                self._stats.last_error = f"eval {flag_key}: {exc}"
            if self._opts.on_error:
                self._opts.on_error("eval", exc)
            return EvaluationDetails(value=None, result=Result.error(
                flag_key, f"remote evaluation failed: {exc}"))

        result = Result.from_wire(body.get("result") or {})
        details = EvaluationDetails(
            value=body.get("value"),
            result=result,
            manifest_revision=int(body.get("manifestRevision", 0)),
            exposure_event_id=body.get("exposureEventId", "") or "",
            randomization_unit=body.get("randomizationUnit", "") or "",
        )

        with self._lock:
            self._last_success = time.monotonic()
            self._stats.last_error = ""
            # The eval response is itself a revision observation: newer than
            # our view → invalidate everything computed against older config.
            if self._revision is None or details.manifest_revision > self._revision:
                if self._revision is not None:
                    self._invalidate_locked()
                self._revision = details.manifest_revision
            self._cache[cache_key] = _CacheEntry(
                details=details, revision=details.manifest_revision,
                created_at=time.time())
            if len(self._cache) > self._opts.cache_max_entries:
                # Evict oldest-created entries (bounded memory, §9 限额).
                ordered = sorted(self._cache.items(),
                                 key=lambda kv: kv[1].created_at)
                for k, _ in ordered[: len(self._cache) - self._opts.cache_max_entries]:
                    self._cache.pop(k, None)
        return details

    def _type_mismatch(self, flag_key: str, details: EvaluationDetails,
                       want: str) -> EvaluationDetails:
        got = type(details.value).__name__
        res = Result(
            flag_key=flag_key, value=None,
            variation_type="",
            reason="ERROR", error_code="TYPE_MISMATCH",
            error_details=f"flag value is {got}, want {want}",
            failed=True, flag_version=details.result.flag_version,
            track_events=False, experiment=None, bucket=details.result.bucket,
            rule=details.result.rule, decision_id=details.result.decision_id,
            manifest_revision=details.result.manifest_revision,
            snapshot_age_millis=details.result.snapshot_age_millis,
            exposure_event_id=details.result.exposure_event_id)
        return EvaluationDetails(value=details.value, result=res,
                                 manifest_revision=details.manifest_revision,
                                 exposure_event_id=details.exposure_event_id,
                                 randomization_unit=details.randomization_unit)

    def _invalidate_locked(self) -> None:
        self._cache.clear()

    def _poll_loop(self) -> None:
        while not self._stop.is_set():
            try:
                self.sync_revision()
            except Exception as exc:
                with self._lock:
                    self._stats.last_error = f"revision poll: {exc}"
                if self._opts.on_error:
                    self._opts.on_error("poll", exc)
            self._stop.wait(self._opts.poll_interval)

    def _flush_loop(self) -> None:
        while not self._stop.is_set():
            self._stop.wait(self._opts.exposure_flush_interval)
            self._flush_once()

    def _flush_once(self) -> None:
        batch = self._drain(self._opts.exposure_batch_size)
        if not batch:
            return
        try:
            self._post_events(batch)
        except Exception:
            # One retry after a short pause (Go SDK parity), then drop with
            # a counter — events must never accumulate without bound.
            time.sleep(0.5)
            try:
                self._post_events(batch)
            except Exception as exc:
                with self._lock:
                    self._stats.events_dropped += len(batch)
                    self._stats.last_error = f"collect flush: {exc}"
                if self._opts.on_error:
                    self._opts.on_error("collect", exc)
                return
        with self._lock:
            self._stats.events_flushed += len(batch)

    def _drain(self, max_events: int) -> "list[Dict[str, Any]]":
        batch: "list[Dict[str, Any]]" = []
        while len(batch) < max_events:
            try:
                batch.append(self._events.get_nowait())
            except queue.Empty:
                break
        return batch

    def _enqueue(self, event: Dict[str, Any]) -> None:
        try:
            self._events.put_nowait(event)
            with self._lock:
                self._stats.events_queued += 1
        except queue.Full:
            with self._lock:
                self._stats.events_dropped += 1

    def _post_events(self, batch: "list[Dict[str, Any]]") -> Dict[str, Any]:
        return self._http_json("POST", "/api/v1/collect",
                               token=self._opts.collect_token,
                               payload={"events": batch})

    def _http_json(self, method: str, path: str, *, token: str,
                   payload: Optional[Dict[str, Any]] = None) -> Dict[str, Any]:
        data = None
        headers = {
            "Authorization": f"Bearer {token}",
            "Accept": "application/json",
        }
        if payload is not None:
            data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
            headers["Content-Type"] = "application/json"
        req = urlrequest.Request(self._base + path, data=data,
                                 headers=headers, method=method)
        try:
            with urlrequest.urlopen(req, timeout=self._opts.request_timeout) as resp:
                raw = resp.read()
        except urlerror.HTTPError as exc:
            detail = ""
            try:
                detail = exc.read().decode("utf-8", "replace")[:512]
            except Exception:
                pass
            raise GolabError(f"{method} {path} → HTTP {exc.code}: {detail}") from exc
        if not raw:
            return {}
        try:
            out = json.loads(raw)
        except ValueError as exc:
            raise GolabError(f"{method} {path}: invalid JSON response") from exc
        if not isinstance(out, dict):
            raise GolabError(f"{method} {path}: unexpected response shape")
        return out
