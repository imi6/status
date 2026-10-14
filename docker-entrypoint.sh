#!/bin/sh
set -eu

if [ ! -f /data/config.json ]; then
    echo "缺少 /data/config.json。请挂载可写的配置文件，例如：-v ./config.json:/data/config.json" >&2
    exit 1
fi

cd /data
rm -f /data/status.html /data/admin.html
ln -s /opt/status/status.html /data/status.html
ln -s /opt/status/admin.html /data/admin.html

exec /usr/local/bin/status
