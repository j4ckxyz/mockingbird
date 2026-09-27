#!/usr/bin/env bash
# One-command setup for mockingbird behind a Cloudflare Tunnel.
#
#   ./deploy/pi/setup.sh mockingbird.example.com
#
# What it does (safe to run again; it never overwrites your keys):
#   1. installs the network sandbox firewall (needs sudo),
#   2. creates deploy/pi/.env with fresh random keys,
#   3. builds and starts the container on 127.0.0.1:18480.
# Afterwards, add a Cloudflare Tunnel route to http://localhost:18480.
set -euo pipefail

host="${1:-}"
if [[ -z "$host" ]]; then
	echo "usage: $0 <public hostname, e.g. mockingbird.example.com>" >&2
	exit 1
fi
here="$(cd "$(dirname "$0")" && pwd)"
cd "$here"

for cmd in docker openssl sudo; do
	command -v "$cmd" >/dev/null || { echo "missing: $cmd" >&2; exit 1; }
done
docker compose version >/dev/null || { echo "missing: docker compose plugin" >&2; exit 1; }
nft=$(command -v nft || echo /usr/sbin/nft)
[[ -x "$nft" ]] || { echo "missing: nftables (sudo apt install nftables)" >&2; exit 1; }

echo "==> Installing the network sandbox firewall"
sudo "$nft" -c -f firewall.nft
sudo install -d -m 0755 /etc/mockingbird
sudo install -m 0644 firewall.nft /etc/mockingbird/firewall.nft
sudo install -m 0644 mockingbird-firewall.service /etc/systemd/system/mockingbird-firewall.service
sudo systemctl daemon-reload
sudo systemctl enable --now mockingbird-firewall.service
sudo systemctl restart mockingbird-firewall.service

if [[ -f .env ]]; then
	echo "==> Keeping existing .env"
else
	echo "==> Creating .env with fresh keys"
	umask 077
	cat >.env <<EOF
MB_SECRET_KEY=$(openssl rand -hex 32)
MB_ENCRYPTION_KEY=$(openssl rand -hex 32)
MB_PUBLIC_URL=http://$host
MB_BRIDGE_HOSTS=$host
MB_LOG_LEVEL=info
EOF
fi
chmod 600 .env

echo "==> Building and starting mockingbird"
docker compose up -d --build

echo "==> Waiting for it to answer"
for _ in $(seq 1 30); do
	if curl -fs http://127.0.0.1:18480/help/test.json >/dev/null 2>&1; then
		echo
		echo "mockingbird is running on http://127.0.0.1:18480"
		echo
		echo "Last step: in the Cloudflare dashboard, open Networks > Tunnels > your tunnel >"
		echo "Published application routes > Add, and set:"
		echo "    hostname: $host    service: HTTP    URL: localhost:18480"
		echo "Keep 'Always Use HTTPS' off for $host (old apps only speak plain HTTP)."
		exit 0
	fi
	sleep 2
done
echo "mockingbird did not start; see: docker compose -f $here/compose.yaml logs" >&2
exit 1
