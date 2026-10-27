#!/usr/bin/env bash
#
# 瞬传 QuickDrop · 一键部署脚本（Linux / macOS）
#
#   curl -fsSL https://raw.githubusercontent.com/Steve0723/quickdrop/main/deploy.sh | bash
#
# 可选环境变量：
#   QD_DIR              安装目录，默认 ~/quickdrop
#   QD_PORT             对外端口，默认 6026
#   QD_ADMIN_USER       管理员用户名，默认 admin
#   QD_ADMIN_PASSWORD   管理员密码，默认随机生成
#   QD_INSTALL_DOCKER   设为 1 时，缺少 Docker 会调用官方脚本自动安装
#
set -euo pipefail

RAW_URL="https://raw.githubusercontent.com/Steve0723/quickdrop/main"
ARCHIVE_URL="https://github.com/Steve0723/quickdrop/archive/refs/heads/main.tar.gz"

# 默认安装到执行命令时所在的目录
INSTALL_DIR="${QD_DIR:-$PWD}"
PORT="${QD_PORT:-6026}"
ADMIN_USER="${QD_ADMIN_USER:-admin}"
ADMIN_PASS="${QD_ADMIN_PASSWORD:-}"

info() { printf '\033[36m›\033[0m %s\n' "$*"; }
ok()   { printf '\033[32m✓\033[0m %s\n' "$*"; }
warn() { printf '\033[33m!\033[0m %s\n' "$*"; }
die()  { printf '\033[31m✗\033[0m %s\n' "$*" >&2; exit 1; }

gen_pass() {
  local p=""
  if command -v openssl >/dev/null 2>&1; then
    p="$(openssl rand -hex 8 2>/dev/null || true)"
  fi
  if [ -z "$p" ]; then
    p="$(od -An -tx1 -N8 /dev/urandom 2>/dev/null | tr -d ' \n' || true)"
  fi
  if [ -z "$p" ]; then
    p="qd$(date +%s)$$"
  fi
  printf '%s' "$p"
}

# ---------- 1. 检查 Docker ----------

SUDO=""
if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then
  SUDO="sudo"
fi

if ! command -v docker >/dev/null 2>&1; then
  warn "未检测到 Docker"
  if [ "${QD_INSTALL_DOCKER:-0}" = "1" ]; then
    info "使用官方脚本安装 Docker…"
    curl -fsSL https://get.docker.com | $SUDO sh
    $SUDO systemctl enable --now docker >/dev/null 2>&1 || true
  else
    cat <<'EOF'

请先安装 Docker，再重新执行本脚本：

  curl -fsSL https://get.docker.com | sh

或让脚本自动安装：

  curl -fsSL https://raw.githubusercontent.com/Steve0723/quickdrop/main/deploy.sh | QD_INSTALL_DOCKER=1 bash

EOF
    exit 1
  fi
fi

if docker info >/dev/null 2>&1; then
  DOCKER="docker"
elif [ -n "$SUDO" ] && $SUDO docker info >/dev/null 2>&1; then
  DOCKER="$SUDO docker"
else
  die "Docker 未运行或当前用户无权限（可执行 sudo systemctl start docker 后重试）"
fi

if $DOCKER compose version >/dev/null 2>&1; then
  COMPOSE="$DOCKER compose"
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE="docker-compose"
else
  die "缺少 Docker Compose 插件，请安装 docker-compose-plugin"
fi

ok "Docker 环境就绪"

# ---------- 2. 获取代码 ----------

NEED_DOWNLOAD=0

if [ -d "$INSTALL_DIR/.git" ]; then
  info "检测到 Git 仓库，拉取最新代码…"
  git -C "$INSTALL_DIR" pull --ff-only >/dev/null 2>&1 || warn "拉取失败，沿用当前代码继续"
elif [ -f "$INSTALL_DIR/docker-compose.yml" ] && [ -f "$INSTALL_DIR/main.go" ]; then
  info "检测到已有代码，更新到最新版本…"
  NEED_DOWNLOAD=1
