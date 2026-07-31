#!/bin/sh
set -eu

mkdir -p /app/data
chown -R app:app /app/data

exec su-exec app /usr/local/bin/sub2api-accountinfo
