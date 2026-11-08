FROM ruby:3.3-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl procps build-essential \
 && gem install --no-document puma rack \
 && apt-get purge -y --auto-remove build-essential \
 && rm -rf /var/lib/apt/lists/*
COPY app /srv/myapp_prod
COPY app /srv/myapp_staging
# Staging runs without control app: the check falls back on process checks.
RUN sed -i '/activate_control_app/d' /srv/myapp_staging/config/puma.rb \
 && mkdir -p /srv/myapp_prod/tmp/pids /srv/myapp_staging/tmp/pids
COPY check-puma /usr/local/bin/check-puma
CMD ["sleep", "infinity"]
