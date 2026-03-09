package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/shirou/gopsutil/v3/cpu"
)

// --- CONFIGURATION ---
var (
	TargetURL   string  // The actual OpenFaaS Node URL (e.g., http://192.168.64.2:8080)
	ProxyPort   int     // The port this proxy listens on (e.g., 8081)
	GossipPort  int     // The port used for internal gossip (e.g., 7946)
	BindAddr    string  // The IP address to bind Gossip to
	FakeLoad    float64 // For demo: force a specific CPU load
	OffloadAt   float64 // Offload when score >= this threshold
	AcceptBelow float64 // Only offload to peers below this score
	MaxInflight int64   // Soft local inflight threshold

	// State (atomics to avoid lock contention on hot path).
	MyCurrentLoadBits uint64
	LocalInflight     int64

	// Gossip cluster.
	list *memberlist.Memberlist

	// Reused HTTP client with pooled keep-alive connections.
	upstreamTransport = &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 256,
		IdleConnTimeout:     90 * time.Second,
	}
	upstreamClient = &http.Client{
		Timeout:   30 * time.Second,
		Transport: upstreamTransport,
	}
)

// NodeMeta is the data we whisper to other nodes.
type NodeMeta struct {
	ProxyAddress string  `json:"addr"` // How to reach me
	CPULoad      float64 `json:"cpu"`  // CPU usage
	Inflight     int64   `json:"if"`   // Number of in-flight requests
	Score        float64 `json:"score"`
}

func main() {
	// 1. Parse command-line arguments.
	flag.StringVar(&TargetURL, "target", "http://localhost:8080", "The real OpenFaaS node URL")
	flag.StringVar(&BindAddr, "bind", "127.0.0.1", "IP address for Gossip to bind to")
	flag.IntVar(&ProxyPort, "port", 8081, "HTTP port for this proxy")
	flag.IntVar(&GossipPort, "gossip", 7946, "Gossip port")
	flag.Float64Var(&FakeLoad, "fake-load", 0.0, "Force a fake CPU load (0-100)")
	flag.Float64Var(&OffloadAt, "offload-at", 60.0, "Offload when local score exceeds this value")
	flag.Float64Var(&AcceptBelow, "accept-below", 55.0, "Only offload to peers with score below this value")
	flag.Int64Var(&MaxInflight, "max-inflight", 8, "Soft local inflight threshold")
	joinAddr := flag.String("join", "", "Address of a peer to join (e.g., 127.0.0.1:7947)")
	flag.Parse()

	if MaxInflight < 1 {
		MaxInflight = 1
	}

	// 2. Start the gossip protocol.
	startGossip(*joinAddr)

	// 3. Start the CPU monitor (runs in background).
	go monitorCPU()

	// 4. Start the HTTP proxy server.
	http.HandleFunc("/", handleRequest)

	fmt.Printf("--------------------------------------------------\n")
	fmt.Printf("Proxy Started on Port :%d\n", ProxyPort)
	fmt.Printf("Forwarding to Target  : %s\n", TargetURL)
	fmt.Printf("Gossip Port           : %d\n", GossipPort)
	if FakeLoad > 0 {
		fmt.Printf("DEMO MODE             : Forcing Load to %.0f%%\n", FakeLoad)
	}
	fmt.Printf("Offload Threshold     : %.1f (accept peers < %.1f)\n", OffloadAt, AcceptBelow)
	fmt.Printf("Max Inflight          : %d\n", MaxInflight)
	fmt.Printf("--------------------------------------------------\n")

	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", ProxyPort), nil))
}

// --- GOSSIP LOGIC ---

func startGossip(joinAddr string) {
	config := memberlist.DefaultLANConfig()
	config.BindPort = GossipPort
	config.BindAddr = BindAddr
	config.Name = fmt.Sprintf("Node-Port-%d", ProxyPort)
	config.Delegate = &GossipDelegate{}

	var err error
	list, err = memberlist.Create(config)
	if err != nil {
		panic("Failed to create gossip list: " + err.Error())
	}

	if joinAddr != "" {
		_, err := list.Join([]string{joinAddr})
		if err != nil {
			panic("Failed to join cluster: " + err.Error())
		}
		fmt.Println(">> Successfully joined the cluster!")
	}
}

// GossipDelegate defines what data we share.
type GossipDelegate struct{}

