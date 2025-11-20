#!/bin/bash

# This script simulates an ArgoCD application sync.
# It periodically pulls the latest changes from the git repository
# and applies them to the Kubernetes cluster.

REPO_DIR=$1
INTERVAL=${2:-10}

if [ -z "$REPO_DIR" ]; then
    echo "Usage: $0 <path-to-repo> [interval]"
    echo "Example: $0 ../ns-resizer-demo 10"
    exit 1
fi

if [ ! -d "$REPO_DIR/.git" ]; then
    echo "❌ Error: '$REPO_DIR' is not a git repository."
    exit 1
fi

echo "🔄 Starting GitOps Sync Loop (ArgoCD Replacement)"
echo "   Repo: $REPO_DIR"
echo "   Interval: ${INTERVAL}s"
echo "------------------------------------------------"

while true; do
    echo "[$(date +%T)] 📥 Git Pull..."
    
    # Capture output to avoid spamming if nothing changes, 
    # but we want to see if it updates.
    OUTPUT=$(git -C "$REPO_DIR" pull 2>&1)
    EXIT_CODE=$?
    
    if [ $EXIT_CODE -ne 0 ]; then
        echo "❌ Git pull failed:"
        echo "$OUTPUT"
    else
        if [[ "$OUTPUT" == *"Already up to date."* ]]; then
            echo "   No changes detected."
        else
            echo "   Changes detected:"
            echo "$OUTPUT"
            
            echo "[$(date +%T)] 🚀 Applying resources..."
            # We apply the specific folder structure we know exists in ns-resizer-demo
            if [ -d "$REPO_DIR/managed-resources/resizer-demo" ]; then
                kubectl apply -R -f "$REPO_DIR/managed-resources/resizer-demo/"
            else
                echo "⚠️ Warning: Directory '$REPO_DIR/managed-resources/resizer-demo' not found. Applying root..."
                kubectl apply -R -f "$REPO_DIR/"
            fi
        fi
    fi
    
    sleep $INTERVAL
done
