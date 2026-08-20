#!/bin/bash
# 隐私扫描 — 开源铁律：仓库零私人信息（IP/域名/密码/路径）。
# 用法: bash scripts/privacy-scan.sh [root-dir]
# 退出码: 0 = 干净；1 = 命中（CI 会失败）。
set -u
ROOT="${1:-.}"

# 模式清单（QA 第 6 轮确立，9 类）：
# 1) 真实部署 IP（wool 公网 / lambs / 本机 tailnet / OCI / 家宽）
# 2) 真实域名
# 3) 测试容器真实密码
# 4) 个人身份线索
PATTERNS=(
  '140\.83\.35\.152'
  '161\.33\.133\.135'
  '112\.64\.62\.210'
  '100\.9[0-9]\.[0-9]+\.[0-9]+'
  '100\.10[0-9]\.[0-9]+\.[0-9]+'
  '100\.12[0-9]\.[0-9]+\.[0-9]+'
  'wool\.cc\.cd'
  'Lambs\.cc\.cd'
  'LambsTest2026'
  '肖生荣'
  'qejt7thq'
)

HITS=0
for pat in "${PATTERNS[@]}"; do
  # 排除 .git 目录、二进制、lockfile、自身脚本
  found=$(grep -rInE "$pat" "$ROOT" \
    --exclude-dir=.git --exclude-dir=node_modules --exclude-dir=__pycache__ \
    --exclude='*.sum' --exclude='*.exe' --exclude='*.bin' \
    --exclude='privacy-scan.sh' 2>/dev/null | grep -v 'scripts/privacy-scan.sh')
  if [ -n "$found" ]; then
    echo "[HIT] $pat"
    echo "$found" | head -3
    HITS=$((HITS + 1))
  fi
done

if [ "$HITS" -eq 0 ]; then
  echo "privacy-scan: clean (${#PATTERNS[@]} patterns)"
  exit 0
fi
echo "privacy-scan: $HITS pattern group(s) hit — fix before merge"
exit 1
