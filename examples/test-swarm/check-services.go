//go:build checkservices
// +build checkservices

package main

import (
	"fmt"
	"os"

	"github.com/redentordev/tako-cli/pkg/ssh"
)

func main() {
	// Connect to server1
	client, err := ssh.NewClient("95.216.194.236", 22, "root", "~/.ssh/id_rsa")
	if err != nil {
		fmt.Printf("Error connecting: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	// Check what services are running
	fmt.Println("=== Docker Services ===")
	output, _ := client.Execute("docker service ls")
	fmt.Println(output)

	// Check what's using ports 80, 443, 8080
	fmt.Println("\n=== Port Usage ===")
	output, _ = client.Execute("docker ps --format '{{.Names}}\t{{.Ports}}' | grep -E ':(80|443|8080)->'")
	fmt.Println(output)

	// Check if Traefik container is running
	fmt.Println("\n=== Traefik Containers ===")
	output, _ = client.Execute("docker ps | grep traefik")
	fmt.Println(output)

	// Try to create traefik service manually to see error
	fmt.Println("\n=== Testing Traefik Service Creation ===")
	output, err = client.Execute("docker service create --detach --name traefik-test --network tako_swarm-test_production --constraint node.role==manager --publish published=8081,target=8080,mode=ingress --replicas 1 traefik:v3.0 --api.insecure=true 2>&1")
	fmt.Printf("Output: %s\n", output)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
	}
}
