#!/usr/bin/env bash
# Thin wrapper for running an update from cron or a systemd timer when the
# `scheduler` compose profile is not used. All logic lives in cmd/updater.
#
#   0 3 * * 0  /srv/map-service/scripts/update-data.sh >> /var/log/map-update.log 2>&1
set -euo pipefail
cd "$(dirname "$0")/.."
exec make update-data
