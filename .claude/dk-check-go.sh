#!/usr/bin/env bash
# dk-workflow 门禁用的 Go 检查包装脚本（dk-run-checks 的 argv 不走 shell，
# 进目录 / PATH 回退这类逻辑只能放在脚本里）。
# 用法：bash .claude/dk-check-go.sh build|vet
set -euo pipefail

cd "$(dirname "$0")/../backend"

# go 不在 PATH 时回退到 Windows 默认安装位置；其他平台无影响。
if ! command -v go >/dev/null 2>&1; then
  if [ -n "${LOCALAPPDATA:-}" ] && command -v cygpath >/dev/null 2>&1; then
    export PATH="$PATH:$(cygpath -u "$LOCALAPPDATA")/Programs/Go/bin"
  fi
fi
command -v go >/dev/null 2>&1 || { echo "go not found in PATH" >&2; exit 1; }

case "${1:-}" in
  build) exec go build ./... ;;
  vet)   exec go vet ./... ;;
  *) echo "usage: $0 build|vet" >&2; exit 2 ;;
esac
