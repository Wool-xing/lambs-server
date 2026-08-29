#!/bin/bash
# Lambs 管理系统 — Go 版本部署脚本
# 用法: bash deploy/deploy.sh [--frontend-only|--backend-only]
set -e

APP1_IP="YOUR_APP_HOST"                  # 后端服务器 (Go binary) — ssh config Host 别名（密钥绑定在别名上，裸 IP 不走 config）
WEB1_IP="YOUR_WEB_HOST"                  # 网关服务器 (Nginx + 前端静态文件) — ssh config Host 别名（密钥绑定在别名上，裸 IP 不走 config）
APP1_USER="YOUR_SSH_USER"
WEB1_USER="YOUR_SSH_USER"
APP_PORT=3602
BIN_NAME="lambs-server"
BIN_DIR="/usr/local/bin"
APP_DIR="/home/ubuntu/apps/lambs-server"

MODE="${1:-full}"

echo "=== Lambs 管理系统 (Go) 部署 ==="
echo "后端 → App1 (${APP1_IP}:${APP_PORT})"
echo "前端 → Web1 (${WEB1_IP}:/var/www/Lambs/)"
echo ""

# ── 构建 ──────────────────────────────────────────────
build_frontend() {
    echo "[前端] 构建..."
    cd web && npm install --silent && npm run build && cd ..
    echo "[前端] 构建完成"
}

build_backend() {
    echo "[后端] 交叉编译 (linux/amd64)..."
    cd go-server && GOOS=linux GOARCH=amd64 go build -o "${BIN_NAME}" . && cd ..
    echo "[后端] 构建完成 ($(du -h go-server/${BIN_NAME} | cut -f1))"
}

# ── 上传 ──────────────────────────────────────────────
deploy_frontend() {
    echo "[前端] 上传到 Web1..."
    cd web/dist && tar czf /tmp/lambs-web.tar.gz . && cd ../..
    scp /tmp/lambs-web.tar.gz ${WEB1_USER}@${WEB1_IP}:~/
    ssh ${WEB1_USER}@${WEB1_IP} \
        "sudo mkdir -p /var/www/Lambs && sudo tar xzf ~/lambs-web.tar.gz -C /var/www/Lambs && sudo chown -R www-data:www-data /var/www/Lambs && rm ~/lambs-web.tar.gz"
    rm -f /tmp/lambs-web.tar.gz
    echo "[前端] 部署完成"
}

deploy_backend() {
    echo "[后端] 上传到 App1..."
    scp "go-server/${BIN_NAME}" ${APP1_USER}@${APP1_IP}:${APP_DIR}/${BIN_NAME}-new

    ssh ${APP1_USER}@${APP1_IP} << 'ENDSSH'
        # 创建 .env（如果不存在）
        if [ ! -f ~/apps/lambs-server/.env ]; then
            JWT_SEC=$(python3 -c "import secrets; print(secrets.token_hex(32))")
            cat > ~/apps/lambs-server/.env << EOF
DATABASE_URL=postgresql+asyncpg://lambs_admin:CHANGE_ME@127.0.0.1:5433/lambs
JWT_SECRET=${JWT_SEC}
PORT=3602
EOF
            chmod 600 ~/apps/lambs-server/.env
            echo "[后端] .env 已创建 — 请编辑修改数据库密码"
        fi

        # 部署二进制
        sudo systemctl stop lambs-server 2>/dev/null || true
        sleep 1
        sudo cp ~/apps/lambs-server/lambs-server-new /usr/local/bin/lambs-server
        sudo chmod +x /usr/local/bin/lambs-server
        sudo systemctl start lambs-server
        sleep 2
        sudo systemctl status lambs-server --no-pager -l | head -6
ENDSSH
    echo "[后端] 部署完成"
}

setup_systemd() {
    echo "[Systemd] 注册服务..."
    scp deploy/lambs-server.service ${APP1_USER}@${APP1_IP}:~/apps/lambs-server/
    ssh ${APP1_USER}@${APP1_IP} \
        "sudo cp ~/apps/lambs-server/lambs-server.service /etc/systemd/system/ && sudo systemctl daemon-reload && sudo systemctl enable lambs-server"
    echo "[Systemd] 完成"
}

setup_nginx() {
    echo "[Nginx] 检查 Web1 配置..."
    ssh ${WEB1_USER}@${WEB1_IP} << 'ENDSSH'
        if grep -q "location /lambs/" /etc/nginx/sites-available/wool-cc-cd 2>/dev/null; then
            echo "[Nginx] lambs 已配置，跳过"
        else
            echo "[Nginx] 需要手动添加 lambs 配置块到 nginx — 参考 deploy/nginx-lambs.conf"
        fi
        # 确保 lambs-managed.conf 包含
        if ! grep -q "lambs-managed.conf" /etc/nginx/sites-available/wool-cc-cd 2>/dev/null; then
            echo "[Nginx] 需要在 nginx 443 块中添加: include /etc/nginx/sites-available/lambs-managed.conf;"
        fi
ENDSSH
}

# ── 主流程 ──────────────────────────────────────────────
case "$MODE" in
    frontend-only)
        build_frontend && deploy_frontend
        ;;
    backend-only)
        build_backend && deploy_backend
        ;;
    full)
        build_frontend && build_backend
        deploy_frontend && deploy_backend
        setup_nginx
        ;;
    setup)
        setup_systemd
        setup_nginx
        ;;
    *)
        echo "用法: bash deploy/deploy.sh [frontend-only|backend-only|full|setup]"
        exit 1
        ;;
esac

echo ""
echo "=========================================="
echo "  部署完成"
echo "  Lambs:   https://wool.cc.cd/Lambs/"
echo "  API:     https://wool.cc.cd/Lambs/api/health"
echo "=========================================="
