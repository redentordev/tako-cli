package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var (
	certsServer   string
	certsCertFile string
	certsKeyFile  string
)

var certsCmd = &cobra.Command{
	Use:          "certs",
	Short:        "Manage validated TLS certificates on proxy nodes",
	SilenceUsage: true,
}

var certsPushCmd = &cobra.Command{
	Use:          "push DOMAIN",
	Short:        "Validate and push a certificate/key pair to proxy nodes",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	RunE:         runCertsPush,
}

var certsListCmd = &cobra.Command{
	Use:          "ls",
	Aliases:      []string{"list"},
	Short:        "List node-local proxy certificates",
	SilenceUsage: true,
	Args:         cobra.NoArgs,
	RunE:         runCertsList,
}

var certsRemoveCmd = &cobra.Command{
	Use:          "rm DOMAIN",
	Aliases:      []string{"remove", "delete"},
	Short:        "Remove a certificate and return matching routes to automatic HTTPS",
	SilenceUsage: true,
	Args:         cobra.ExactArgs(1),
	RunE:         runCertsRemove,
}

func init() {
	rootCmd.AddCommand(certsCmd)
	certsCmd.AddCommand(certsPushCmd, certsListCmd, certsRemoveCmd)
	for _, command := range []*cobra.Command{certsPushCmd, certsListCmd, certsRemoveCmd} {
		command.Flags().StringVarP(&certsServer, "server", "s", "", "Target one environment proxy node (default: all proxy nodes)")
	}
	certsPushCmd.Flags().StringVar(&certsCertFile, "cert", "", "Certificate PEM file, including intermediates")
	certsPushCmd.Flags().StringVar(&certsKeyFile, "key", "", "Private key PEM file")
	_ = certsPushCmd.MarkFlagRequired("cert")
	_ = certsPushCmd.MarkFlagRequired("key")
}

func runCertsPush(cmd *cobra.Command, args []string) error {
	certPEM, err := os.ReadFile(certsCertFile)
	if err != nil {
		return &engine.InvalidRequestError{Err: fmt.Errorf("failed to read certificate file: %w", err)}
	}
	keyPEM, err := os.ReadFile(certsKeyFile)
	if err != nil {
		return &engine.InvalidRequestError{Err: fmt.Errorf("failed to read private key file: %w", err)}
	}
	cliEngine().RegisterSecret(string(keyPEM))
	return runCertsCommand(cmd, "push", args[0], &takod.ProxyCertificatePushRequest{Domain: args[0], CertPEM: string(certPEM), KeyPEM: string(keyPEM)})
}

func runCertsList(cmd *cobra.Command, _ []string) error {
	return runCertsCommand(cmd, "list", "", nil)
}

func runCertsRemove(cmd *cobra.Command, args []string) error {
	return runCertsCommand(cmd, "remove", args[0], nil)
}

func runCertsCommand(cmd *cobra.Command, action string, domain string, push *takod.ProxyCertificatePushRequest) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}
	envName := getEnvironmentName(cfg)
	targets, err := certificateTargetServers(cfg, envName, certsServer)
	if err != nil {
		return err
	}
	startedAt := time.Now()
	result, operationErr := executeCertificateOperation(cmd.Context(), cfg, envName, targets, action, domain, push)
	result.StartedAt = startedAt
	result.Duration = time.Since(startedAt).Seconds()
	if operationErr != nil {
		operationErr = redactCertificateError(operationErr)
		result.Error = operationErr.Error()
	}
	redactCertsResultErrors(&result)
	emitCertificateOperationEvents(result)
	if !machineOutputEnabled() {
		printCertsResult(cmd, result)
	}
	if emitErr := emitResultDocument(result); emitErr != nil && operationErr == nil {
		operationErr = emitErr
	}
	return operationErr
}

func certificateTargetServers(cfg *config.Config, envName string, requested string) ([]string, error) {
	servers, err := cfg.GetEnvironmentProxyServers(envName)
	if err != nil {
		return nil, err
	}
	sort.Strings(servers)
	if requested == "" {
		if len(servers) == 0 {
			return nil, &engine.InvalidRequestError{Err: fmt.Errorf("environment %s has no proxy nodes", envName)}
		}
		return servers, nil
	}
	for _, server := range servers {
		if server == requested {
			return []string{requested}, nil
		}
	}
	return nil, &engine.InvalidRequestError{Err: fmt.Errorf("server %s is not a proxy target for environment %s", requested, envName)}
}

