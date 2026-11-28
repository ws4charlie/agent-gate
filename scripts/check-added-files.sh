#!/usr/bin/env bash
# check-added-files: fail if a change adds or modifies a file over 1 MB or an
# executable binary (ELF, Mach-O, PE, e.g. a `go build` output committed by
# `git add -A`). Compares HEAD (or the second argument) with the merge-base of
# it and BASE; when there is no merge-base (a shallow CI checkout of a PR's
# merge commit), with BASE itself.
#   scripts/check-added-files.sh origin/main [HEAD]
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
base=${1:?usage: check-added-files.sh BASE [HEAD]}
head=${2:-HEAD}
max=$((1024 * 1024))
from=$(git merge-base "$base" "$head" 2>/dev/null || git rev-parse "$base")
fail=0
while IFS= read -r -d '' path; do
  mode=$(git ls-tree "$head" -- "$path" | cut -d' ' -f1)
  [ "$mode" = 160000 ] || [ "$mode" = 120000 ] && continue # submodule, symlink
  size=$(git cat-file -s "$head:$path")
  if [ "$size" -gt "$max" ]; then
    echo "check-added-files: $path is $size bytes (limit $max)"; fail=1
  fi
  case $(git cat-file blob "$head:$path" | head -c 4 | od -An -tx1 | tr -d ' \n') in
    7f454c46 | cffaedfe | cefaedfe | feedfacf | feedface | cafebabe | 4d5a*)
      echo "check-added-files: $path is an executable binary"; fail=1 ;;
  esac
done < <(git diff -z --name-only --no-renames --diff-filter=AM "$from" "$head")
[ $fail -eq 0 ] && echo "check-added-files: ok"
exit $fail
