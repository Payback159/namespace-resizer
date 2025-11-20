#!/bin/bash
set -e

# This script resets the demo repository to its initial clean state.
# It deletes the 'managed-resources' directory and recreates it with
# the initial demo files, then pushes the changes to origin.

REPO_DIR=$1

if [ -z "$REPO_DIR" ]; then
    echo "Usage: $0 <path-to-repo>"
    echo "Example: $0 ../ns-resizer-demo"
    exit 1
fi

if [ ! -d "$REPO_DIR/.git" ]; then
    echo "❌ Error: '$REPO_DIR' is not a git repository."
    exit 1
fi

BASE_DIR="$REPO_DIR/managed-resources/resizer-demo"

echo "🧹 Cleaning up '$BASE_DIR'..."
rm -rf "$BASE_DIR"
mkdir -p "$BASE_DIR"

echo "🏗️ Recreating Demo Scenarios..."

# ==========================================
# 1. Demo Standard (Metric-based)
# ==========================================
DIR="$BASE_DIR/demo-standard"
mkdir -p "$DIR"

cat <<EOF > "$DIR/namespace.yaml"
apiVersion: v1
kind: Namespace
metadata:
  name: demo-standard
  annotations:
    resizer.io/enabled: "true"
EOF

cat <<EOF > "$DIR/quota.yaml"
apiVersion: v1
kind: ResourceQuota
metadata:
  name: standard-quota
  namespace: demo-standard
spec:
  hard:
    requests.cpu: "1000m"
    requests.memory: "1Gi"
    limits.cpu: "2000m"
    limits.memory: "2Gi"
EOF

cat <<EOF > "$DIR/pod.yaml"
apiVersion: v1
kind: Pod
metadata:
  name: load-generator
  namespace: demo-standard
spec:
  containers:
  - name: stress
    image: busybox
    command: ["sh", "-c", "while true; do sleep 3600; done"]
    resources:
      requests:
        cpu: "850m" # 85% of 1000m -> Triggers resize (Threshold 80%)
        memory: "100Mi"
EOF

# ==========================================
# 2. Demo Custom (Custom Threshold)
# ==========================================
DIR="$BASE_DIR/demo-custom"
mkdir -p "$DIR"

cat <<EOF > "$DIR/namespace.yaml"
apiVersion: v1
kind: Namespace
metadata:
  name: demo-custom
  annotations:
    resizer.io/enabled: "true"
    resizer.io/cpu-threshold: "50" # Trigger at 50%
EOF

cat <<EOF > "$DIR/quota.yaml"
apiVersion: v1
kind: ResourceQuota
metadata:
  name: custom-quota
  namespace: demo-custom
spec:
  hard:
    requests.cpu: "1000m"
    requests.memory: "1Gi"
EOF

cat <<EOF > "$DIR/pod.yaml"
apiVersion: v1
kind: Pod
metadata:
  name: load-generator
  namespace: demo-custom
spec:
  containers:
  - name: stress
    image: busybox
    command: ["sh", "-c", "while true; do sleep 3600; done"]
    resources:
      requests:
        cpu: "600m" # 60% > 50% -> Triggers resize
        memory: "100Mi"
EOF

# ==========================================
# 3. Demo Opt-Out
# ==========================================
DIR="$BASE_DIR/demo-opt-out"
mkdir -p "$DIR"

cat <<EOF > "$DIR/namespace.yaml"
apiVersion: v1
kind: Namespace
metadata:
  name: demo-opt-out
  annotations:
    resizer.io/enabled: "false" # Controller should ignore this
EOF

cat <<EOF > "$DIR/quota.yaml"
apiVersion: v1
kind: ResourceQuota
metadata:
  name: ignored-quota
  namespace: demo-opt-out
spec:
  hard:
    requests.cpu: "1000m"
    requests.memory: "1Gi"
EOF

