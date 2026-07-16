#!/bin/bash
# Creates one database per service so each service owns its data exclusively
# (logical isolation in dev; prod uses separate Postgres instances/accounts).
# Reads a comma-separated list from $SERVICE_DATABASES.
set -euo pipefail

if [ -z "${SERVICE_DATABASES:-}" ]; then
  echo "SERVICE_DATABASES not set; nothing to create."
  exit 0
fi

IFS=',' read -ra DBS <<< "$SERVICE_DATABASES"
for db in "${DBS[@]}"; do
  db_trimmed="$(echo "$db" | xargs)"
  echo "Creating database: $db_trimmed"
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" <<-EOSQL
    SELECT 'CREATE DATABASE "$db_trimmed"'
    WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '$db_trimmed')\gexec
EOSQL
done
