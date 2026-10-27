# 瞬传 QuickDrop

一个自托管的临时分享服务：**登录后才能创建分享**，支持文本、隐藏文本（4 位数字密码解锁）、文件；链接可加提取码、可设有效期（默认 24 小时），到期即自动删除记录与磁盘文件。

后端 Go（标准库，零第三方依赖），前端原生 HTML/CSS/JS 并通过 `go:embed` 打进同一个二进制。

## 功能

| 能力 | 说明 |
| --- | --- |
| 登录鉴权 | 只有管理员创建的账号能登录，**不开放注册** |
| 文本分享 | 支持公开文本 + 隐藏文本 |
| 隐藏文本 | 创建时系统随机生成 **4 位数字密码**，访客输入密码才能查看；连续输错 8 次锁定 30 分钟 |
| 文件分享 | 多文件上传（默认最多 10 个，单文件 200MB），支持图片预览 |
| 提取码 | 可选，字母数字；校验通过后下发签名 Cookie，**下载文件同样校验** |
| 有效期 | 默认 24 小时，可选 30 分钟 / 1 / 6 / 12 / 24 小时 / 3 天 / 7 天 / 自定义 |
| 过期清理 | 每分钟扫描一次，过期分享连记录带文件一起删除，不保留 |
| 账号管理 | 管理员可建号、改密、禁用、删除；删除账号会连带删除其分享与文件 |
| 分享面板 | 查看自己分享的状态、剩余时间、浏览/下载次数，可复制链接、查看详情、手动删除 |

## 快速开始

```bash
docker compose up -d --build
```

打开 <http://localhost:6026>，会跳到登录页。

### 首次登录

首次启动会自动创建管理员账号，密码随机生成，两种取法任选：

```bash
# 1. 看启动日志
docker compose logs quickdrop | grep -A2 初始管理员

# 2. 读凭据文件
docker compose exec quickdrop cat /app/data/admin-credentials.txt
```

也可以用环境变量自己指定，避免随机密码：

```bash
# .env
QD_ADMIN_PASSWORD=your-strong-password
```

登录后进入「账号管理」创建其他账号，并建议先改掉管理员密码、删除 `admin-credentials.txt`。

## 配置

在项目根目录建 `.env`（compose 会自动读取），或直接改 `docker-compose.yml`。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `QD_PORT` | `6026` | 宿主机端口 |
| `QD_ADMIN_USER` | `admin` | 初始管理员用户名（仅首次启动生效） |
| `QD_ADMIN_PASSWORD` | 随机 | 初始管理员密码（仅首次启动生效） |
| `QD_SECRET` | 随机 | 访问令牌签名密钥，留空则生成后存入 `db.json` |
| `QD_DEFAULT_TTL_HOURS` | `24` | 默认有效期（小时） |
| `QD_MAX_TTL_HOURS` | `720` | 最长有效期（小时） |
| `QD_MAX_FILE_MB` | `200` | 单文件大小上限 |
| `QD_MAX_SHARE_MB` | `500` | 单次分享总大小上限 |
| `QD_MAX_FILES` | `10` | 单次分享文件数上限 |
| `QD_SESSION_DAYS` | `7` | 登录状态保留天数 |

## 数据与备份

数据全部落在容器内 `/app/data`：

```
/app/data
├── db.json                  # 账号、会话、分享元数据
├── files/                   # 上传的文件（随机文件名）
└── admin-credentials.txt    # 首次启动生成的初始密码
```

默认使用命名卷 `quickdrop_data`。备份：

```bash
docker run --rm -v quickdrop_quickdrop_data:/data -v "$PWD":/backup alpine \
  tar czf /backup/quickdrop-backup.tar.gz -C /data .
```

想直接在宿主机看到文件，把 compose 里的卷换成绑定挂载，并先修好属主（容器内 UID 为 10001）：

```bash
mkdir -p data && sudo chown -R 10001:10001 data
# docker-compose.yml
#   volumes:
#     - ./data:/app/data
```

## 反向代理 / HTTPS

放在 Nginx、Caddy 后面时，记得透传这两个头，否则会话 Cookie 不会带 `Secure`，提取码校验也拿不到真实 IP：

```nginx
location / {
    proxy_pass http://127.0.0.1:6026;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    client_max_body_size 200m;   # 与 QD_MAX_FILE_MB 保持一致
}
```

## 本地开发

需要 Go 1.22+：

```bash
go run .                       # 默认 http://localhost:6026，数据写入 ./data
QD_DATA_DIR=.dev PORT=3100 go run .
```

前端改了不用重新编译：设置 `QD_PUBLIC_DIR=public` 后服务会从磁盘读静态文件，而不是用二进制里内嵌的版本。

```bash
QD_PUBLIC_DIR=public go run .
```

构建：

```bash
go build -trimpath -ldflags="-s -w" -o quickdrop .
```

## 目录结构

```
quickdrop/
├── main.go          # 启动、路由、静态资源、优雅关闭
├── config.go        # 环境变量与默认值
├── store.go         # JSON 持久化（读写锁 + 原子落盘）
├── security.go      # PBKDF2 口令哈希、短 ID、限流
├── handlers.go      # 认证、上传、分享、访客访问、下载
├── admin.go         # 账号管理
├── public/          # 前端（编译时 embed）
│   ├── login.html   index.html   share.html   admin.html   404.html
│   └── assets/      style.css  common.js
├── Dockerfile
├── docker-compose.yml
└── data/            # 运行时生成
```

## 接口

```
POST   /api/login                            登录
POST   /api/logout                           退出
GET    /api/me                               当前用户
PATCH  /api/me                               修改自己的密码

POST   /api/uploads                          原始二进制流上传，返回临时 ID
GET    /api/shares                           我的分享列表
POST   /api/shares                           创建分享
GET    /api/shares/{id}                      分享详情（所有者/管理员）
DELETE /api/shares/{id}                      删除分享

GET    /api/public/shares/{id}               访客读取（提取码未过则 401 need_code）
POST   /api/public/shares/{id}/unlock        校验提取码
POST   /api/public/shares/{id}/reveal        校验隐藏文本密码
GET    /api/public/shares/{id}/files/{fileId} 下载（?mode=inline 可预览图片）

GET    /api/admin/users                      账号列表
POST   /api/admin/users                      创建账号
PATCH  /api/admin/users/{id}                 改密 / 改角色 / 启用禁用
DELETE /api/admin/users/{id}                 删除账号（连带分享与文件）
```

## 安全说明

- 口令用 **PBKDF2-HMAC-SHA256（12 万轮 + 随机盐）** 存储，比对走常量时间
- 会话令牌随机 32 字节，存服务端，Cookie 为 `HttpOnly` + `SameSite=Lax`
- 所有写接口校验 `Origin` 与 `Host` 一致（CSRF 防护）
- 登录、提取码、隐藏密码三处独立限流；隐藏密码另有按分享计的失败锁定
- 上传文件名不参与磁盘路径，磁盘文件名为随机串，杜绝路径穿越
- 下载强制 `Content-Disposition`，`X-Content-Type-Options: nosniff`
- 隐藏文本密码与提取码均**只在服务端保存**，不会随公开接口下发

已知取舍：4 位数字密码只有 1 万种组合，安全边界依赖限流与锁定。若分享内容非常敏感，建议同时开启提取码。