cat <<EOF > "$DIR/pod.yaml"
apiVersion: v1
kind: Pod
metadata:
  name: load-generator
  namespace: demo-opt-out
spec:
  containers:
  - name: stress
    image: busybox
    command: ["sh", "-c", "while true; do sleep 3600; done"]
    resources:
      requests:
        cpu: "900m" # 90% > 80% -> But ignored due to opt-out
        memory: "100Mi"
EOF

# ==========================================
# 4. Demo Burst (Event-based)
# ==========================================
DIR="$BASE_DIR/demo-burst"
mkdir -p "$DIR"

cat <<EOF > "$DIR/namespace.yaml"
apiVersion: v1
kind: Namespace
metadata:
  name: demo-burst
  annotations:
    resizer.io/enabled: "true"
EOF

cat <<EOF > "$DIR/quota.yaml"
apiVersion: v1
kind: ResourceQuota
metadata:
  name: burst-quota
  namespace: demo-burst
spec:
  hard:
    requests.cpu: "500m"
    requests.memory: "1Gi"
EOF

cat <<EOF > "$DIR/deployment.yaml"
apiVersion: apps/v1
kind: Deployment
metadata:
  name: burst-app
  namespace: demo-burst
spec:
  replicas: 1
  selector:
    matchLabels:
      app: burst
  template:
    metadata:
      labels:
        app: burst
    spec:
      containers:
      - name: app
        image: nginx
        resources:
          requests:
            cpu: "1000m" # 1000m > 500m -> FailedCreate Event -> Resize
            memory: "100Mi"
EOF

# ==========================================
# 5. Demo Storage
# ==========================================
DIR="$BASE_DIR/demo-storage"
mkdir -p "$DIR"

cat <<EOF > "$DIR/05-storage-pressure.yaml"
apiVersion: v1
kind: Namespace
metadata:
  name: demo-storage
  annotations:
    resizer.io/enabled: "true"
---
apiVersion: v1
kind: ResourceQuota
metadata:
  name: storage-quota
  namespace: demo-storage
spec:
  hard:
    requests.storage: "5Gi"
---
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: storage-stress
  namespace: demo-storage
spec:
  serviceName: "storage"
  replicas: 1
  selector:
    matchLabels:
      app: storage
  template:
    metadata:
      labels:
        app: storage
    spec:
      containers:
      - name: nginx
        image: nginx
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: [ "ReadWriteOnce" ]
      resources:
        requests:
          storage: 6Gi # 6Gi > 5Gi -> FailedCreate -> Resize
EOF

# ==========================================
# 6. Demo Dual Pressure
# ==========================================
DIR="$BASE_DIR/demo-dual-pressure"
mkdir -p "$DIR"

cat <<EOF > "$DIR/04-dual-pressure.yaml"
apiVersion: v1
kind: Namespace
metadata:
  name: demo-dual-pressure
  annotations:
    resizer.io/enabled: "true"
    resizer.io/threshold: "80"
---
apiVersion: v1
kind: ResourceQuota
metadata:
  name: dual-quota
  namespace: demo-dual-pressure
spec:
  hard:
    requests.cpu: "1000m"
    requests.memory: "1Gi"
    limits.cpu: "2000m"
    limits.memory: "2Gi"
---
apiVersion: v1
kind: Pod
metadata:
  name: dual-stress
  namespace: demo-dual-pressure
spec:
  containers:
  - name: stress
    image: busybox
    command: ["sh", "-c", "while true; do sleep 3600; done"]
    resources:
      requests:
        cpu: "900m"    # 90%
        memory: "900Mi" # ~88%
      limits:
        cpu: "1000m"
        memory: "1Gi"
EOF

# ==========================================
# 7. Demo Multi Burst
# ==========================================
DIR="$BASE_DIR/demo-multi-burst"
mkdir -p "$DIR"

cat <<EOF > "$DIR/namespace.yaml"
apiVersion: v1
kind: Namespace
metadata:
  name: demo-multi-burst
  annotations:
    resizer.io/enabled: "true"
