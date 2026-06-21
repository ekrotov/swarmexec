#!/usr/bin/env bash
# Generate a throwaway CA + server cert + one client cert for LOCAL TESTING only.
# Production certs are provided externally (REQUIREMENTS §10 — no PKI bootstrap).
#
# Usage: ./gen-dev-certs.sh [output-dir] [server-SAN]
#   server-SAN defaults to "localhost"; for a real node use its hostname/IP and
#   pass e.g. DNS:node1 or IP:10.0.0.5.
set -euo pipefail

OUT=${1:-certs}
SAN=${2:-DNS:localhost,IP:127.0.0.1}
mkdir -p "$OUT"
cd "$OUT"

# CA
openssl genrsa -out ca.key 4096
openssl req -x509 -new -nodes -key ca.key -sha256 -days 3650 \
  -subj "/CN=swarmexec-dev-ca" -out ca.crt

# Server cert (agent), with SANs so the CLI can verify the node address.
openssl genrsa -out agent.key 4096
openssl req -new -key agent.key -subj "/CN=swarmexec-agent" -out agent.csr
openssl x509 -req -in agent.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -days 825 -sha256 -extfile <(printf "subjectAltName=%s\nextendedKeyUsage=serverAuth" "$SAN") \
  -out agent.crt

# Client cert (operator). The CN becomes the audited operator identity.
openssl genrsa -out operator.key 4096
openssl req -new -key operator.key -subj "/CN=operator@example.com" -out operator.csr
openssl x509 -req -in operator.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -days 825 -sha256 -extfile <(printf "extendedKeyUsage=clientAuth") \
  -out operator.crt

rm -f ./*.csr
echo "wrote dev certs to $(pwd): ca.crt, agent.crt/key, operator.crt/key"
