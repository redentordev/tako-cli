//go:build testlb
// +build testlb

package main

import (
	"fmt"
	"os"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

func main() {
	// Load config
	cfg, err := config.LoadConfig("tako.yaml")
	if err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Get server1 config
	server := cfg.Servers["server1"]

	// Connect
	client, err := ssh.NewClient(server.Host, server.Port, server.User, server.SSHKey)
	if err != nil {
		fmt.Printf("Error connecting: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	fmt.Println("Testing load balancing with 15 requests...")
	fmt.Println("If load balancing works, you should see 3 different hostnames\n")

	hostnames := make(map[string]int)

	for i := 1; i <= 15; i++ {
		output, err := client.Execute("curl -s http://localhost:3000/api/instance 2>&1")
		if err != nil {
			fmt.Printf("Request %d: Error - %v\n", i, err)
			continue
		}

		// Extract hostname using grep
		hostname, _ := client.Execute("echo '" + output + "' | grep -o '\"hostname\":\"[^\"]*\"' | cut -d'\"' -f4")
		hostname = hostname[:len(hostname)-1] // Remove trailing newline

		if hostname != "" {
			hostnames[hostname]++
			fmt.Printf("Request %2d: %s\n", i, hostname)
		} else {
			fmt.Printf("Request %2d: Could not parse hostname from: %s\n", i, output[:100])
		}

		time.Sleep(200 * time.Millisecond)
	}

	fmt.Println("\n=== Load Balancing Summary ===")
	fmt.Printf("Total unique containers: %d\n", len(hostnames))
	fmt.Println("\nRequest distribution:")
	for hostname, count := range hostnames {
		fmt.Printf("  %s: %d requests\n", hostname, count)
	}

	if len(hostnames) >= 3 {
		fmt.Println("\n✓ Load balancing is working! Requests distributed across all replicas.")
	} else if len(hostnames) == 1 {
		fmt.Println("\n✗ Load balancing NOT working - all requests hit the same container.")
	} else {
		fmt.Printf("\n⚠ Partial load balancing - only %d/%d replicas receiving traffic.\n", len(hostnames), 3)
	}
}