func (d *GossipDelegate) NodeMeta(limit int) []byte {
	load := atomicLoadFloat64(&MyCurrentLoadBits)
	inflight := atomic.LoadInt64(&LocalInflight)

	meta := NodeMeta{
		ProxyAddress: fmt.Sprintf("http://127.0.0.1:%d", ProxyPort),
		CPULoad:      load,
		Inflight:     inflight,
		Score:        localScore(load, inflight),
	}
	b, _ := json.Marshal(meta)
	return b
}

// Boilerplate methods required by memberlist.
func (d *GossipDelegate) NotifyMsg(b []byte)                         {}
func (d *GossipDelegate) GetBroadcasts(overhead, limit int) [][]byte { return nil }
func (d *GossipDelegate) LocalState(join bool) []byte                { return nil }
func (d *GossipDelegate) MergeRemoteState(buf []byte, join bool)     {}

// --- CPU MONITORING ---

func monitorCPU() {
	for {
		var load float64

		if FakeLoad > 0 {
			load = FakeLoad
		} else {
			percent, err := cpu.Percent(0, false)
			if err == nil && len(percent) > 0 {
				load = percent[0]
			}
		}

		atomicStoreFloat64(&MyCurrentLoadBits, load)
		list.UpdateNode(50 * time.Millisecond)

		// Faster refresh gives fresher peer decisions under bursts.
		time.Sleep(500 * time.Millisecond)
	}
}

// --- HTTP PROXY LOGIC ---

func handleRequest(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt64(&LocalInflight, 1)
	defer atomic.AddInt64(&LocalInflight, -1)

	cpuLoad := atomicLoadFloat64(&MyCurrentLoadBits)
	inflight := atomic.LoadInt64(&LocalInflight)
	score := localScore(cpuLoad, inflight)

	// Allow at most one offload hop to avoid ping-pong.
	offloadHops, _ := strconv.Atoi(r.Header.Get("X-Gossip-Offload-Hop"))
	if offloadHops == 0 && score >= OffloadAt {
		bestNode := findBestPeer()
		if bestNode != "" {
			fmt.Printf("[OFFLOAD] CPU %.0f%% score %.1f inflight %d -> %s\n", cpuLoad, score, inflight, bestNode)
			r.Header.Set("X-Gossip-Offload-Hop", "1")
			proxyRequest(w, r, bestNode)
			return
		}
	}

	proxyRequest(w, r, TargetURL)
}

func findBestPeer() string {
	members := list.Members()

	// --- Shuffle the members to break ties ---
	shuffled := make([]*memberlist.Node, len(members))
	copy(shuffled, members)

	rand.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	var bestAddr string
	bestScore := math.MaxFloat64

	for _, member := range members {
		if member.Name == list.LocalNode().Name {
			continue
		}

		var meta NodeMeta
		if err := json.Unmarshal(member.Meta, &meta); err != nil {
			continue
		}
		if meta.ProxyAddress == "" {
			continue
		}
		if meta.Score < AcceptBelow && meta.Score < bestScore {
			bestScore = meta.Score
			bestAddr = meta.ProxyAddress
		}
	}
	return bestAddr
}

func proxyRequest(w http.ResponseWriter, r *http.Request, target string) {
	finalURL := target + r.URL.RequestURI()

	req, err := http.NewRequestWithContext(r.Context(), r.Method, finalURL, r.Body)
	if err != nil {
		http.Error(w, "Failed to create request", http.StatusInternalServerError)
		return
	}

	// Clone inbound headers and drop hop-by-hop proxy headers.
	req.Header = r.Header.Clone()
	req.Header.Del("Connection")
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Keep-Alive")
	req.Header.Del("Transfer-Encoding")
	req.Header.Del("TE")
	req.Header.Del("Trailer")
	req.Header.Del("Upgrade")

	resp, err := upstreamClient.Do(req)
	if err != nil {
		http.Error(w, "Upstream Error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for name, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func localScore(cpuLoad float64, inflight int64) float64 {
	queuePressure := math.Min(100.0, float64(inflight)*100.0/float64(MaxInflight))
	// CPU is primary, queue pressure helps react faster during spikes.
	return 0.7*cpuLoad + 0.3*queuePressure
}

func atomicStoreFloat64(dst *uint64, v float64) {
	atomic.StoreUint64(dst, math.Float64bits(v))
}

func atomicLoadFloat64(src *uint64) float64 {
	return math.Float64frombits(atomic.LoadUint64(src))
}
