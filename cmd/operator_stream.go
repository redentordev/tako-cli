package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	takossh "github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/redentordev/tako-cli/pkg/utils"
	"golang.org/x/term"
)

type streamTarget struct {
	serverName string
	server     config.ServerConfig
	client     *takossh.Client
}

func encodeTakodStreamRequest(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func streamTakodCommand(target streamTarget, cfg *config.Config, endpoint string, request any, tty bool, stdin bool) error {
	encoded, err := encodeTakodStreamRequest(request)
	if err != nil {
		return err
	}
	remoteCommand := buildRemoteTakodStreamCommand(takodSocketFromConfig(cfg), endpoint, stdin || tty)
	err = target.client.ExecuteDuplex(remoteCommand, streamTakodInput(encoded, os.Stdin, stdin || tty), os.Stdout, os.Stderr, tty)
	if err == nil {
		return nil
	}
	if code, ok := takossh.ExitCode(err); ok {
		return newExitCodeError(code, nil)
	}
	return err
}

func streamTakodInput(encodedRequest string, commandInput io.Reader, forwardCommandInput bool) io.Reader {
	requestPrefix := strings.NewReader(encodedRequest + "\n")
	if forwardCommandInput {
		return io.MultiReader(requestPrefix, commandInput)
	}
	return requestPrefix
}

func buildRemoteTakodStreamCommand(socket string, endpoint string, stdin bool) string {
	args := []string{
		"internal",
		"stream-takod",
		"--socket", socket,
		"--endpoint", endpoint,
		"--request-stdin",
	}
	if stdin {
		args = append(args, "--stdin")
	}
	quotedArgs := make([]string, 0, len(args))
	for _, arg := range args {
		quotedArgs = append(quotedArgs, utils.ShellQuote(arg))
	}
	return strings.Join([]string{
		"tako_bin=$(command -v tako 2>/dev/null || true)",
		"if test -z \"$tako_bin\" && test -x /usr/local/bin/tako; then tako_bin=/usr/local/bin/tako; fi",
		"if test -z \"$tako_bin\"; then echo 'server-side tako binary is unavailable' >&2; exit 42; fi",
		"exec \"$tako_bin\" " + strings.Join(quotedArgs, " "),
	}, "; ")
}

func commandWantsTTY(command []string, explicitTTY bool) bool {
	if explicitTTY {
		return true
	}
	return len(command) == 0 && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

func commandWantsStdin(command []string, tty bool, explicitStdin bool) bool {
	if explicitStdin || tty {
		return true
	}
	return len(command) == 0 && !term.IsTerminal(int(os.Stdin.Fd()))
}

func selectExecTarget(pool *takossh.Pool, cfg *config.Config, envName string, serviceName string, slot int, requestedServer string) (streamTarget, error) {
	serverNames, err := statePullServerNames(cfg, envName, requestedServer)
	if err != nil {
		return streamTarget{}, err
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

		endpoint := takodclient.ExecTargetEndpoint(cfg.Project.Name, envName, serviceName, slot)
		output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", endpoint, nil)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", serverName, err))
			continue
		}
		var response takod.ExecTargetResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			failures = append(failures, fmt.Sprintf("%s: failed to parse exec target: %v", serverName, err))
			continue
		}
		if err := validateExecTargetResponse(response, cfg.Project.Name, envName, serviceName, slot); err != nil {
			failures = append(failures, fmt.Sprintf("%s: invalid exec target: %v", serverName, err))
			continue
		}
		return streamTarget{serverName: serverName, server: server, client: client}, nil
	}

	if len(failures) == 0 {
		return streamTarget{}, fmt.Errorf("no environment nodes available for exec target")
	}
	return streamTarget{}, fmt.Errorf("no exec target found for %s: %s", serviceName, strings.Join(failures, "; "))
}

func validateExecTargetResponse(response takod.ExecTargetResponse, project string, envName string, serviceName string, slot int) error {
	if response.Project != project {
		return fmt.Errorf("project mismatch")
	}
	if response.Environment != envName {
		return fmt.Errorf("environment mismatch")
	}
	if response.Service != serviceName {
		return fmt.Errorf("service mismatch")
	}
	if response.Slot != slot {
		return fmt.Errorf("slot mismatch")
	}
	if strings.TrimSpace(response.Container) == "" {
		return fmt.Errorf("container is required")
	}
	if strings.TrimSpace(response.ContainerID) == "" {
		return fmt.Errorf("container id is required")
	}
	return nil
}

func selectRunTarget(pool *takossh.Pool, cfg *config.Config, envName string, requestedServer string) (streamTarget, []string, error) {
	serverNames, err := statePullServerNames(cfg, envName, requestedServer)
	if err != nil {
		return streamTarget{}, nil, err
	}
	if len(serverNames) == 0 {
		return streamTarget{}, nil, fmt.Errorf("no environment nodes available for run target")
	}

	for _, serverName := range serverNames {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return streamTarget{}, nil, fmt.Errorf("server %s not found in configuration", serverName)
		}
		client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return streamTarget{}, nil, fmt.Errorf("failed to connect to node %s: %w", serverName, err)
		}
		return streamTarget{serverName: serverName, server: server, client: client}, serverNames, nil
	}

	return streamTarget{}, nil, fmt.Errorf("no environment nodes available for run target")
}

func runOneOffNetworkName(project string, environment string) string {
	return runtimeid.NetworkName(project, environment)
}
