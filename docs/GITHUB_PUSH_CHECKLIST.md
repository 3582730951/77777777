# GitHub Push Checklist

This repository contains a Go gateway, an embedded Vue admin SPA, and the
`services/autoreg` Python/React service. Use this checklist before the final
Windows-side push.

## Keep out of Git

The root `.gitignore` and `.dockerignore` intentionally exclude local runtime
state and secrets:

- `auth.json`
- `.claude/`, `.codex/`
- `.gocache/`, `.gomodcache/`
- `data/`, `**/data/`
- `bin/`, `release/`, root `gateway`
- `**/node_modules/`, `**/.venv/`, `**/.pytest_cache/`, `**/__pycache__/`
- `.env`, `.env.*` except checked-in `.env.example`
- `*.db`, `*.sqlite*`, `*.pem`, `*.key`, `*.crt`, `*.log`

## Expected source/build artifacts to include

These are part of the current tree and should be included when committing:

- Go source and tests under `cmd/`, `internal/`, `tools/`
- Gateway configs under `config/` and `deploy/config/`
- Root admin frontend source under `frontend/`
- Embedded admin build output under `internal/admin/dist/`
- Embedded autoreg SPA build output under `internal/admin/autoreg_spa/`
- `services/autoreg/` source, frontend, tests, Docker files, and docs
- Documentation under `docs/`, `plan/`, and `security/`

## Verification commands

From the repository root:

```bash
# Go 1.25 is required by go.mod. If local Windows Go is older, use Docker.
docker run --rm -v "%cd%:/workspace" -w /workspace ^
  -e GOCACHE=/workspace/.gocache ^
  -e GOMODCACHE=/workspace/.gomodcache ^
  golang:1.25-bookworm go test ./...

cd frontend
npm run build

cd ..\services\autoreg\frontend
npm run build

cd ..\..\..
python -m pytest services/autoreg/tests
```

PowerShell equivalent for the Docker test:

```powershell
docker run --rm -v "${PWD}:/workspace" -w /workspace `
  -e GOCACHE=/workspace/.gocache `
  -e GOMODCACHE=/workspace/.gomodcache `
  golang:1.25-bookworm go test ./...
```

## Final Windows commit/push

```powershell
git status --short
git add -A
git status --short
git commit -m "chore: prepare gateway project for GitHub"
git push origin main
```

If you want to inspect only files that will be committed before `git add -A`:

```powershell
git diff --stat
git ls-files --others --exclude-standard
```
