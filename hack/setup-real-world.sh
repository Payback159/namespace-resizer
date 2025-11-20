#!/bin/bash
set -e

CLUSTER_NAME="resizer-real-world"
DEMO_REPO_PATH="../ns-resizer-demo"

echo "🌍 Starting Real-World Integration Setup..."

# 1. Check Prerequisites
if ! command -v kind &> /dev/null; then
    echo "❌ 'kind' is not installed."
    exit 1
fi

if ! command -v kubectl &> /dev/null; then
    echo "❌ 'kubectl' is not installed."
    exit 1
fi

# 2. Create/Reuse Cluster
if kind get clusters | grep -q "^$CLUSTER_NAME$"; then
    echo "✅ Cluster '$CLUSTER_NAME' already exists."
else
    echo "📦 Creating kind cluster '$CLUSTER_NAME'..."
    kind create cluster --name $CLUSTER_NAME
fi

# Switch context
kubectl cluster-info --context kind-$CLUSTER_NAME

echo "⏳ Waiting for cluster nodes..."
kubectl wait --for=condition=Ready nodes --all --timeout=60s

# 3. Setup System Namespace
echo "📦 Creating system namespace 'namespace-resizer-system'..."
kubectl create ns namespace-resizer-system --dry-run=client -o yaml | kubectl apply -f -

# 4. Initial Sync
echo "------------------------------------------------"
echo "🚀 Performing initial sync from $DEMO_REPO_PATH..."
if [ -d "$DEMO_REPO_PATH" ]; then
    kubectl apply -R -f "$DEMO_REPO_PATH/managed-resources/resizer-demo/"
else
    echo "❌ Error: Demo repo not found at $DEMO_REPO_PATH"
    exit 1
fi

echo "------------------------------------------------"
echo "✅ Environment Ready!"
echo ""
echo "📋 Next Steps:"
echo ""
echo "1. Open a new terminal and start the GitOps Sync Loop (ArgoCD Replacement):"
echo "   ./hack/sync-loop.sh $DEMO_REPO_PATH"
echo ""
echo "2. Export your GitHub Credentials (REQUIRED for Real Mode):"
echo "   export GITHUB_TOKEN=ghp_..."
echo "   export GITHUB_OWNER=your-username"
echo "   export GITHUB_REPO=ns-resizer-demo"
echo "   export ENABLE_AUTO_MERGE=true"
echo ""
echo "3. Run the Controller:"
echo "   go run cmd/main.go"
echo ""
echo "4. Trigger a resize:"
echo "   - Edit a quota in $DEMO_REPO_PATH"
echo "   - Or deploy a bursting pod"
echo "   - Watch the controller create a PR"
echo "   - Watch the Sync Loop apply the PR after merge"
