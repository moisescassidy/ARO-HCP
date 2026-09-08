#!/usr/bin/env bash

# Copyright 2026 Microsoft Corporation
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Lints every *.bicep file in the repository. dev-infrastructure/Makefile only
# covers its own templates/ and modules/ directories, so bicep living next to a
# service (for example tooling/tenant-quota/alerting.bicep) was never checked.

set -o errexit
set -o nounset
set -o pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NPROC="${NPROC:-$(nproc)}"

if ! command -v az > /dev/null 2>&1; then
  echo "ERROR: the az CLI is required to lint bicep files"
  exit 1
fi

export AZURE_BICEP_CHECK_VERSION=False

# Warm up the bicep install once, so the parallel workers below do not race to
# download it and so the "already installed" chatter stays out of the output.
az bicep install > /dev/null 2>&1 || true

cd "${REPO_ROOT}"

mapfile -t files < <(git ls-files '*.bicep' | sort)
if [ "${#files[@]}" -eq 0 ]; then
  echo "ERROR: no bicep files found"
  exit 1
fi

echo "Linting ${#files[@]} bicep files across ${NPROC} cores..."

# az bicep lint exits non-zero on error but prints diagnostics to stderr, so
# capture both and let xargs propagate the first failure. The "Bicep CLI is
# already installed" notice is dropped; it is emitted on every invocation.
if printf '%s\n' "${files[@]}" | xargs -P "${NPROC}" -I {} \
  bash -c 'if ! out="$(az bicep lint --file "$1" 2>&1)"; then
    printf "%s\n" "$out" | grep -v "Bicep CLI is already installed"
    exit 1
  fi' _ {}; then
  echo "All bicep files lint clean"
else
  echo
  echo "ERROR: bicep lint failed; fix the errors above"
  exit 1
fi
