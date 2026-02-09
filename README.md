# Decentralized Edge Cluster with Gossip Load Balancing

## Project Overview
This project simulates a resource-constrained **Edge Computing** environment to demonstrate the "Silo Effect", where a single node becomes saturated while its neighbors remain idle.

To solve this, we implemented a **Decentralized Sidecar Proxy** using a Gossip Protocol (HashiCorp Memberlist). This proxy allows isolated `faasd` nodes to discover each other and intelligently offload CPU-intensive tasks to idle peers without a central master node.

---

## 1. Prerequisites & Tool Installation (macOS)

The following tools are required on the host machine.

### Install Multipass (VM Manager)
Used to provision Ubuntu VMs that act as Edge nodes.
```bash
brew install --cask multipass
```

### Install `hey` (Load Generator)
Used to flood the system with traffic for stress testing.
```bash
brew install hey
```

### Install `faas-cli` (OpenFaaS CLI)
Used to deploy functions to the nodes.
```bash
# Install arkade:
curl -sSL https://get.arkade.dev | sudo -E sh

# Install faas-cli:
arkade get faas-cli
```

### Install Go (Golang)
Used to run the Gossip Proxy sidecar.
```bash
brew install go
```

---

## 2. Infrastructure Setup

We provision two Ubuntu VMs (`node1` and `node2`) with limited resources (2 CPUs, 2GB RAM) to mimic Edge Gateways.

### Provision Node 1
```bash
# Sudo is used to avoid permission issues with local certificates on some macOS setups
sudo multipass launch --name node1 --cpus 2 --mem 2G --disk 10G
```

### Provision Node 2
```bash
sudo multipass launch --name node2 --cpus 2 --mem 2G --disk 10G
```

---

## 3. Runtime Configuration (faasd)

We install `faasd`, a lightweight serverless runtime, on both nodes.

### Configure Node 1
1.  **Shell into the VM:**
    ```bash
    sudo multipass shell node1
    ```
2.  **Install faasd:**
    ```bash
    curl -sfL https://raw.githubusercontent.com/openfaas/faasd/master/hack/install.sh | sudo -E bash
    ```
3.  **Retrieve Credentials & IP:**
    Save the output of these commands for later use:
    ```bash
    sudo cat /var/lib/faasd/secrets/basic-auth-password
    hostname -I
    ```
    *(Example Node 1 IP: `192.168.64.2`)*
4.  **Exit:** `exit`

### Configure Node 2
Repeat the steps for Node 2.
1.  `sudo multipass shell node2`
2.  Run the install script.
3.  Get Password and IP (`192.168.64.3`).

---

## 4. Function Deployment

We deploy a CPU-intensive Python function (`stress-test`) to both nodes.

**1. Create `stack.yaml`**
Ensure your function configuration includes scaling labels to utilize all CPU cores (will change labels for performance optimization as next steps):
```yaml
functions:
  stress-test:
    lang: python3-http
    handler: ./stress-test
    image: jfan981/stress-test:v5-final
    labels:
      com.openfaas.scale.min: 5
      com.openfaas.scale.max: 5
```

**2. Deploy to Node 1**
```bash
export OPENFAAS_URL=http://192.168.64.2:8080
echo -n <NODE_1_PASSWORD> | faas-cli login --password-stdin
faas-cli deploy -f stack.yaml
```

**3. Deploy to Node 2**
```bash
export OPENFAAS_URL=http://192.168.64.3:8080
echo -n <NODE_2_PASSWORD> | faas-cli login --password-stdin
faas-cli deploy -f stack.yaml
```

---

## 5. Baseline Testing (The "Failure" Scenario)

We establish a baseline by flooding Node 1 directly to demonstrate the "Silo Effect."

**Command:**
```bash
hey -n 100 -c 20 -d "150000" http://192.168.64.2:8080/function/stress-test
```

**Baseline Results:**
*  Total:        27.3915 secs
*  Slowest:      6.2998 secs
*  Fastest:      1.0889 secs
*  Average:      4.9511 secs
*  Requests/sec: 3.6508

---

## 6. The Solution: Gossip Proxy

We implement a Go-based sidecar that intercepts requests and offloads them if the node is overloaded.

### Setup
```bash
mkdir -p gossip-proxy
cd gossip-proxy
go mod init gossip-proxy
go get github.com/hashicorp/memberlist
go get github.com/shirou/gopsutil/v3/cpu
```

### The Code (`main.go`)
Create a `main.go` file (see project source code) with the following logic:
1.  **Gossip:** Uses `memberlist` to broadcast CPU load to peers.
2.  **Monitor:** Checks local CPU usage (or accepts a `--fake-load` flag for demo purposes).
3.  **Proxy:** If Load > 50%, forward the request to a peer with < 50% load.

### Running the Cluster Simulation
Open two new terminal windows to simulate the sidecars running on the nodes.

**Terminal 1 (Proxy for Node 1 - "Overloaded")**
This proxy forces a fake load of 85%, so it *always* attempts to offload work.
```bash
go run main.go \
  --target="http://192.168.64.2:8080" \
  --port=8081 \
  --gossip=7946 \
  --fake-load=85 \
  --join="127.0.0.1:7947"
```

**Terminal 2 (Proxy for Node 2 - "Idle")**
This proxy forces a fake load of 0%, so it *always* accepts work.
```bash
go run main.go \
  --target="http://192.168.64.3:8080" \
  --port=8082 \
  --gossip=7947 \
  --join="127.0.0.1"
```

---

## 7. Verification Results

We test the system again, but this time sending traffic to the **Proxy** (Port 8081) instead of the Node directly.

**Command:**
```bash
hey -n 100 -c 20 -d "150000" http://localhost:8081/function/stress-test
```

**Results:**
* **Success:** 100% `200 OK` (No failures/timeouts).
* **Fastest Response:** **0.37 seconds** (vs 1.0s baseline).
    * *Significance:* This proves offloading occurred; the request skipped the local queue and was executed instantly on the idle node.
* **Throughput:** Increased by **~48%**.
* **Logs:** Terminal 1 reported `[OFFLOAD] My Load 85% -> Forwarding to Node-8082`.

## Conclusion
The Gossip Proxy successfully transformed two isolated `faasd` nodes into a cooperative cluster. By offloading traffic from the saturated node to the idle one, we significantly reduced tail latency and increased aggregate throughput without requiring a central master node (like Kubernetes).