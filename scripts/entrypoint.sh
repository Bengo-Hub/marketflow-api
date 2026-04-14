#!/bin/sh
set -e

echo "==> Starting marketflow-api..."
echo "    APP_ENV=${APP_ENV:-development}"
echo "    HTTP_PORT=${HTTP_PORT:-4000}"

# Run idempotent seed (RBAC perms/roles + KB); non-fatal on warnings.
if [ -x /usr/local/bin/marketflow-seed ]; then
  echo "==> Running seed (idempotent)..."
  /usr/local/bin/marketflow-seed || echo "    seed completed with warnings (non-fatal)"
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
