"""NLI sidecar for Akashi conflict detection.

Runs a DeBERTa-v3-base NLI model to classify entailment/contradiction/neutral
between decision outcome pairs. Returns contradiction probability as a float
compatible with the CrossEncoder ``POST /score`` contract.
"""

import logging
import os
import time

import torch
from fastapi import FastAPI, Request, Response
from pydantic import BaseModel
from transformers import AutoModelForSequenceClassification, AutoTokenizer

logger = logging.getLogger("nli")
logging.basicConfig(
    level=os.getenv("LOG_LEVEL", "INFO").upper(),
    format="%(asctime)s %(levelname)s %(name)s: %(message)s",
)

MODEL_NAME = os.getenv("NLI_MODEL", "cross-encoder/nli-deberta-v3-base")
MAX_LENGTH = int(os.getenv("NLI_MAX_LENGTH", "512"))

app = FastAPI(title="Akashi NLI Sidecar", version="1.0.0")

# Globals populated at startup.
tokenizer = None
model = None
label2id: dict[str, int] = {}


@app.on_event("startup")
def load_model() -> None:
    global tokenizer, model, label2id  # noqa: PLW0603
    logger.info("loading NLI model: %s", MODEL_NAME)
    start = time.monotonic()
    tokenizer = AutoTokenizer.from_pretrained(MODEL_NAME)
    model = AutoModelForSequenceClassification.from_pretrained(MODEL_NAME)
    model.eval()

    # Build label index. DeBERTa NLI models use id2label like
    # {0: "CONTRADICTION", 1: "NEUTRAL", 2: "ENTAILMENT"} but ordering
    # varies across checkpoints, so we look it up dynamically.
    id2label = model.config.id2label
    label2id = {v.upper(): int(k) for k, v in id2label.items()}

    elapsed = time.monotonic() - start
    logger.info(
        "model loaded in %.1fs  labels=%s  device=cpu",
        elapsed,
        label2id,
    )


class ScoreRequest(BaseModel):
    text_a: str
    text_b: str


class ScoreResponse(BaseModel):
    score: float


class ClassifyResponse(BaseModel):
    contradiction: float
    neutral: float
    entailment: float


@app.post("/score", response_model=ScoreResponse)
def score(req: ScoreRequest) -> ScoreResponse:
    """Return contradiction probability (CrossEncoder-compatible contract)."""
    probs = _classify(req.text_a, req.text_b)
    return ScoreResponse(score=probs["CONTRADICTION"])


@app.post("/classify", response_model=ClassifyResponse)
def classify(req: ScoreRequest) -> ClassifyResponse:
    """Return full 3-way NLI probabilities for richer downstream use."""
    probs = _classify(req.text_a, req.text_b)
    return ClassifyResponse(
        contradiction=probs["CONTRADICTION"],
        neutral=probs["NEUTRAL"],
        entailment=probs["ENTAILMENT"],
    )


@app.get("/health")
def health(response: Response) -> dict:
    if model is None or tokenizer is None:
        response.status_code = 503
        return {"status": "loading"}
    return {"status": "ok", "model": MODEL_NAME}


@app.middleware("http")
async def log_requests(request: Request, call_next):
    start = time.monotonic()
    response = await call_next(request)
    elapsed_ms = (time.monotonic() - start) * 1000
    if request.url.path not in ("/health",):
        logger.info(
            "%s %s %d %.1fms",
            request.method,
            request.url.path,
            response.status_code,
            elapsed_ms,
        )
    return response


def _classify(text_a: str, text_b: str) -> dict[str, float]:
    """Run NLI inference and return label → probability mapping."""
    inputs = tokenizer(
        text_a,
        text_b,
        return_tensors="pt",
        truncation=True,
        max_length=MAX_LENGTH,
        padding=True,
    )
    with torch.no_grad():
        logits = model(**inputs).logits
    probs = torch.softmax(logits, dim=-1)[0]

    return {
        label: float(probs[idx])
        for label, idx in label2id.items()
    }
