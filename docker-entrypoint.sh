#!/bin/sh
#
# 容器入口：以 root 启动时，先把挂载进来的数据目录交给运行用户，
# 再降权到 UID 10001 运行主程序，避免绑定挂载出现 permission denied。
#
set -e

DATA_DIR="${QD_DATA_DIR:-/app/data}"
APP_UID=10001
APP_GID=10001

if [ "$(id -u)" = "0" ]; then
  mkdir -p "$DATA_DIR" 2>/dev/null || true

  owner="$(stat -c '%u' "$DATA_DIR" 2>/dev/null || echo unknown)"
  if [ "$owner" != "$APP_UID" ]; then
    echo "[entrypoint] 修正数据目录属主: $DATA_DIR -> ${APP_UID}:${APP_GID}"
    if ! chown -R "${APP_UID}:${APP_GID}" "$DATA_DIR" 2>/dev/null; then
      echo "[entrypoint] 警告: 无法修改 $DATA_DIR 属主，若启动失败请检查挂载权限" >&2
    fi
  fi

  exec su-exec "${APP_UID}:${APP_GID}" "$@"
fi

# 已通过 compose 的 user: 指定了非 root 用户，直接运行
exec "$@"
