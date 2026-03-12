FROM python:3.12-slim AS base

WORKDIR /app

# Install dependencies first for layer caching.
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Download model weights at build time so the container starts fast.
ARG NLI_MODEL=cross-encoder/nli-deberta-v3-base
RUN python -c "from transformers import AutoTokenizer, AutoModelForSequenceClassification; \
    AutoTokenizer.from_pretrained('${NLI_MODEL}'); \
    AutoModelForSequenceClassification.from_pretrained('${NLI_MODEL}')"

COPY app.py .

ENV NLI_MODEL=${NLI_MODEL}
EXPOSE 5000

HEALTHCHECK --interval=10s --timeout=5s --retries=3 --start-period=30s \
    CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:5000/health')" || exit 1

CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "5000", "--workers", "1"]
