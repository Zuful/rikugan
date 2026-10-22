#!/usr/bin/env bash
# The OpenCode config of deploy/compose is the chart's generated one,
# but for the address of rikugan serve: a change to one must reach the
# other.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
helm template sync "$root/helm/rikugan" --set serve.enabled=true --set prometheus.url=http://p:9090 \
  --set serve.escalation.enabled=true |
  python3 -c '
import json, sys, yaml
compose = json.load(open(sys.argv[1]))
for doc in yaml.safe_load_all(sys.stdin):
    if doc and doc["kind"] == "ConfigMap" and doc["metadata"]["name"].endswith("-opencode"):
        chart = json.loads(doc["data"]["opencode.json"])
        chart["mcp"]["rikugan"]["url"] = compose["mcp"]["rikugan"]["url"]
        if chart != compose:
            sys.exit("deploy/compose/opencode.json differs from the chart'"'"'s generated config")
        print("OpenCode configs in sync")
        break
else:
    sys.exit("no OpenCode ConfigMap rendered")
' "$root/deploy/compose/opencode.json"
