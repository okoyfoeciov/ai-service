#!/usr/bin/env bash
# run.sh — start the llm-service backend in the foreground. The service that
# install.sh writes runs this: a launchd LaunchAgent (com.llm-service.backend)
# on macOS, a systemd user unit (llm-service.service) on Linux. To run it by
# hand, stop the service first: both want port 8770.
#
# llm-service is the localhost LLM backend (127.0.0.1:8770) for the comart
# Chrome extension and Cairn's Memoir tab. Nothing fronts it, so loopback is the
# trust boundary.
#
# This script loads llm-service.env with bash, so that file must stay plain
# `KEY=value` lines. Put single quotes around a value with spaces, `$` or
# parentheses in it. A bad line stops the server at start.
#
# CACHE_DB defaults relative to this directory, so moving the folder needs no
# edit here — only a re-run of install.sh, because the service definition
# carries absolute paths.
set -eu

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$HERE"

if [ -f ./llm-service.env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./llm-service.env
  set +a
fi

export CACHE_DB="${CACHE_DB:-$HERE/data/cache.db}"

if [ ! -x "$HERE/bin/llm-service" ]; then
  echo "run.sh: $HERE/bin/llm-service is missing — run $HERE/install.sh to build it" >&2
  exit 1
fi

exec "$HERE/bin/llm-service"
