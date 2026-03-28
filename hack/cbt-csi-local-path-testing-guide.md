# Kubernetes CSI HostPath Driver: CBT Testing Guide

This guide covers the end-to-end process of setting up the `csi-driver-host-path` on a `kind` cluster to test **Changed Block Tracking (CBT)** features.

---

## 1. Cluster Setup (`kind`)
Create a `kind` cluster configuration. While not strictly required, mounting a host path can help you inspect the raw block data later.

```yaml
# cluster-config.yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraMounts:
    - hostPath: /tmp/csi-data
      containerPath: /var/lib/csi-hostpath-data
- role: worker
```

**Command:**

```bash
# create cluster
kind create cluster --config cluster-config.yaml --name cbtcluster
# destroy cluster
kind delete cluster --name cbtcluster
```

---

## 2. Installation with CBT Support
CBT is an Alpha feature and requires the `external-snapshot-metadata` sidecar.

1. **Install Snapshot CRDs & Controller:**

```bash
# Install standard Snapshot CRDs (required by CSI hostpath driver)
kubectl apply -f https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/master/client/config/crd/snapshot.storage.k8s.io_volumesnapshotclasses.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/master/client/config/crd/snapshot.storage.k8s.io_volumesnapshotcontents.yaml
kubectl apply -f https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/master/client/config/crd/snapshot.storage.k8s.io_volumesnapshots.yaml

# Install the Snapshot Controller
kubectl apply -k https://github.com/kubernetes-csi/external-snapshotter/deploy/kubernetes/snapshot-controller

# Install CBT Snapshot Metadata CRDs
kubectl apply -f https://raw.githubusercontent.com/kubernetes-csi/external-snapshot-metadata/v0.2.0/client/config/crd/cbt.storage.k8s.io_snapshotmetadataservices.yaml
```

2. **Deploy HostPath Driver:**
   Clone the repo and use the specific environment variable to enable the metadata service.

```bash
git clone https://github.com/kubernetes-csi/csi-driver-host-path.git
cd csi-driver-host-path

# Patch the sidecar configuration to expose the HTTP endpoint and fix the readiness probe
sed -i 's|path: /health|path: /metrics|' deploy/kubernetes-latest/hostpath/csi-snapshot-metadata-sidecar.patch
sed -i '/--tls-key=\/tmp\/certificates\/tls.key/a \          - "--http-endpoint=:8080"' deploy/kubernetes-latest/hostpath/csi-snapshot-metadata-sidecar.patch

# Enable the Snapshot Metadata Service during deployment
SNAPSHOT_METADATA_TESTS=true ./deploy/kubernetes-latest/deploy.sh
```

---

## 3. Usage Samples (Block Mode)
CBT **only** works with volumes in `Block` mode.

### A. StorageClass & PVC

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: csi-hostpath-sc
provisioner: hostpath.csi.k8s.io
reclaimPolicy: Delete
volumeBindingMode: Immediate

---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: cbt-block-pvc
spec:
  accessModes: ["ReadWriteOnce"]
  volumeMode: Block
  resources:
    requests:
      storage: 1Gi
  storageClassName: csi-hostpath-sc
```

### B. Testing App (Data Writer)

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: cbt-writer
spec:
  containers:
    - name: writer
      image: ubuntu
      command: ["/bin/bash", "-c", "sleep infinity"]
      volumeDevices:
        - devicePath: /dev/xvda
          name: block-vol
  volumes:
    - name: block-vol
      persistentVolumeClaim:
        claimName: cbt-block-pvc
```

---

## 4. Step-by-Step CBT Workflow

1. **Write Initial Data & Snapshot 1:**

```bash
# Write 10MB to the start (use conv=fsync to ensure data is flushed before the snapshot)
kubectl exec cbt-writer -- dd if=/dev/urandom of=/dev/xvda bs=1M count=10 conv=fsync

# Create Base Snapshot
kubectl apply -f - <<EOF
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: snapshot-base
spec:
  volumeSnapshotClassName: csi-hostpath-snapclass
  source:
    persistentVolumeClaimName: cbt-block-pvc
EOF
```

2. **Modify Data & Snapshot 2:**

