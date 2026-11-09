FROM ruby:3.3.13-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl procps build-essential \
 && gem install --no-document puma -v 8.0.2 \
 && gem install --no-document rack -v 3.2.8 \
 && apt-get purge -y --auto-remove build-essential \
 && rm -rf /var/lib/apt/lists/*
# Deployment directories do not match the Puma tags (myapp_prod, myapp_staging).
COPY app /srv/myapp-production
COPY app /srv/myapp-staging
# Stale state file of a removed app: its PID is no running master, it must be ignored.
RUN mkdir -p /srv/myapp-production/tmp/pids /srv/myapp-staging/tmp/pids /srv/oldapp/tmp/pids \
 && printf -- '---\npid: 4242\ncontrol_url: tcp://127.0.0.1:9299\ncontrol_auth_token: x\n' > /srv/oldapp/tmp/pids/puma.state
COPY check-puma-status /usr/local/bin/check-puma-status
CMD ["sleep", "infinity"]
