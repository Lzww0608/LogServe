#!/usr/bin/env bash
set -euo pipefail

# Activates the expected conda environment, installs Python requirements, and
# runs the full Go test suite. This is a developer convenience wrapper for the
# local MCTS-oriented environment, not a hermetic CI image.
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

CONDA_ENV="${LOGSERVE_CONDA_ENV:-mcts}"

# Source conda.sh before activation so the script works in non-interactive
# shells where the conda function is not preloaded.
source "$(conda info --base)/etc/profile.d/conda.sh"
conda activate "$CONDA_ENV"

echo "Using Python: $(command -v python) ($(python --version))"
echo "Using Go: $(command -v go) ($(go version))"

# Install both executor and SDK dependencies because integration-style Go tests
# can invoke Python workers or client helpers through the local workspace.
python -m pip install -q -r executor/python/requirements.txt -r sdk/python/requirements.txt
go mod download

# Forward any extra arguments to go test so callers can narrow packages or add
# flags while keeping the environment setup shared.
go test ./... -count=1 -timeout "${LOGSERVE_TEST_TIMEOUT:-180s}" "$@"
