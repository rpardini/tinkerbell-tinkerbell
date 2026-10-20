#!/bin/bash

set -eou pipefail

function usage() {
        echo "Usage: $0 [OPTION]..."
        echo "Predict which commits will conflict when rebasing a branch onto upstream,"
        echo "without touching the working tree, the index or any ref."
        echo
        echo "Options:"
        echo "  -u, --upstream            The ref to rebase onto (default: upstream/main)"
        echo "  -b, --branch              The branch to rebase (default: HEAD)"
        echo "  -n, --no-fetch            Do not fetch the upstream remote first"
        echo "  -v, --verbose             Print the conflict hunks of each conflicting commit"
        echo "  -h, --help                Display this help and exit"
        echo
        echo "Exits 0 when the rebase is predicted to apply cleanly, 2 when it is not."
    }

# default values
upstream="upstream/main"
branch="HEAD"
fetch=true
verbose=false

# Options are parsed by hand rather than with getopt(1), as in certs.sh, so this runs on macOS.
while [[ $# -gt 0 ]]; do
  case $1 in
    -u | --upstream)
      [[ $# -ge 2 ]] || { usage; exit 1; }
      upstream=$2
      shift 2 ;;
    --upstream=*)
      upstream=${1#*=}
      shift ;;
    -b | --branch)
      [[ $# -ge 2 ]] || { usage; exit 1; }
      branch=$2
      shift 2 ;;
    --branch=*)
      branch=${1#*=}
      shift ;;
    -n | --no-fetch)
      fetch=false
      shift ;;
    -v | --verbose)
      verbose=true
      shift ;;
    -h | --help)
      usage
      exit 1 ;;
    *)
      echo "Unknown option: $1"
      usage
      exit 1 ;;
  esac
done

if $fetch; then
  remote=${upstream%%/*}
  if git remote get-url "$remote" >/dev/null 2>&1; then
    echo "==> Fetching ${remote}"
    git fetch --quiet "$remote"
  fi
fi

upstream_oid=$(git rev-parse --verify "${upstream}^{commit}")
branch_oid=$(git rev-parse --verify "${branch}^{commit}")
merge_base=$(git merge-base "$upstream_oid" "$branch_oid")

read -r ahead behind < <(git rev-list --left-right --count "${branch_oid}...${upstream_oid}")
echo "==> ${branch} vs ${upstream}: ${ahead} ahead, ${behind} behind (merge-base $(git rev-parse --short "$merge_base"))"

if [[ $behind -eq 0 ]]; then
  echo "    Already on top of ${upstream}; nothing to rebase."
  exit 0
fi

echo
echo "==> Incoming from ${upstream}:"
git log --no-merges --format='    %h %s' "${branch_oid}..${upstream_oid}"

# Commits whose patch is already upstream are dropped by rebase, as `git cherry` reports them.
dropped=$(git cherry "$upstream_oid" "$branch_oid" | sed -n 's/^- //p')
if [[ -n $dropped ]]; then
  echo
  echo "==> Already upstream, rebase will drop:"
  for c in $dropped; do
    git log -1 --format='    %h %s' "$c"
  done
fi

if [[ $(git rev-list --merges --count "${upstream_oid}..${branch_oid}") -gt 0 ]]; then
  echo
  echo "    Note: ${branch} has merge commits; rebase linearizes them, and so does this preview."
fi

echo
echo "==> Aggregate, as one merge (git merge-tree):"
if aggregate=$(git merge-tree --write-tree --name-only --no-messages "$upstream_oid" "$branch_oid"); then
  echo "    clean"
else
  echo "$aggregate" | tail -n +2 | sed 's/^/    /'
fi

# Replay each commit the way rebase would pick them, as a three-way merge of the commit onto the
# rewritten tip with the commit's parent as base. Everything happens in the object store: a
# conflicting commit's tree is recomputed with -X theirs so later commits still have a sensible
# base to apply onto. The commits written along the way are unreachable and left to gc.
echo
echo "==> Per commit, as rebase would apply them:"
onto=$upstream_oid
total=0
conflicts=0
for c in $(git rev-list --reverse --no-merges --cherry-pick --right-only "${upstream_oid}...${branch_oid}"); do
  total=$((total + 1))
  if out=$(git merge-tree --write-tree --name-only --no-messages --merge-base="${c}^" "$onto" "$c"); then
    tree=$(echo "$out" | head -1)
  else
    conflicts=$((conflicts + 1))
    git log -1 --format="    #${total} CONFLICT %h %s" "$c"
    conflicted_tree=$(echo "$out" | head -1)
    for f in $(echo "$out" | tail -n +2); do
      echo "        $f"
      if $verbose; then
        git show "${conflicted_tree}:${f}" | awk '/^<<<<<<< /{p=1} p{print "          " NR ": " $0} /^>>>>>>> /{p=0; print ""}'
      fi
    done
    tree=$(git merge-tree --write-tree --no-messages -X theirs --merge-base="${c}^" "$onto" "$c" | head -1)
  fi
  onto=$(git commit-tree "$tree" -p "$onto" -m "rebase-preview of ${c}")
done

echo
echo "==> ${conflicts} of ${total} commits predicted to conflict"

# Cross-check with git replay (rebase's plumbing sibling); --ref-action=print keeps refs untouched.
if git replay --ref-action=print --onto "$upstream_oid" "${upstream_oid}..${branch_oid}" >/dev/null 2>&1; then
  replay="clean"
else
  replay="conflicts"
fi
echo "    git replay cross-check: ${replay}"

if [[ $conflicts -gt 0 ]]; then
  if [[ $(git config --bool rerere.enabled || true) == true ]]; then
    echo "    rerere is enabled: recorded resolutions may resolve some of these during the real rebase."
  fi
  exit 2
fi
