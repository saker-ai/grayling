"""golab_sdk — the GoLab Python SDK (remote-evaluation first version).

Contract summary (docs/技术方案.md §8 受限投影):

* Evaluations call ``POST /api/v1/sdk/eval`` on the GoLab server (or a
  ``cmd/golab-proxy`` relay). The full configuration snapshot — internal
  rules, segments and AI content — is NEVER downloaded by this SDK, so it
  is safe for script/edge/browser-adjacent runtimes.
* Freshness uses the restricted projection ``GET /api/v1/sdk/revision``
  (manifest revision only): a background poller keeps the local evaluation
  cache honest without ever pulling flag content.
* Exposures of remote evaluations are recorded SERVER-side with a
  deterministic, idempotent event ID. Conversions (and any manual
  exposures) are batched to ``POST /api/v1/collect`` with a separate
  collect credential, matching the collect contract exactly.
"""

from .client import GolabClient, GolabError, Options
from .types import EvaluationContext, EvaluationDetails, Exposure, Result

__all__ = [
    "GolabClient",
    "GolabError",
    "Options",
    "EvaluationContext",
    "EvaluationDetails",
    "Exposure",
    "Result",
]

__version__ = "0.1.0"
