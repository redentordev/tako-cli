package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var proxyServer string

type proxyPortSpec struct {
	LocalPort  int
	RemotePort int
}

type proxyTarget struct {
	serverName string
	server     config.ServerConfig
	client     *ssh.Client
	response   takod.ProxyTargetResponse
}

var proxyCmd = &cobra.Command{
	Use:   "proxy SERVICE [LOCAL_PORT:]REMOTE_PORT",
	Short: "Proxy a private service port to localhost",
	Long: `Proxy a service port in the takod mesh to a local loopback port.

The command selects a running healthy service replica on the requested node. If
--server is omitted, Tako checks environment nodes in configured order and uses
the first reachable node with a healthy target.

Examples:
  tako proxy postgres 5432
  tako proxy postgres 15432:5432
  tako proxy --server prod-a api 3000`,
	Args: cobra.ExactArgs(2),
	RunE: runProxy,
}

func init() {
	rootCmd.AddCommand(proxyCmd)
	proxyCmd.Flags().StringVarP(&proxyServer, "server", "s", "", "Node to proxy through")
}

func runProxy(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	serviceName := args[0]
	if _, ok := services[serviceName]; !ok {
		return fmt.Errorf("service %s not found in environment %s", serviceName, envName)
	}

	ports, err := parseProxyPortSpec(args[1])
	if err != nil {
		return err
	}

	pool := ssh.NewPool()
	defer pool.CloseAll()

	target, err := resolveProxyTargetFromMesh(pool, cfg, envName, serviceName, ports.RemotePort, proxyServer)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return runLocalServiceProxy(ctx, target, ports.LocalPort)
}

func parseProxyPortSpec(raw string) (proxyPortSpec, error) {
	parts := strings.Split(raw, ":")
	switch len(parts) {
	case 1:
		remotePort, err := parseProxyPort(parts[0], false)
		if err != nil {
			return proxyPortSpec{}, fmt.Errorf("invalid remote port: %w", err)
		}
		return proxyPortSpec{RemotePort: remotePort}, nil
	case 2:
		localPort, err := parseProxyPort(parts[0], true)
		if err != nil {
			return proxyPortSpec{}, fmt.Errorf("invalid local port: %w", err)
		}
		remotePort, err := parseProxyPort(parts[1], false)
		if err != nil {
			return proxyPortSpec{}, fmt.Errorf("invalid remote port: %w", err)
		}
		return proxyPortSpec{LocalPort: localPort, RemotePort: remotePort}, nil
	default:
		return proxyPortSpec{}, fmt.Errorf("port must use REMOTE_PORT or LOCAL_PORT:REMOTE_PORT")
	}
}

func parseProxyPort(raw string, allowZero bool) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, fmt.Errorf("port is required")
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	min := 1
	if allowZero {
		min = 0
	}
	if port < min || port > 65535 {
		if allowZero {
			return 0, fmt.Errorf("port must be between 0 and 65535")
		}
		return 0, fmt.Errorf("port must be between 1 and 65535")
	}
	return port, nil
}

func resolveProxyTargetFromMesh(
	pool *ssh.Pool,
	cfg *config.Config,
	envName string,
	serviceName string,
	remotePort int,
	requestedServer string,
) (*proxyTarget, error) {
	serverNames, err := statePullServerNames(cfg, envName, requestedServer)
	if err != nil {
		return nil, err
	}

	var failures []string
	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			failures = append(failures, fmt.Sprintf("%s: server not found in configuration", serverName))
			continue
		}
		client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: connect failed: %v", serverName, err))
			continue
		}

		endpoint := takodclient.ProxyTargetEndpoint(cfg.Project.Name, envName, serviceName, remotePort)
		output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", endpoint, nil)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", serverName, err))
			continue
		}

		var response takod.ProxyTargetResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			failures = append(failures, fmt.Sprintf("%s: failed to parse proxy target: %v", serverName, err))
			continue
		}
		response, err = validateProxyTargetResponse(response, cfg.Project.Name, envName, serviceName, remotePort)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: invalid proxy target: %v", serverName, err))
			continue
		}
		return &proxyTarget{
			serverName: serverName,
			server:     server,
			client:     client,
			response:   response,
		}, nil
	}

	if len(failures) == 0 {
		return nil, fmt.Errorf("no environment nodes available for proxy target")
	}
	return nil, fmt.Errorf("no proxy target found for %s:%d: %s", serviceName, remotePort, strings.Join(failures, "; "))
}

func validateProxyTargetResponse(
	response takod.ProxyTargetResponse,
	project string,
	environment string,
	service string,
	remotePort int,
) (takod.ProxyTargetResponse, error) {
	if response.Project != project {
		return takod.ProxyTargetResponse{}, fmt.Errorf("project mismatch")
	}
	if response.Environment != environment {
		return takod.ProxyTargetResponse{}, fmt.Errorf("environment mismatch")
	}
	if response.Service != service {
		return takod.ProxyTargetResponse{}, fmt.Errorf("service mismatch")
	}
	if strings.TrimSpace(response.Container) == "" {
		return takod.ProxyTargetResponse{}, fmt.Errorf("container is required")
	}
	if strings.TrimSpace(response.ContainerID) == "" {
		return takod.ProxyTargetResponse{}, fmt.Errorf("container id is required")
	}
	if response.Port != remotePort {
		return takod.ProxyTargetResponse{}, fmt.Errorf("port mismatch")
	}
	ip := net.ParseIP(response.Host)
	if ip == nil {
		return takod.ProxyTargetResponse{}, fmt.Errorf("host must be an IP address")
	}
	if !ip.IsPrivate() {
		return takod.ProxyTargetResponse{}, fmt.Errorf("host must be a private container IP")
	}
	expectedAddress := net.JoinHostPort(response.Host, strconv.Itoa(response.Port))
	if response.Address != expectedAddress {
		return takod.ProxyTargetResponse{}, fmt.Errorf("address mismatch")
	}
	response.Address = expectedAddress
	return response, nil
}

func runLocalServiceProxy(ctx context.Context, target *proxyTarget, localPort int) error {
	if target == nil {
		return fmt.Errorf("proxy target is required")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)))
	if err != nil {
		return fmt.Errorf("listen on localhost: %w", err)
	}
	defer listener.Close()

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	localAddr := listener.Addr().String()
	fmt.Printf("Proxying %s -> %s/%s on %s (%s)\n",
		localAddr,
		target.response.Service,
		target.response.Container,
		target.serverName,
		target.response.Address,
	)
	fmt.Println("Press Ctrl-C to stop.")

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accept local connection: %w", err)
		}
		go proxyLocalConnection(target.client, target.response.Address, conn)
	}
}

func proxyLocalConnection(client interface {
	Dial(network string, address string) (net.Conn, error)
}, remoteAddress string, local net.Conn) {
	remote, err := client.Dial("tcp", remoteAddress)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to connect to proxy target %s: %v\n", remoteAddress, err)
		_ = local.Close()
		return
	}

	done := make(chan struct{}, 2)
	copyConn := func(dst net.Conn, src net.Conn) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go copyConn(remote, local)
	go copyConn(local, remote)
	<-done
	_ = local.Close()
	_ = remote.Close()
	<-done
}
