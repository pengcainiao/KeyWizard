#!/usr/bin/env bash
# 构建脚本：本机 mac 版 / 交叉编译 Windows 版
set -e
cd "$(dirname "$0")"

case "${1:-mac}" in
  mac)
    echo "==> 构建 macOS 版 (keywizard)"
    go build -o keywizard .
    echo "完成: ./keywizard"
    ;;
  win|windows)
    echo "==> 交叉编译 Windows 版 (keywizard.exe)"
    CGO_ENABLED=1 \
    GOOS=windows GOARCH=amd64 \
    CC=x86_64-w64-mingw32-gcc \
    CXX=x86_64-w64-mingw32-g++ \
    go build -ldflags "-H windowsgui" -o keywizard.exe .
    echo "完成: ./keywizard.exe (拷到 Windows 双击即可)"
    ;;
  *)
    echo "用法: ./build.sh [mac|win]"
    exit 1
    ;;
esac
