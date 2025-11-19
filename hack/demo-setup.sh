#!/bin/bash
set -e

CLUSTER_NAME="resizer-demo"

echo "🚀 Starting Demo Setup for Namespace Resizer..."

# 1. Check prerequisites
if ! command -v kind &> /dev/null; then
    echo "❌ 'kind' is not installed. Please install it first."
    exit 1
fi

if ! command -v kubectl &> /dev/null; then
    echo "❌ 'kubectl' is not installed. Please install it first."
    exit 1
fi

# 2. Create Cluster
if kind get clusters | grep -q "^$CLUSTER_NAME$"; then
    echo "✅ Cluster '$CLUSTER_NAME' already exists."
else
    echo "📦 Creating kind cluster '$CLUSTER_NAME'..."
    kind create cluster --name $CLUSTER_NAME
fi

# Switch context
kubectl cluster-info --context kind-$CLUSTER_NAME

echo "⏳ Waiting for cluster to be ready..."
kubectl wait --for=condition=Ready nodes --all --timeout=60s

# 2.1 Create System Namespace
echo "📦 Creating system namespace 'namespace-resizer-system'..."
kubectl create ns namespace-resizer-system --dry-run=client -o yaml | kubectl apply -f -

# 3. Apply Resources from Demo Repo
echo "------------------------------------------------"
echo "🚀 Applying resources from ns-resizer-demo..."
kubectl apply -R -f ../ns-resizer-demo/managed-resources/resizer-demo/

# 4. Scenario 1: Standard Metric-Based Trigger
echo "------------------------------------------------"
echo "🧪 Scenario 1: Standard Metric Trigger"
echo "   Namespace: demo-standard"
echo "   Quota: hard.cpu=1000m"
echo "   Workload: requests.cpu=850m (85%)"
echo "   Expectation: Controller recommends resize (Threshold 80% exceeded)"

# 5. Scenario 2: Event-Based Trigger (Burst)
echo "------------------------------------------------"
echo "🧪 Scenario 2: Event-Based Trigger (Burst)"
echo "   Namespace: demo-burst"
echo "   Quota: hard.cpu=500m"
echo "   Workload: requests.cpu=1000m (Fails immediately)"
echo "   Expectation: Controller detects FailedCreate event and recommends resize"

# 6. Scenario 3: Opt-Out
echo "------------------------------------------------"
echo "🧪 Scenario 3: Opt-Out"
echo "   Namespace: demo-opt-out"
echo "   Annotation: resizer.io/enabled: 'false'"
echo "   Quota: hard.cpu=1000m"
echo "   Workload: requests.cpu=900m (90%)"
echo "   Expectation: NO recommendation despite >80% usage"

# 7. Scenario 4: Custom Policy
echo "------------------------------------------------"
echo "🧪 Scenario 4: Custom Policy"
echo "   Namespace: demo-custom"
echo "   Annotation: resizer.io/cpu-threshold: '50'"
echo "   Quota: hard.cpu=1000m"
echo "   Workload: requests.cpu=600m (60%)"
echo "   Expectation: Recommendation triggered because 60% > 50% (Custom Threshold)"

echo "------------------------------------------------"
echo "✅ Setup complete!"
echo "To run the controller locally and see the logs:"
echo "  go run cmd/main.go"
echo ""
echo "To inspect the cluster manually:"
echo "  kubectl get quota -A"
echo "  kubectl get events -A --sort-by='.lastTimestamp'"
