#!/bin/sh
set -e

# Use direct PostgreSQL URL for migrate/seed to bypass PgBouncer transaction mode.
MIGRATE_URL="${POSTGRES_MIGRATE_URL:-$POSTGRES_URL}"

echo "==> Starting marketflow-api..."
echo "    APP_ENV=${APP_ENV:-development}"
echo "    HTTP_PORT=${HTTP_PORT:-4000}"

echo "==> Waiting for database and running migrations..."
MAX_RETRIES=60
RETRY_COUNT=0

until POSTGRES_URL="$MIGRATE_URL" /usr/local/bin/marketflow-migrate > /dev/null 2>&1 || [ $RETRY_COUNT -eq $MAX_RETRIES ]; do
  RETRY_COUNT=$((RETRY_COUNT+1))
  echo "    DB not ready... (attempt $RETRY_COUNT/$MAX_RETRIES)"
  sleep 5
done

if [ $RETRY_COUNT -eq $MAX_RETRIES ]; then
  echo "    ERROR: DB not ready after $MAX_RETRIES attempts"
  exit 1
fi

echo "    Migrations applied"

# Run idempotent seed (RBAC perms/roles + KB); non-fatal on warnings.
if [ -x /usr/local/bin/marketflow-seed ]; then
  echo "==> Running seed (idempotent)..."
  POSTGRES_URL="$MIGRATE_URL" /usr/local/bin/marketflow-seed || echo "    seed completed with warnings (non-fatal)"
fi

# Launch the worker in the background (NATS consumer + health endpoint on :4002).
# Shares the same image, same DB connection pool settings, same env.
if [ -x /usr/local/bin/marketflow-worker ]; then
  echo "==> Starting marketflow-worker (background)..."
  /usr/local/bin/marketflow-worker &
  WORKER_PID=$!
  echo "    worker PID=$WORKER_PID"
fi

echo "==> Exec marketflow-api server..."
exec /usr/local/bin/marketflow-api
