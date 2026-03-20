FROM node:22-alpine

RUN apk add --no-cache mosquitto mosquitto-clients supervisor

WORKDIR /app

# Install Node dependencies
COPY package.json package-lock.json ./
RUN npm ci --production

# Copy application
COPY server.js db.js packet-store.js config.example.json ./
COPY public/ ./public/

# Supervisor config
COPY docker/supervisord.conf /etc/supervisor/conf.d/supervisord.conf
COPY docker/mosquitto.conf /etc/mosquitto/mosquitto.conf

# Create data directory for SQLite + Mosquitto persistence
RUN mkdir -p /app/data /var/lib/mosquitto && \
    chown -R node:node /app/data && \
    chown -R mosquitto:mosquitto /var/lib/mosquitto

# Default config: copy example if no config mounted
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 3000 1883

VOLUME ["/app/data"]

ENTRYPOINT ["/entrypoint.sh"]