```bash
# Write 5MB starting at a 50MB offset (use conv=fsync to ensure data is flushed before the snapshot)
kubectl exec cbt-writer -- dd if=/dev/urandom of=/dev/xvda bs=1M count=5 seek=50 conv=fsync

# Create Delta Snapshot
kubectl apply -f - <<EOF
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: snapshot-delta
spec:
  volumeSnapshotClassName: csi-hostpath-snapclass
  source:
    persistentVolumeClaimName: cbt-block-pvc
EOF
```

---

## 5. Troubleshooting & Verification

### Check Logs
Verify the metadata sidecar is running and listening:

```bash
kubectl logs csi-hostpathplugin-0  -c csi-snapshot-metadata
```

### Block Volume Attach Failures (`losetup` failed)
If your `cbt-writer` pod gets stuck in `ContainerCreating`, and `kubectl describe pod cbt-writer` shows an error like:
> `MapVolume.MapBlockVolume failed... makeLoopDevice failed for path... losetup -f ... failed: exit status 1`

This happens because the `kind` worker node is running out of available loop devices (`/dev/loopX`). Containers in `kind` have a static snapshot of `/dev`, meaning when Kubelet tries to request a new loop device dynamically, it cannot find it inside the container.

To fix this, you can manually create the loop devices inside the `kind` worker node using `docker exec`:

```bash
# Create loop devices 0 through 100 inside the worker container
docker exec cbtcluster-worker bash -c 'for i in {0..100}; do [ -e /dev/loop$i ] || mknod /dev/loop$i b 7 $i; done'

# If the pod was stuck for a while, delete and recreate it to bypass kubelet's backoff timer
kubectl delete pod cbt-writer --force
kubectl apply -f cbt-writer.yml # (Or re-run the pod creation YAML)
```

### Expired TLS Certificates
If you encounter a `transport: authentication handshake failed: tls: failed to verify certificate: x509: certificate has expired or is not yet valid` error when running the `lister` tool, the hardcoded TLS certificates in the `csi-driver-host-path` repository tests have likely expired. 

To fix this, you can generate a new self-signed certificate, update the Kubernetes Secret, and patch the `SnapshotMetadataService` CustomResource with the new certificate authority:

```bash
# 1. Generate a new self-signed certificate valid for 1 year
openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
  -keyout /tmp/tls.key -out /tmp/tls.crt \
  -subj "/CN=csi-snapshot-metadata.default" \
  -addext "subjectAltName = DNS:csi-snapshot-metadata.default.svc, DNS:csi-snapshot-metadata.default"

# 2. Update the TLS secret in the cluster
kubectl create secret tls csi-snapshot-metadata-certs \
  --cert=/tmp/tls.crt --key=/tmp/tls.key \
  --dry-run=client -o yaml | kubectl apply -f -

# 3. Patch the SnapshotMetadataService with the new CA cert
cert_b64=$(cat /tmp/tls.crt | base64 -w0)
kubectl patch snapshotmetadataservices hostpath.csi.k8s.io \
  -p "{\"spec\": {\"caCert\": \"$cert_b64\"}}" --type=merge

# 4. Restart the CSI driver pod to pick up the new certificates
kubectl delete pod csi-hostpathplugin-0
```

### Query using the snapshot-metadata-lister (Recommended)
Instead of using `grpcurl` directly (which requires manual protobuf handling, TokenReview auth tokens, and TLS setup), the `external-snapshot-metadata` repo provides a `lister` tool designed exactly for this.

1. **Build the `lister` tool locally:**

```bash
git clone https://github.com/kubernetes-csi/external-snapshot-metadata.git
cd external-snapshot-metadata/tools/snapshot-metadata-lister
GOOS=linux go build -o /tmp/lister .
```

2. **Copy the tool into your writer pod:**

```bash
# Make sure your default service account has the necessary permissions (or run it from a dedicated RBAC pod)
kubectl create clusterrolebinding default-admin --clusterrole=cluster-admin --serviceaccount=default:default

# Copy it into the writer pod
kubectl cp /tmp/lister cbt-writer:/tmp/lister
kubectl exec cbt-writer -- chmod +x /tmp/lister
```

3. **Query the Changed Block Tracking (CBT) Delta:**

```bash
# This will automatically resolve the metadata service DNS, authenticate, and query the delta
kubectl exec cbt-writer -- /tmp/lister -n default -s snapshot-delta -p snapshot-base -o json
```