func executeCertificateOperation(ctx context.Context, cfg *config.Config, envName string, targets []string, action string, domain string, push *takod.ProxyCertificatePushRequest) (engine.CertsResult, error) {
	result := engine.CertsResult{
		APIVersion: takoapi.APIVersionCurrent, Kind: engine.KindCertsResult, Project: cfg.Project.Name, Environment: envName, Action: action, Domain: domain, Nodes: []engine.CertsNodeResult{},
	}
	pool := ssh.NewPool()
	defer pool.CloseAll()
	factory, err := nodeclient.NewFactory(cfg, pool, takodSocketFromConfig(cfg))
	if err != nil {
		return result, err
	}
	defer factory.CloseIdleConnections()
	clients := make(map[string]*takodclient.AgentClient, len(targets))
	var connectionErr error
	for _, serverName := range targets {
		server, ok := cfg.Servers[serverName]
		node := engine.CertsNodeResult{Server: serverName, Certificates: []takod.ProxyCertificateMetadata{}}
		if ok {
			node.Host = server.Host
		}
		if !ok {
			node.Error = "server is not defined"
			if connectionErr == nil {
				connectionErr = &engine.InvalidRequestError{Err: fmt.Errorf("server %s is not defined", serverName)}
			}
			result.Nodes = append(result.Nodes, node)
			continue
		}
		client, _, err := factory.Client(ctx, serverName)
		if err != nil {
			node.Error = "connect: " + err.Error()
			if connectionErr == nil {
				connectionErr = &engine.ConnectivityError{Server: serverName, Err: fmt.Errorf("failed to connect to %s: %w", serverName, err)}
			}
			result.Nodes = append(result.Nodes, node)
			continue
		}
		clients[serverName] = client
		result.Nodes = append(result.Nodes, node)
	}
	if connectionErr != nil {
		return result, connectionErr
	}
	var operationErr error
	result.Nodes, operationErr = executeCertificateNodeRequests(ctx, takodSocketFromConfig(cfg), result.Nodes, clients, action, domain, push)
	return result, operationErr
}

func executeCertificateNodeRequests[T any](ctx context.Context, socket string, nodes []engine.CertsNodeResult, clients map[string]T, action string, domain string, push *takod.ProxyCertificatePushRequest) ([]engine.CertsNodeResult, error) {
	var preflightErr error
	for i := range nodes {
		node := &nodes[i]
		if node.Error != "" {
			if preflightErr == nil {
				preflightErr = fmt.Errorf("certificate operation preflight failed on %s: %s", node.Server, node.Error)
			}
			continue
		}
		client, found := clients[node.Server]
		if !found || any(client) == nil {
			node.Error = "connection is unavailable"
			if preflightErr == nil {
				preflightErr = fmt.Errorf("connection to %s is unavailable", node.Server)
			}
			continue
		}
		if err := takodclient.RequireCapability(ctx, client, socket, node.Server, takod.CapabilityProxyCertsV1, "proxy certificate management"); err != nil {
			node.Error = err.Error()
			if preflightErr == nil {
				preflightErr = err
			}
		}
	}
	if preflightErr != nil {
		return nodes, preflightErr
	}

	var firstInvalid error
	for i := range nodes {
		node := &nodes[i]
		var output string
		var err error
		switch action {
		case "push":
			output, err = takodclient.RequestJSONWithContext(ctx, clients[node.Server], socket, "PUT", takodclient.CertificatesEndpoint(""), push)
		case "remove":
			output, err = takodclient.RequestJSONWithContext(ctx, clients[node.Server], socket, "DELETE", takodclient.CertificatesEndpoint(domain), nil)
		case "list":
			output, err = takodclient.RequestJSONWithContext(ctx, clients[node.Server], socket, "GET", takodclient.CertificatesEndpoint(""), nil)
		default:
			return nodes, &engine.InvalidRequestError{Err: fmt.Errorf("unsupported certificate action %q", action)}
		}
		if err != nil {
			node.Error = err.Error()
			var httpErr *takodclient.HTTPError
			if errors.As(err, &httpErr) && httpErr.Status == 400 && firstInvalid == nil {
				firstInvalid = err
			}
			continue
		}
		if action == "list" {
			var response takod.ProxyCertificateListResponse
			if err := json.Unmarshal([]byte(output), &response); err != nil {
				node.Error = "invalid response: " + err.Error()
				continue
			}
			node.Certificates = response.Certificates
		} else {
			var response takod.ProxyCertificateMutationResponse
			if err := json.Unmarshal([]byte(output), &response); err != nil {
				node.Error = "invalid response: " + err.Error()
				continue
			}
			node.Certificates = []takod.ProxyCertificateMetadata{response.Certificate}
		}
	}
	return nodes, certificateOperationError(nodes, firstInvalid)
}

