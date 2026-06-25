#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

CONDA_ENV="${LOGSERVE_CONDA_ENV:-mcts}"

source "$(conda info --base)/etc/profile.d/conda.sh"
conda activate "$CONDA_ENV"

echo "Using Python: $(command -v python) ($(python --version))"
echo "Using Go: $(command -v go) ($(go version))"

python -m pip install -q -r executor/python/requirements.txt -r sdk/python/requirements.txt
go mod download

go test ./... -count=1 -timeout "${LOGSERVE_TEST_TIMEOUT:-180s}" "$@"
