#!/usr/bin/env bash
# check-leaks: fail if tracked files hold a real email address or a live-looking
# secret. Placeholders are allowed: example.com addresses, cloud service
# accounts, and the sample logins you/me/friend/stranger/someone. Fake keys in
# tests are allowed; a Mercury token is never allowed anywhere.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
fail=0

emails=$(git grep -nIoE '[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}' -- . ':!scripts/check-leaks.sh' |
  grep -viE '@([a-z0-9-]+\.)*example\.(com|org|net)$|\.iam\.gserviceaccount\.com$|:(you|me|friend|stranger|someone)@gmail\.com$' || true)
if [ -n "$emails" ]; then
  echo "check-leaks: email addresses that are not placeholders (use name@example.com):"; echo "$emails"; fail=1
fi

tokens=$(git grep -nIE 'secret-token:mercury_' -- . ':!scripts/check-leaks.sh' || true)
if [ -n "$tokens" ]; then
  echo "check-leaks: Mercury API token:"; echo "$tokens" | cut -c1-80; fail=1
fi

keys=$(git grep -nIE 'sk-ant-api[0-9]{2}-[A-Za-z0-9_-]{20,}|sk-[A-Za-z0-9]{40,}|-----BEGIN [A-Z ]*PRIVATE KEY-----|AKIA[0-9A-Z]{16}|ghp_[A-Za-z0-9]{36}' \
  -- . ':!*_test.go' ':!scripts/check-leaks.sh' || true)
if [ -n "$keys" ]; then
  echo "check-leaks: secret-looking value outside tests:"; echo "$keys" | cut -c1-80; fail=1
fi

[ $fail -eq 0 ] && echo "check-leaks: ok"
exit $fail
