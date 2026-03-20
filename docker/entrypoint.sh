#!/bin/sh

# Copy example config if no config.json exists
if [ ! -f /app/config.json ]; then
  echo "[entrypoint] No config.json found, copying from config.example.json"
  cp /app/config.example.json /app/config.json
fi

exec /usr/bin/supervisord -c /etc/supervisor/conf.d/supervisord.conf