EOF

cat <<EOF > "$DIR/quota.yaml"
apiVersion: v1
kind: ResourceQuota
metadata:
  name: multi-quota
  namespace: demo-multi-burst
spec:
  hard:
    requests.cpu: "500m"
    pods: "10"
EOF

cat <<EOF > "$DIR/deployment-a.yaml"
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app-a
  namespace: demo-multi-burst
spec:
  replicas: 1
  selector:
    matchLabels:
      app: app-a
  template:
    metadata:
      labels:
        app: app-a
    spec:
      containers:
      - name: app
        image: nginx
        resources:
          requests:
            cpu: "300m"
EOF

cat <<EOF > "$DIR/deployment-b.yaml"
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app-b
  namespace: demo-multi-burst
spec:
  replicas: 1
  selector:
    matchLabels:
      app: app-b
  template:
    metadata:
      labels:
        app: app-b
    spec:
      containers:
      - name: app
        image: nginx
        resources:
          requests:
            cpu: "300m" # 300+300 = 600 > 500 -> Resize
EOF

# ==========================================
# 8. Demo StatefulSet Burst
# ==========================================
DIR="$BASE_DIR/demo-statefulset-burst"
mkdir -p "$DIR"

cat <<EOF > "$DIR/namespace.yaml"
apiVersion: v1
kind: Namespace
metadata:
  name: demo-statefulset-burst
  annotations:
    resizer.io/enabled: "true"
EOF

cat <<EOF > "$DIR/quota.yaml"
apiVersion: v1
kind: ResourceQuota
metadata:
  name: sts-burst-quota
  namespace: demo-statefulset-burst
spec:
  hard:
    requests.cpu: "100m"
    requests.memory: "100Mi"
    requests.storage: "1Gi"
EOF

cat <<EOF > "$DIR/statefulset.yaml"
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: burst-sts
  namespace: demo-statefulset-burst
spec:
  serviceName: "burst"
  replicas: 3
  selector:
    matchLabels:
      app: burst
  template:
    metadata:
      labels:
        app: burst
    spec:
      containers:
      - name: nginx
        image: nginx:alpine
        resources:
          requests:
            cpu: "200m"
            memory: "100Mi"
  volumeClaimTemplates:
  - metadata:
      name: data
    spec:
      accessModes: [ "ReadWriteOnce" ]
      resources:
        requests:
          storage: 1Gi
EOF

# ==========================================
# 9. Demo ReplicaSet Burst
# ==========================================
DIR="$BASE_DIR/demo-replicaset-burst"
mkdir -p "$DIR"

cat <<EOF > "$DIR/namespace.yaml"
apiVersion: v1
kind: Namespace
metadata:
  name: demo-replicaset-burst
  annotations:
    resizer.io/enabled: "true"
EOF

cat <<EOF > "$DIR/quota.yaml"
apiVersion: v1
kind: ResourceQuota
metadata:
  name: rs-burst-quota
  namespace: demo-replicaset-burst
spec:
  hard:
    requests.cpu: "150m"
EOF

cat <<EOF > "$DIR/replicaset.yaml"
apiVersion: apps/v1
kind: ReplicaSet
metadata:
  name: burst-rs
  namespace: demo-replicaset-burst
spec:
  replicas: 5
  selector:
    matchLabels:
      app: burst
  template:
    metadata:
      labels:
        app: burst
    spec:
      containers:
      - name: nginx
        image: nginx:alpine
        resources:
          requests:
            cpu: "100m"
            memory: "50Mi"
EOF

echo "✅ Files recreated."

# Git Operations
echo "📤 Committing and Pushing to origin/main..."
git -C "$REPO_DIR" add .
git -C "$REPO_DIR" commit -m "chore: reset demo environment to initial state" || echo "No changes to commit."
git -C "$REPO_DIR" push origin main

echo "🎉 Reset Complete!"
