FROM node:22-bookworm-slim AS autoreg-spa
WORKDIR /src/services/autoreg/frontend
COPY services/autoreg/frontend/package*.json ./
RUN npm ci
COPY services/autoreg/frontend/ ./
RUN VITE_API_BASE=/api/autoreg npx vite build --base=/autoreg/ --outDir /out/autoreg_spa

# --- Go build stage ---
FROM golang:1.25-alpine AS go-build
WORKDIR /src
COPY go.mod go.sum ./
ENV GOTOOLCHAIN=auto
RUN go mod download
COPY . .
COPY --from=autoreg-spa /out/autoreg_spa ./internal/admin/autoreg_spa
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

# --- Python autoreg stage ---
FROM python:3.12-slim AS py-build
WORKDIR /autoreg
COPY services/autoreg/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt 2>/dev/null || pip install --no-cache-dir fastapi uvicorn sqlmodel pydantic
RUN mkdir -p /root/.cache/ms-playwright
COPY services/autoreg/ .

# --- runtime stage ---
FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl git jq nodejs npm python3-venv rsync tar tzdata unzip wget \
    && rm -rf /var/lib/apt/lists/*

COPY --from=go-build /out/gateway /usr/local/bin/gateway
COPY --from=go-build /src /workspace
COPY --from=py-build /autoreg /opt/autoreg
COPY --from=py-build /usr/local/lib/python3.12/site-packages /usr/local/lib/python3.12/site-packages
COPY --from=py-build /root/.cache/ms-playwright /root/.cache/ms-playwright
COPY config/config.example.yaml /etc/llm-pool/config.example.yaml

WORKDIR /workspace
VOLUME ["/data", "/config"]
EXPOSE 8787 8788

ENV AUTOREG_PORT=9900
ENTRYPOINT ["/usr/local/bin/gateway", "-config", "/config/config.yaml"]
