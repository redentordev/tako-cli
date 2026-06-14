package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	internalStreamSocket   string
	internalStreamEndpoint string
	internalStreamRequest  string
	internalStreamReqStdin bool
	internalStreamStdin    bool
)

var internalStreamTakodCmd = &cobra.Command{
	Use:    "stream-takod",
	Short:  "Bridge local stdio to a takod streaming endpoint",
	Hidden: true,
	Run:    runInternalStreamTakod,
}

func init() {
	internalCmd.AddCommand(internalStreamTakodCmd)
	internalStreamTakodCmd.Flags().StringVar(&internalStreamSocket, "socket", takodclient.DefaultSocket, "takod Unix socket")
	internalStreamTakodCmd.Flags().StringVar(&internalStreamEndpoint, "endpoint", "", "takod endpoint")
	internalStreamTakodCmd.Flags().StringVar(&internalStreamRequest, "request", "", "base64 encoded stream request metadata")
	internalStreamTakodCmd.Flags().BoolVar(&internalStreamReqStdin, "request-stdin", false, "Read base64 encoded stream request metadata from stdin")
	internalStreamTakodCmd.Flags().BoolVar(&internalStreamStdin, "stdin", false, "Forward stdin to takod")
}

func runInternalStreamTakod(cmd *cobra.Command, args []string) {
	request, input, err := internalStreamRequestAndInput(os.Stdin, internalStreamRequest, internalStreamReqStdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var stdin io.Reader
	if internalStreamStdin {
		if term.IsTerminal(int(os.Stdin.Fd())) {
			stdin = input
		} else {
			data, err := io.ReadAll(input)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			stdin = bytes.NewReader(data)
		}
	}
	code, err := streamTakodEndpoint(cmd.Context(), internalStreamSocket, internalStreamEndpoint, request, stdin, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(code)
}

func internalStreamRequestAndInput(input io.Reader, request string, requestFromStdin bool) (string, io.Reader, error) {
	if !requestFromStdin {
		return request, input, nil
	}
	reader := bufio.NewReader(input)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", nil, fmt.Errorf("failed to read stream request metadata: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), reader, nil
}

func streamTakodEndpoint(ctx context.Context, socket string, endpoint string, request string, stdin io.Reader, stdout io.Writer) (int, error) {
	if strings.TrimSpace(socket) == "" {
		socket = takodclient.DefaultSocket
	}
	if !strings.HasPrefix(endpoint, "/") {
		return 1, fmt.Errorf("takod endpoint must start with /")
	}
	if strings.TrimSpace(request) == "" {
		return 1, fmt.Errorf("--request is required")
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socket)
		},
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://takod"+endpoint, stdin)
	if err != nil {
		return 1, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Tako-Request", request)

	resp, err := client.Do(req)
	if err != nil {
		return 1, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return 1, fmt.Errorf("takod stream request returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if _, err := io.Copy(stdout, resp.Body); err != nil {
		return 1, err
	}

	rawExitCode := strings.TrimSpace(resp.Trailer.Get("X-Tako-Exit-Code"))
	if rawExitCode == "" {
		return 1, fmt.Errorf("takod stream response missing exit code")
	}
	exitCode, err := strconv.Atoi(rawExitCode)
	if err != nil {
		return 1, fmt.Errorf("invalid takod exit code %q", rawExitCode)
	}
	if exitCode < 0 || exitCode > 255 {
		return 1, fmt.Errorf("takod exit code out of range: %d", exitCode)
	}
	return exitCode, nil
}
