#!/usr/bin/env bash
# build-local.sh — Build the CACPPT controller image using local CABPT changes.
#
# Usage:
#   ./build-local.sh [USERNAME]
#
# USERNAME defaults to "glacier-technologies". The image tag is derived from git describe.
# The script patches go.mod, Dockerfile, and .dockerignore to vendor the local CABPT
# dependency into the Docker build, then restores all files when done (or on failure).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CABPT_DIR="${SCRIPT_DIR}/../cluster-api-bootstrap-provider-talos"
USERNAME="${1:-glacier-technologies}"

if [[ ! -d "$CABPT_DIR" ]]; then
  echo "Error: CABPT directory not found at $CABPT_DIR" >&2
  exit 1
fi

cd "$SCRIPT_DIR"

# --- Backup files we'll modify ---
backup_files=(go.mod go.sum Dockerfile .dockerignore)
for f in "${backup_files[@]}"; do
  cp "$f" "$f.bak"
done

cleanup() {
  echo "Restoring original files..."
  for f in "${backup_files[@]}"; do
    mv "$f.bak" "$f"
  done
  rm -rf vendor
}
trap cleanup EXIT

# --- 1. Add local replace directive and vendor ---
echo "==> Adding local CABPT replace directive"
go mod edit -replace "github.com/siderolabs/cluster-api-bootstrap-provider-talos=${CABPT_DIR}"
go mod tidy

echo "==> Vendoring dependencies"
go mod vendor

# --- 2. Regenerate CRDs locally ---
echo "==> Regenerating CRDs with local controller-gen"
CONTROLLER_GEN="${GOPATH:-$HOME/go}/bin/controller-gen"
if [[ ! -x "$CONTROLLER_GEN" ]]; then
  echo "Installing controller-gen v0.17.0..."
  go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.0
fi
"$CONTROLLER_GEN" crd:crdVersions=v1 paths="./api/..." output:crd:dir=config/crd/bases output:webhook:dir=config/webhook webhook
"$CONTROLLER_GEN" rbac:roleName=manager-role paths="./controllers/..." output:rbac:dir=config/rbac

# --- 3. Patch .dockerignore to include vendor ---
if ! grep -q '!vendor' .dockerignore; then
  echo '!vendor' >> .dockerignore
fi

# --- 4. Patch Dockerfile for vendored build ---
# Replace "go mod download" + "go mod verify" with vendor copy,
# add GOFLAGS=-mod=vendor after tool installs, fix go list.
sed -i \
  '/^RUN.*go mod download/d;
   /^RUN.*go mod verify/d;
   /^COPY \.\/go\.sum/a COPY ./vendor ./vendor' \
  Dockerfile

sed -i \
  's|go list -mod=readonly all|go list all|' \
  Dockerfile

# Insert GOFLAGS=-mod=vendor after the conversion-gen install line
sed -i \
  '/mv.*conversion-gen \/usr\/bin\/conversion-gen/a ENV GOFLAGS=-mod=vendor' \
  Dockerfile

# --- 5. Build ---
echo "==> Building CACPPT controller image (USERNAME=${USERNAME})"
make docker-container USERNAME="$USERNAME"

echo "==> Done. Image built successfully."
