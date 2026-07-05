$ErrorActionPreference = "Stop"

# Builds the local LogServe image and applies the Kubernetes manifests. When
# kind is installed, the freshly built image is loaded into the local kind
# cluster before kubectl applies the kustomize deployment.
$root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path

docker build -t logserve:local -f (Join-Path $root "deployments\Dockerfile") $root

# Loading into kind is optional so the same script can target clusters that
# already know how to pull logserve:local or use a different image cache.
if (Get-Command kind -ErrorAction SilentlyContinue) {
  kind load docker-image logserve:local
}

kubectl apply -k (Join-Path $root "deployments\k8s")
# Wait for each deployment that participates in the default local topology so
# callers get a deployment-level readiness signal before the script exits.
kubectl -n logserve rollout status deployment/logserve-logd
kubectl -n logserve rollout status deployment/logserve-control
kubectl -n logserve rollout status deployment/logserve-worker-a
kubectl -n logserve rollout status deployment/logserve-worker-b
