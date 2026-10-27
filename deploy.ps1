# 瞬传 QuickDrop · Windows 一键部署（需已安装并启动 Docker Desktop）
#
#   irm https://raw.githubusercontent.com/Steve0723/quickdrop/main/deploy.ps1 | iex
#
# 可选环境变量（执行前设置）：
#   QD_DIR  QD_PORT  QD_ADMIN_USER  QD_ADMIN_PASSWORD

$ErrorActionPreference = 'Stop'

$RepoZip    = 'https://github.com/Steve0723/quickdrop/archive/refs/heads/main.zip'
$InstallDir = if ($env:QD_DIR) { $env:QD_DIR } else { Join-Path $env:USERPROFILE 'quickdrop' }
$Port       = if ($env:QD_PORT) { $env:QD_PORT } else { '6026' }
$AdminUser  = if ($env:QD_ADMIN_USER) { $env:QD_ADMIN_USER } else { 'admin' }
$AdminPass  = $env:QD_ADMIN_PASSWORD

function Info($m) { Write-Host "› $m" -ForegroundColor Cyan }
function Ok($m)   { Write-Host "✓ $m" -ForegroundColor Green }
function Warn($m) { Write-Host "! $m" -ForegroundColor Yellow }
function Die($m)  { throw "✗ $m" }

# ---------- 1. 检查 Docker ----------

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    Die "未检测到 Docker，请先安装 Docker Desktop：https://www.docker.com/products/docker-desktop/"
}

docker info *> $null
if ($LASTEXITCODE -ne 0) { Die "Docker Desktop 未运行，请先启动后重试" }

docker compose version *> $null
if ($LASTEXITCODE -ne 0) { Die "缺少 docker compose 插件，请升级 Docker Desktop" }

Ok 'Docker 环境就绪'

# ---------- 2. 获取代码 ----------

$ComposeFile = Join-Path $InstallDir 'docker-compose.yml'

if (Test-Path $ComposeFile) {
    Info "复用已有目录 $InstallDir"
} else {
    Info "下载代码到 $InstallDir …"
    $tmp = Join-Path $env:TEMP ("qd-" + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp -Force | Out-Null

    Invoke-WebRequest -Uri $RepoZip -OutFile (Join-Path $tmp 'src.zip')
    Expand-Archive -Path (Join-Path $tmp 'src.zip') -DestinationPath $tmp -Force

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    Copy-Item -Path (Join-Path $tmp 'quickdrop-main\*') -Destination $InstallDir -Recurse -Force
    Remove-Item $tmp -Recurse -Force
}

# ---------- 3. 生成配置 ----------

$EnvFile = Join-Path $InstallDir '.env'

if (Test-Path $EnvFile) {
    Info '已存在 .env，保留原有配置'
    $lines = Get-Content $EnvFile
    foreach ($line in $lines) {
        if ($line -match '^QD_PORT=(.*)$')         { $Port = $Matches[1] }
        if ($line -match '^QD_ADMIN_USER=(.*)$')   { $AdminUser = $Matches[1] }
        if ($line -match '^QD_ADMIN_PASSWORD=(.*)$') { $AdminPass = $Matches[1] }
    }
} else {
    if (-not $AdminPass) {
        $chars = '0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ'
        $AdminPass = -join (1..16 | ForEach-Object { $chars[(Get-Random -Maximum $chars.Length)] })
    }
    @(
        "QD_PORT=$Port"
        "QD_ADMIN_USER=$AdminUser"
        "QD_ADMIN_PASSWORD=$AdminPass"
        'TZ=Asia/Shanghai'
    ) | Set-Content -Path $EnvFile -Encoding utf8
    Ok '已生成 .env（含随机管理员密码）'
}

# ---------- 4. 构建并启动 ----------

Push-Location $InstallDir
try {
    Info '构建镜像并启动容器（首次约 1-2 分钟）…'
    docker compose up -d --build
    if ($LASTEXITCODE -ne 0) { Die "容器启动失败，可执行 docker compose logs 查看原因" }
} finally {
    Pop-Location
}

Info '等待服务就绪…'
$ready = $false
for ($i = 0; $i -lt 45; $i++) {
    try {
        $resp = Invoke-WebRequest -Uri "http://127.0.0.1:$Port/api/me" -UseBasicParsing -TimeoutSec 2
        if ($resp.StatusCode -eq 200) { $ready = $true; break }
    } catch {
        Start-Sleep -Seconds 1
    }
}

# ---------- 5. 输出结果 ----------

Write-Host ''
if ($ready) { Ok '部署完成' } else { Warn '容器已启动，但健康检查尚未通过，可稍后查看日志' }

Write-Host @"

  访问地址   http://localhost:$Port
  用户名     $AdminUser
  密码       $AdminPass

  安装目录   $InstallDir
  数据目录   Docker 命名卷 quickdrop_data

  常用命令（在 $InstallDir 下执行）
    docker compose logs -f        查看日志
    docker compose restart        重启
    docker compose down           停止

"@
