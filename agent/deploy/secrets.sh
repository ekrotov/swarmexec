#!/usr/bin/env bash
# Create the Docker secrets the agent stack expects from existing PEM files.
# Run against a Swarm manager. Certs are provided externally (no PKI bootstrap
# here — see REQUIREMENTS §10). Expects: ca.crt, agent.crt, agent.key.
set -euo pipefail

CA=${1:-ca.crt}
CERT=${2:-agent.crt}
KEY=${3:-agent.key}

for f in "$CA" "$CERT" "$KEY"; do
  [ -f "$f" ] || { echo "missing file: $f" >&2; exit 1; }
done

docker secret create swarmexec_ca   "$CA"
docker secret create swarmexec_cert "$CERT"
docker secret create swarmexec_key  "$KEY"

echo "created secrets: swarmexec_ca, swarmexec_cert, swarmexec_key"
echo "rotate by: docker secret rm <name> after redeploying with a new versioned secret name."
