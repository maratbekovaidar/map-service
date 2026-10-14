#!/usr/bin/env bash
# Thin pre-flight check for a fresh server; prints what is missing and exits non-zero.
set -uo pipefail
cd "$(dirname "$0")/.."
bad=0
need() { command -v "$1" >/dev/null 2>&1 || { echo "MISSING: $1 ($2)"; bad=1; }; }
need docker "https://docs.docker.com/engine/install/"
need make "apt install make"
docker compose version >/dev/null 2>&1 || { echo "MISSING: the Docker Compose v2 plugin"; bad=1; }
docker info >/dev/null 2>&1 || { echo "Docker daemon is not reachable for this user"; bad=1; }
[ -f .env ] || { echo "MISSING: .env (cp .env.example .env and edit it)"; bad=1; }
if [ -r /proc/meminfo ]; then
  mem_gb=$(awk '/MemTotal/ {printf "%d", $2/1024/1024}' /proc/meminfo)
  [ "$mem_gb" -ge 7 ] || { echo "WARNING: ${mem_gb} GB RAM detected, 8 GB are recommended"; }
fi
free_gb=$(df -Pk . | awk 'NR==2 {printf "%d", $4/1024/1024}')
[ "$free_gb" -ge 20 ] || echo "WARNING: only ${free_gb} GB free here, plan for at least 20 GB"
[ "$bad" -eq 0 ] && echo "prerequisites look fine"
exit "$bad"
