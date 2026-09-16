#!/usr/bin/env bash
# Pre-pull the base images the E2E Dockerfiles build on, retrying on
# transient registry errors.
#
# `docker build` resolves each `FROM` image against the registry the
# moment it loads the Dockerfile's metadata. A single failed connection
# there (the 2026-09-16 nightly died on an unreachable IPv6 CloudFront
# address for one Docker Hub blob) aborts the whole build and with it the
# lab bring-up. `docker pull` does not retry across attempts either, so
# this script loops over the pull with a backoff until it succeeds or
# the attempts are exhausted. Once the image is in the local store,
# BuildKit's default resolve mode uses it without another registry
# round-trip.
#
# The image list is derived from the Dockerfiles themselves so the pins
# (ARG defaults + FROM lines) stay the single source of truth. Only the
# ARG defaults are honoured — pass `--build-arg` overrides to `make
# e2e-images` yourself if you deviate from them.
#
# Usage: test/e2e/pull-base-images.sh DOCKERFILE...
# Env:   E2E_PULL_ATTEMPTS (default 5), E2E_PULL_BACKOFF seconds (default 5,
#        doubled after every failed attempt).
set -euo pipefail

attempts="${E2E_PULL_ATTEMPTS:-5}"
backoff="${E2E_PULL_BACKOFF:-5}"

if [ "$#" -eq 0 ]; then
  echo "usage: $0 DOCKERFILE..." >&2
  exit 2
fi

# Print the registry images a Dockerfile builds on: every `FROM` whose
# reference is not an earlier stage alias, with `${ARG}` placeholders
# expanded from the file's own `ARG NAME=default` lines.
base_images() {
  awk '
    /^[[:space:]]*ARG[[:space:]]+[A-Za-z_][A-Za-z0-9_]*=/ {
      split($2, kv, "=")
      args[kv[1]] = substr($2, length(kv[1]) + 2)
      next
    }
    /^[[:space:]]*FROM[[:space:]]/ {
      ref = $2
      while (match(ref, /\$\{[A-Za-z_][A-Za-z0-9_]*\}/)) {
        name = substr(ref, RSTART + 2, RLENGTH - 3)
        ref = substr(ref, 1, RSTART - 1) args[name] substr(ref, RSTART + RLENGTH)
      }
      if (!(ref in stages)) print ref
      for (i = 3; i <= NF; i++) if (toupper($i) == "AS") stages[$(i + 1)] = 1
    }
  ' "$1"
}

pull_with_retry() {
  local image="$1" attempt=1 wait="$backoff"
  while :; do
    echo "[pull] $image (attempt $attempt/$attempts)"
    if docker pull --quiet "$image"; then
      return 0
    fi
    if [ "$attempt" -ge "$attempts" ]; then
      echo "[pull] giving up on $image after $attempts attempts" >&2
      return 1
    fi
    echo "[pull] $image failed; retrying in ${wait}s" >&2
    sleep "$wait"
    wait=$((wait * 2))
    attempt=$((attempt + 1))
  done
}

while IFS= read -r image; do
  [ -n "$image" ] || continue
  pull_with_retry "$image"
done < <(for dockerfile in "$@"; do base_images "$dockerfile"; done | sort -u)