func emitCertificateOperationEvents(result engine.CertsResult) {
	for _, node := range result.Nodes {
		if node.Error != "" {
			continue
		}
		cliEngine().EventStream().Emit(events.Event{
			Type: events.TypeCertificate, Phase: events.PhaseDomains, Level: events.LevelInfo, Node: node.Server,
			Message: fmt.Sprintf("certificate %s completed on %s\n", result.Action, node.Server),
			Data:    map[string]any{"action": result.Action, "domain": result.Domain},
		})
	}
}

func certificateOperationError(nodes []engine.CertsNodeResult, invalidErr error) error {
	failures := 0
	for _, node := range nodes {
		if node.Error != "" {
			failures++
		}
	}
	if failures == 0 {
		return nil
	}
	if failures == len(nodes) {
		if invalidErr != nil {
			return &engine.InvalidRequestError{Err: invalidErr}
		}
		return fmt.Errorf("certificate operation failed on all %d node(s)", failures)
	}
	return &engine.AttentionError{Err: fmt.Errorf("certificate operation failed on %d of %d node(s)", failures, len(nodes))}
}

func redactCertsResultErrors(result *engine.CertsResult) {
	if result == nil {
		return
	}
	redact := cliEngine().RedactSensitive
	result.Error = redact(result.Error)
	for i := range result.Nodes {
		result.Nodes[i].Error = redact(result.Nodes[i].Error)
	}
}

func redactCertificateError(err error) error {
	if err == nil {
		return nil
	}
	message := errors.New(cliEngine().RedactSensitive(err.Error()))
	var invalid *engine.InvalidRequestError
	if errors.As(err, &invalid) {
		return &engine.InvalidRequestError{Err: message}
	}
	var connectivity *engine.ConnectivityError
	if errors.As(err, &connectivity) {
		return &engine.ConnectivityError{Server: connectivity.Server, Err: message}
	}
	var attention *engine.AttentionError
	if errors.As(err, &attention) {
		return &engine.AttentionError{Err: message}
	}
	var capability *takodclient.CapabilityRequiredError
	if errors.As(err, &capability) {
		return capability
	}
	return message
}

func printCertsResult(cmd *cobra.Command, result engine.CertsResult) {
	for _, node := range result.Nodes {
		if node.Error != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "%s: ERROR %s\n", node.Server, node.Error)
			continue
		}
		if len(node.Certificates) == 0 {
			fmt.Fprintf(cmd.OutOrStdout(), "%s: no certificates\n", node.Server)
			continue
		}
		for _, certificate := range node.Certificates {
			expires := "pending"
			if !certificate.NotAfter.IsZero() {
				expires = certificate.NotAfter.UTC().Format(time.RFC3339)
			}
			state := ""
			if certificate.OwnerProject != "" {
				state += " owner=" + certificate.OwnerProject + "/" + certificate.OwnerEnvironment
			}
			if certificate.DNSProvider != "" {
				state += " dnsProvider=" + certificate.DNSProvider
			}
			if certificate.CAProvider != "" {
				state += " caProvider=" + certificate.CAProvider
			}
			if certificate.Staging {
				state += " staging=true"
			}
			if certificate.Orphaned {
				state = " orphaned=true"
			}
			if certificate.LastError != "" {
				state += " lastError=" + certificate.LastError
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s source=%s expires=%s%s\n", node.Server, certificate.Domain, certificate.Source, expires, state)
		}
	}
}
