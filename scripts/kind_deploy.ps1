$ErrorActionPreference = "Stop"

$root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path

docker build -t logserve:local -f (Join-Path $root "deployments\Dockerfile") $root

if (Get-Command kind -ErrorAction SilentlyContinue) {
  kind load docker-image logserve:local
}

kubectl apply -k (Join-Path $root "deployments\k8s")
kubectl -n logserve rollout status deployment/logserve-logd
kubectl -n logserve rollout status deployment/logserve-control
kubectl -n logserve rollout status deployment/logserve-worker-a
kubectl -n logserve rollout status deployment/logserve-worker-b
