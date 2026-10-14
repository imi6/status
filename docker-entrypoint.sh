#!/bin/sh
set -eu

if [ ! -f /data/config.json ]; then
    echo "缺少 /data/config.json。请挂载可写的配置文件，例如：-v ./config.json:/data/config.json" >&2
    exit 1
fi

cd /data
# 页面在镜像内的 /opt/status，不写到挂载目录。顺手清掉旧版本留下的符号链接。
if [ -L /data/status.html ]; then
    rm -f /data/status.html
fi
if [ -L /data/admin.html ]; then
    rm -f /data/admin.html
fi

exec /usr/local/bin/status
