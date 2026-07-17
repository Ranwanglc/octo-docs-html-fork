#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

required_files='.github/CODEOWNERS
.github/PULL_REQUEST_TEMPLATE.md
.github/ISSUE_TEMPLATE/bug_report.yml
.github/ISSUE_TEMPLATE/config.yml
.github/ISSUE_TEMPLATE/feature_request.yml
.github/dependabot.yml
.github/workflows/auto-add-to-project.yml
.github/workflows/check-sprint.yml
.github/workflows/codeql.yml
.github/workflows/community-config.yml
.github/workflows/dependency-review.yml
.github/workflows/docker-lint.yml
.github/workflows/history-check.yml
.github/workflows/labeler.yml
.github/workflows/osv-scanner.yml
.github/workflows/pr-title-lint.yml
.github/workflows/secret-scan.yml
.github/workflows/workflow-sanity.yml'

for path in $required_files; do
  if [[ ! -f "$path" ]]; then
    echo "missing required community config: $path" >&2
    exit 1
  fi
done

reusable_workflows='auto-add-to-project.yml
check-sprint.yml
codeql.yml
dependency-review.yml
docker-lint.yml
history-check.yml
labeler.yml
pr-title-lint.yml
secret-scan.yml
workflow-sanity.yml'

reusable_workflow_ref() {
  case "$1" in
    auto-add-to-project.yml) echo 'Mininglamp-OSS/.github/.github/workflows/auto-add-to-project.yml@v1' ;;
    check-sprint.yml) echo 'Mininglamp-OSS/.github/.github/workflows/reusable-check-sprint.yml@v1' ;;
    codeql.yml) echo 'Mininglamp-OSS/.github/.github/workflows/reusable-codeql.yml@v1' ;;
    dependency-review.yml) echo 'Mininglamp-OSS/.github/.github/workflows/reusable-dependency-review.yml@v1' ;;
    docker-lint.yml) echo 'Mininglamp-OSS/.github/.github/workflows/reusable-docker-lint.yml@v1' ;;
    history-check.yml) echo 'Mininglamp-OSS/.github/.github/workflows/reusable-history-check.yml@v1' ;;
    labeler.yml) echo 'Mininglamp-OSS/.github/.github/workflows/reusable-pr-labeler.yml@v1' ;;
    pr-title-lint.yml) echo 'Mininglamp-OSS/.github/.github/workflows/reusable-pr-title-lint.yml@v1' ;;
    secret-scan.yml) echo 'Mininglamp-OSS/.github/.github/workflows/reusable-secret-scan.yml@v1' ;;
    workflow-sanity.yml) echo 'Mininglamp-OSS/.github/.github/workflows/workflow-sanity.yml@v1' ;;
    *) return 1 ;;
  esac
}

for workflow in $reusable_workflows; do
  path=".github/workflows/$workflow"
  expected="uses: $(reusable_workflow_ref "$workflow")"
  if ! grep -Fq "$expected" "$path"; then
    echo "unexpected reusable workflow reference in $path" >&2
    exit 1
  fi
  if grep -Eq '^[[:space:]]+run:' "$path"; then
    echo "reusable workflow caller must not execute local commands: $path" >&2
    exit 1
  fi
done

for workflow in auto-add-to-project.yml check-sprint.yml labeler.yml pr-title-lint.yml; do
  path=".github/workflows/$workflow"
  if ! grep -Fq 'pull_request_target:' "$path"; then
    echo "metadata-only workflow must use pull_request_target: $path" >&2
    exit 1
  fi
  if grep -Fq 'actions/checkout' "$path"; then
    echo "pull_request_target workflow must not check out PR code: $path" >&2
    exit 1
  fi
  if ! grep -Fq 'permissions: {}' "$path"; then
    echo "pull_request_target workflow must deny permissions by default: $path" >&2
    exit 1
  fi
done

for workflow in auto-add-to-project.yml check-sprint.yml; do
  if ! grep -Fq 'PROJECT_TOKEN' ".github/workflows/$workflow"; then
    echo "project workflow must pass PROJECT_TOKEN: $workflow" >&2
    exit 1
  fi
done

if ! grep -Fq -- "- 'deploy/Dockerfile*'" .github/workflows/docker-lint.yml; then
  echo 'docker lint must watch deploy/Dockerfile' >&2
  exit 1
fi
if ! grep -Fq "dockerfiles: 'deploy/Dockerfile'" .github/workflows/docker-lint.yml; then
  echo 'docker lint must lint deploy/Dockerfile' >&2
  exit 1
fi

osv_sha='9a498708959aeaef5ef730655706c5a1df1edbc2'
if [[ $(grep -Fc "@$osv_sha" .github/workflows/osv-scanner.yml) -ne 2 ]]; then
  echo 'OSV reusable workflows must be pinned to the approved SHA' >&2
  exit 1
fi

if ! grep -Fq 'package-ecosystem: "gomod"' .github/dependabot.yml ||
   ! grep -Fq 'package-ecosystem: "github-actions"' .github/dependabot.yml; then
  echo 'Dependabot must cover Go modules and GitHub Actions' >&2
  exit 1
fi

if ! grep -Fq '* @Mininglamp-OSS/maintainers' .github/CODEOWNERS; then
  echo 'CODEOWNERS must assign the maintainers team' >&2
  exit 1
fi

if ! grep -Fq 'bash .github/tests/community-config.sh' .github/workflows/community-config.yml; then
  echo 'community config contract must run in CI' >&2
  exit 1
fi

echo 'community config contract passed'
