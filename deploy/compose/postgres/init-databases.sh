#!/bin/bash
# Creates the fga and river databases beside kb on first start.
set -euo pipefail
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-SQL
  CREATE DATABASE fga OWNER "$POSTGRES_USER";
  CREATE DATABASE river OWNER "$POSTGRES_USER";
  CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
SQL
