#!/usr/bin/env bash
# Compare contract/ with the contract of the app. OVERSIRE_APP overrides the app checkout.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
app="${OVERSIRE_APP:-$root/../oversire}"
src="$app/Contract"

if [ ! -d "$src" ]; then
  echo "contract-check: no app contract at $src. Check out oversire/oversire next to this repo, or set OVERSIRE_APP." >&2
  exit 2
fi

status=0
for dir in schema fixtures prompts; do
  if ! diff -r "$src/$dir" "$root/contract/$dir"; then
    status=1
  fi
done

if [ "$status" -ne 0 ]; then
  cat >&2 <<MSG

contract-check: contract/ is not the same as $src.
Copy the contract again:
  rm -rf "$root/contract/schema" "$root/contract/fixtures" "$root/contract/prompts"
  cp -R "$src/schema" "$src/fixtures" "$src/prompts" "$root/contract/"
Then run go test ./... and commit.
MSG
  exit 1
fi
echo "contract-check: contract/ is the same as $src."
