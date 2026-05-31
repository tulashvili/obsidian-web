#!/bin/sh
# Стартует под root: чинит права на тома и готовит SSH-ключ, затем сбрасывает
# привилегии до пользователя app и запускает сервер.
set -e

mkdir -p "$OSA_DATA_DIR" "$OSA_WORK_DIR"
chown app:app "$OSA_DATA_DIR" "$OSA_WORK_DIR"

# Если задан исходный SSH-ключ (смонтирован read-only), копируем его в дом
# пользователя app с корректными правами (SSH требует 0600 и владельца).
if [ -n "$OSA_GIT_SSH_KEY_SRC" ] && [ -f "$OSA_GIT_SSH_KEY_SRC" ]; then
  mkdir -p /home/app/.ssh
  cp "$OSA_GIT_SSH_KEY_SRC" /home/app/.ssh/git_key
  chmod 600 /home/app/.ssh/git_key
  chown -R app:app /home/app/.ssh
  export OSA_GIT_SSH_KEY=/home/app/.ssh/git_key
fi

exec su-exec app:app /usr/local/bin/server
