#!/bin/sh
# Applies each service's SQL migrations to its own database, once, idempotently.
#
# The services do not migrate themselves (the distroless images carry only the binary), and pgtest
# applies migrations for tests — but a running compose stack has nothing to create the schema. This
# one-shot fills that gap for local dev. It is deliberately minimal: a real migration tool with
# down-migrations and checksums is an M5 concern, not a dev-compose one.
#
# Idempotent: a per-database _compose_migrations ledger records applied files, so `docker compose
# up` is safe to run repeatedly against a persistent volume. Each file is applied in a single
# transaction (-1) with ON_ERROR_STOP, so a bad migration rolls back instead of leaving a half-DB.
set -eu

: "${POSTGRES_USER:?POSTGRES_USER is required}"
: "${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}"
PGHOST="${POSTGRES_HOST:-postgres}"

# Data ownership (root rule #6): each service's migrations touch only its own database.
SERVICES="catalog availability booking payment notification"

for svc in $SERVICES; do
  url="postgresql://${POSTGRES_USER}:${POSTGRES_PASSWORD}@${PGHOST}:5432/${svc}?sslmode=disable"
  dir="/repo/services/${svc}/migrations"

  if [ ! -d "$dir" ]; then
    echo "[$svc] no migrations directory — skipping"
    continue
  fi

  psql "$url" -v ON_ERROR_STOP=1 -q -c \
    "CREATE TABLE IF NOT EXISTS _compose_migrations (name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now());"

  for f in "$dir"/*.sql; do
    [ -e "$f" ] || continue
    base="$(basename "$f")"
    if [ "$(psql "$url" -tAc "SELECT 1 FROM _compose_migrations WHERE name = '$base';")" = "1" ]; then
      echo "[$svc] $base — already applied"
      continue
    fi
    echo "[$svc] applying $base"
    psql "$url" -v ON_ERROR_STOP=1 -q -1 -f "$f"
    psql "$url" -v ON_ERROR_STOP=1 -q -c "INSERT INTO _compose_migrations(name) VALUES ('$base');"
  done
done

echo "migrations complete"
