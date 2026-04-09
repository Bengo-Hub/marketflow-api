#!/bin/sh
set -e

echo "==> Starting marketflow-api..."
echo "    APP_ENV=${APP_ENV:-development}"
echo "    HTTP_PORT=${HTTP_PORT:-4000}"

exec /usr/local/bin/marketflow-api
