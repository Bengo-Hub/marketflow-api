#!/bin/sh
set -e

# MF_ROLE picks what this container runs, from the same image:
#   all    (default) migrations, seed, the worker in the background and the API
#   api    migrations, seed and the API only (the worker runs in its own deployment)
#   worker migrations, then the worker in the foreground (marketflow-worker deployment)
# Migrations run in every role: they hold an advisory lock and are idempotent, so whichever
# pod starts first applies them and the worker never runs against an older schema.
MF_ROLE="${MF_ROLE:-all}"
case "$MF_ROLE" in
  all|api|worker) ;;
  *) echo "ERROR: MF_ROLE must be all, api or worker (got '$MF_ROLE')"; exit 1 ;;
esac

# Use direct PostgreSQL URL for migrate/seed to bypass PgBouncer transaction mode.
MIGRATE_URL="${POSTGRES_MIGRATE_URL:-$POSTGRES_URL}"

echo "==> Starting marketflow (role: $MF_ROLE)..."
echo "    APP_ENV=${APP_ENV:-development}"
echo "    HTTP_PORT=${HTTP_PORT:-4000}"

echo "==> Waiting for database and running migrations..."
MAX_RETRIES=60
RETRY_COUNT=0

# Captured (not swallowed) so a real migration failure is visible on every attempt -- the
# liveness probe usually kills this container long before MAX_RETRIES is ever reached.
until MIGRATE_OUTPUT=$(POSTGRES_URL="$MIGRATE_URL" /usr/local/bin/marketflow-migrate 2>&1) || [ $RETRY_COUNT -eq $MAX_RETRIES ]; do
  RETRY_COUNT=$((RETRY_COUNT+1))
  echo "    Migration attempt $RETRY_COUNT/$MAX_RETRIES failed:"
  echo "$MIGRATE_OUTPUT"
  sleep 5
done

if [ $RETRY_COUNT -eq $MAX_RETRIES ]; then
  echo "    ERROR: migration failed after $MAX_RETRIES attempts. Last error:"
  echo "$MIGRATE_OUTPUT"
  exit 1
fi

echo "    Migrations applied"

if [ "$MF_ROLE" = "worker" ]; then
  echo "==> Exec marketflow-worker..."
  exec /usr/local/bin/marketflow-worker
fi

# Run idempotent seed (RBAC perms/roles + KB); non-fatal on warnings.
if [ -x /usr/local/bin/marketflow-seed ]; then
  echo "==> Running seed (idempotent)..."
  POSTGRES_URL="$MIGRATE_URL" /usr/local/bin/marketflow-seed || echo "    seed completed with warnings (non-fatal)"
fi

# role all: the worker in the background (NATS consumers, schedulers, health on :4002), sharing
# this container's env.
if [ "$MF_ROLE" = "all" ] && [ -x /usr/local/bin/marketflow-worker ]; then
  echo "==> Starting marketflow-worker (background)..."
  /usr/local/bin/marketflow-worker &
  WORKER_PID=$!
  echo "    worker PID=$WORKER_PID"
fi

echo "==> Exec marketflow-api server..."
exec /usr/local/bin/marketflow-api
