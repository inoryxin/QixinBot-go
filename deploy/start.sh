#!/bin/sh
# Linux 启动脚本。放在部署目录（和 config.toml 同级）。
set -e
cd "$(dirname "$0")"
exec ./bin/qixinbot -config config.toml