else
  # 避免把项目文件铺到用户已有内容的目录里
  if [ -n "$(ls -A "$INSTALL_DIR" 2>/dev/null || true)" ] && [ "${QD_FORCE:-0}" != "1" ]; then
    die "目录 $INSTALL_DIR 不为空，且不是本项目的目录。
   请换一个空目录执行，或指定安装位置：
     curl -fsSL $RAW_URL/deploy.sh | QD_DIR=./quickdrop bash
   确认要覆盖现有文件时，加 QD_FORCE=1"
  fi
  NEED_DOWNLOAD=1
fi

if [ "$NEED_DOWNLOAD" = "1" ]; then
  info "下载代码到 $INSTALL_DIR …"
  mkdir -p "$INSTALL_DIR"
  TMP_DIR="$(mktemp -d)"
  trap 'rm -rf "$TMP_DIR"' EXIT
  curl -fsSL "$ARCHIVE_URL" -o "$TMP_DIR/src.tar.gz" || die "下载代码失败"
  # 先进入临时目录再解压，避免 Windows 盘符被 tar 当成远端主机名
  ( cd "$TMP_DIR" && tar -xzf src.tar.gz ) || die "解压失败"
  cp -a "$TMP_DIR/quickdrop-main/." "$INSTALL_DIR/" || die "写入文件失败"
  rm -rf "$TMP_DIR"
  trap - EXIT
fi

cd "$INSTALL_DIR"

# ---------- 3. 生成配置 ----------

if [ -f .env ]; then
  info "已存在 .env，保留原有配置"
  PORT="$(grep -E '^QD_PORT=' .env | head -1 | cut -d= -f2- || true)"
  ADMIN_USER="$(grep -E '^QD_ADMIN_USER=' .env | head -1 | cut -d= -f2- || true)"
  ADMIN_PASS="$(grep -E '^QD_ADMIN_PASSWORD=' .env | head -1 | cut -d= -f2- || true)"
  [ -n "$PORT" ] || PORT=6026
  [ -n "$ADMIN_USER" ] || ADMIN_USER=admin
else
  [ -n "$ADMIN_PASS" ] || ADMIN_PASS="$(gen_pass)"
  cat > .env <<EOF
QD_PORT=$PORT
QD_ADMIN_USER=$ADMIN_USER
QD_ADMIN_PASSWORD=$ADMIN_PASS
TZ=Asia/Shanghai
EOF
  chmod 600 .env
  ok "已生成 .env（含随机管理员密码）"
fi

# ---------- 4. 构建并启动 ----------

if command -v ss >/dev/null 2>&1 && ss -ltn 2>/dev/null | grep -q ":$PORT "; then
  warn "端口 $PORT 已被占用，若启动失败请改用 QD_PORT=其它端口"
fi

info "构建镜像并启动容器（首次约 1-2 分钟）…"
$COMPOSE up -d --build || die "容器启动失败，执行 cd $INSTALL_DIR && $COMPOSE logs 查看原因"

info "等待服务就绪…"
READY=0
for _ in $(seq 1 45); do
  if curl -fsS "http://127.0.0.1:$PORT/api/me" >/dev/null 2>&1; then
    READY=1
    break
  fi
  sleep 1
done

# ---------- 5. 输出结果 ----------

IP="$(hostname -I 2>/dev/null | awk '{print $1}' || true)"
[ -n "${IP:-}" ] || IP="<服务器IP>"

echo
if [ "$READY" = "1" ]; then
  ok "部署完成"
else
  warn "容器已启动，但健康检查尚未通过，可稍后用下面的命令查看日志"
fi
cat <<EOF

  访问地址   http://$IP:$PORT
  用户名     $ADMIN_USER
  密码       ${ADMIN_PASS:-（见 $INSTALL_DIR/data/admin-credentials.txt）}

  安装目录   $INSTALL_DIR
  数据目录   Docker 命名卷 quickdrop_data

  常用命令
    cd $INSTALL_DIR
    $COMPOSE logs -f          # 查看日志
    $COMPOSE restart          # 重启
    $COMPOSE down             # 停止
    git pull && $COMPOSE up -d --build   # 更新版本

EOF
