package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/shirou/gopsutil/v3/cpu"
)

// --- CONFIGURATION ---
var (
	TargetURL    string  // The actual OpenFaaS Node URL (e.g., http://192.168.64.2:8080)
	ProxyPort    int     // The port this proxy listens on (e.g., 8081)
	GossipPort   int     // The port used for internal gossip (e.g., 7946)
	FakeLoad     float64 // For demo: Force a specific CPU load
	
	// State
	MyCurrentLoad float64
	LoadLock      sync.RWMutex // Protects MyCurrentLoad
	
	// Gossip Cluster
	list         *memberlist.Memberlist
	LocalMeta    NodeMeta
)

// NodeMeta is the data we whisper to other nodes
type NodeMeta struct {
	ProxyAddress string  `json:"addr"` // How to reach me
	CPULoad      float64 `json:"load"` // My current health
}

func main() {
	// 1. Parse Command Line Arguments
	flag.StringVar(&TargetURL, "target", "http://localhost:8080", "The real OpenFaaS node URL")
	flag.IntVar(&ProxyPort, "port", 8081, "HTTP port for this proxy")
	flag.IntVar(&GossipPort, "gossip", 7946, "Gossip port")
	flag.Float64Var(&FakeLoad, "fake-load", 0.0, "Force a fake CPU load (0-100)")
	joinAddr := flag.String("join", "", "Address of a peer to join (e.g., 127.0.0.1)")
	flag.Parse()

	// 2. Start the Gossip Protocol
	startGossip(*joinAddr)

	// 3. Start the CPU Monitor (Runs in background)
	go monitorCPU()

	// 4. Start the HTTP Proxy Server
	http.HandleFunc("/", handleRequest)
	
	fmt.Printf("--------------------------------------------------\n")
	fmt.Printf("Proxy Started on Port :%d\n", ProxyPort)
	fmt.Printf("Forwarding to Target  : %s\n", TargetURL)
	fmt.Printf("Gossip Port           : %d\n", GossipPort)
	if FakeLoad > 0 {
		fmt.Printf("DEMO MODE             : Forcing Load to %.0f%%\n", FakeLoad)
	}
	fmt.Printf("--------------------------------------------------\n")
	
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", ProxyPort), nil))
}

// --- GOSSIP LOGIC ---

func startGossip(joinAddr string) {
	// Configure Memberlist (Standard LAN config is fine)
	config := memberlist.DefaultLANConfig()
	config.BindPort = GossipPort
	config.Name = fmt.Sprintf("Node-Port-%d", ProxyPort) // Unique Name
	config.Delegate = &GossipDelegate{} // Hook up our custom data sharing

	var err error
	list, err = memberlist.Create(config)
	if err != nil {
		panic("Failed to create gossip list: " + err.Error())
	}

	// If we were told to join an existing cluster, do it now
	if joinAddr != "" {
		_, err := list.Join([]string{joinAddr})
		if err != nil {
			panic("Failed to join cluster: " + err.Error())
		}
		fmt.Println(">> Successfully joined the cluster!")
	}
}

// GossipDelegate defines what data we share
type GossipDelegate struct{}

func (d *GossipDelegate) NodeMeta(limit int) []byte {
	// This function is called when we gossip to others.
	// We send our current load and our address.
	LoadLock.RLock()
	defer LoadLock.RUnlock()
	
	meta := NodeMeta{
		ProxyAddress: fmt.Sprintf("http://127.0.0.1:%d", ProxyPort),
		CPULoad:      MyCurrentLoad,
	}
	b, _ := json.Marshal(meta)
	return b
}

// Boilerplate methods required by the library (we don't use them)
func (d *GossipDelegate) NotifyMsg(b []byte) {}
func (d *GossipDelegate) GetBroadcasts(overhead, limit int) [][]byte { return nil }
func (d *GossipDelegate) LocalState(join bool) []byte { return nil }
func (d *GossipDelegate) MergeRemoteState(buf []byte, join bool) {}

// --- CPU MONITORING ---

func monitorCPU() {
	for {
		var load float64
		
		if FakeLoad > 0 {
			// Demo Mode: Use the fake number
			load = FakeLoad
		} else {
			// Real Mode: Read actual system CPU
			percent, err := cpu.Percent(0, false)
			if err == nil && len(percent) > 0 {
				load = percent[0]
			}
		}

		// Update global state
		LoadLock.Lock()
		MyCurrentLoad = load
		LoadLock.Unlock()

		// Force the gossip library to refresh our node's data immediately
		list.UpdateNode(100 * time.Millisecond)

		// Check every 2 seconds
		time.Sleep(2 * time.Second)
	}
}

// --- HTTP PROXY LOGIC ---

func handleRequest(w http.ResponseWriter, r *http.Request) {
	LoadLock.RLock()
	currentLoad := MyCurrentLoad
	LoadLock.RUnlock()

	// THRESHOLD: If CPU > 50%, try to offload
	if currentLoad > 50.0 {
		bestNode := findBestPeer()
		if bestNode != "" {
			fmt.Printf("[OFFLOAD] My Load %.0f%% -> Forwarding to %s\n", currentLoad, bestNode)
			proxyRequest(w, r, bestNode)
			return
		}
		fmt.Println("[WARNING] Overloaded, but no idle peers found!")
	}

	// Default: Handle locally
	// fmt.Printf("[LOCAL] Handling Request (Load: %.0f%%)\n", currentLoad)
	proxyRequest(w, r, TargetURL)
}

func findBestPeer() string {
	members := list.Members()
	for _, member := range members {
		// Skip myself
		if member.Name == list.LocalNode().Name {
			continue
		}

		// Decode their metadata
		var meta NodeMeta
		if err := json.Unmarshal(member.Meta, &meta); err == nil {
			// If they are idle (Load < 50%), send it to them!
			if meta.CPULoad < 50.0 {
				return meta.ProxyAddress
			}
		}
	}
	return ""
}

func proxyRequest(w http.ResponseWriter, r *http.Request, target string) {
	// Construct the forwarding URL
	finalURL := target + r.URL.Path
	
	// Create a new request
	req, err := http.NewRequest(r.Method, finalURL, r.Body)
	if err != nil {
		http.Error(w, "Failed to create request", 500)
		return
	}

	// Copy headers (Important for Content-Type)
	for name, values := range r.Header {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}

	// Execute the request
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Upstream Error: "+err.Error(), 502)
		return
	}
	defer resp.Body.Close()

	// Copy response headers and body back to the user
	for name, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}