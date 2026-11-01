#!/usr/bin/env bash
# Fail when Go code writes an event kind that is not in contract/schema/event-kinds.json (OVERSIRE-385).
# It reads the kind constants (KindX = "a.b") and the Kind fields (Kind: "a.b") of the Go code.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
catalog="$root/contract/schema/event-kinds.json"

kinds="$(grep -o '"kind": *"[a-z_]*\.[a-z_]*"' "$catalog" | sed 's/.*"\([a-z_]*\.[a-z_]*\)"$/\1/' | sort -u)"

used="$(cd "$root" && grep -rhoE --include='*.go' --exclude='*_test.go' 'Kind([A-Z][A-Za-z]*[[:space:]]*=|:)[[:space:]]*"[a-z_]+\.[a-z_]+"' . |
  sed 's/.*"\([a-z_]*\.[a-z_]*\)"$/\1/' | sort -u)"

status=0
for k in $used; do
  if ! printf '%s\n' "$kinds" | grep -qx "$k"; then
    echo "event-kinds-check: \"$k\" is not in contract/schema/event-kinds.json" >&2
    status=1
  fi
done
if [ "$status" -ne 0 ]; then
  echo "Add the kind to oversire/Contract/schema/event-kinds.json first, with a fixture. Then copy the contract." >&2
  exit 1
fi
echo "event-kinds-check: $(printf '%s\n' "$used" | grep -c .) kinds in the Go code are in the catalog."
