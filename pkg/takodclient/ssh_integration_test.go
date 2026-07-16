//go:build linux || darwin

package takodclient_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	cryptossh "golang.org/x/crypto/ssh"
)

type sshStreamlocalDialer struct {
	client *cryptossh.Client
}

func (d sshStreamlocalDialer) DialUnixSocket(ctx context.Context, path string) (net.Conn, error) {
	return d.client.DialContext(ctx, "unix", path)
}

type directStreamlocalPayload struct {
	SocketPath string
	Reserved   string
	Port       uint32
}

func TestAgentClientOverRealSSHStreamlocal(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "tako-ssh-agent-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "takod.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	installation, err := nodeidentity.New(
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"node-1",
		[]string{nodeidentity.RoleWorker},
		time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	statusBody, err := json.Marshal(takodclient.AgentStatus{
		Runtime:      "takod",
		Capabilities: []string{nodeidentity.Capability},
		Identity:     &installation.Identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/status":
			_, _ = writer.Write(statusBody)
		case "/v1/echo":
			data, _ := io.ReadAll(request.Body)
			_, _ = writer.Write(data)
		case "/v1/stream":
			_, _ = io.WriteString(writer, "first\nsecond\n")
		case "/v1/block":
			<-request.Context().Done()
		default:
			http.NotFound(writer, request)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	sshClient := startStreamlocalSSHServer(t, socket)
	client, err := takodclient.NewAgentClient(sshStreamlocalDialer{client: sshClient}, socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("Status over SSH streamlocal failed: %v", err)
	}
	if status.Identity == nil || !status.Identity.Matches(installation.ClusterID, installation.NodeID) {
		t.Fatalf("unexpected SSH status identity: %#v", status.Identity)
	}
	output, err := client.RequestJSON(context.Background(), http.MethodPost, "/v1/echo", map[string]string{"name": "demo"})
	if err != nil || !strings.Contains(output, `"name": "demo"`) {
		t.Fatalf("JSON request over SSH = %q, %v", output, err)
	}
	var streamed bytes.Buffer
	if err := client.StreamOutput(context.Background(), http.MethodGet, "/v1/stream", nil, "", &streamed, io.Discard); err != nil {
		t.Fatalf("stream over SSH failed: %v", err)
	}
	if streamed.String() != "first\nsecond\n" {
		t.Fatalf("stream output = %q", streamed.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := client.StreamOutput(ctx, http.MethodGet, "/v1/block", nil, "", io.Discard, io.Discard); err == nil {
		t.Fatal("canceled SSH streamlocal request unexpectedly succeeded")
	}
}

func startStreamlocalSSHServer(t *testing.T, targetSocket string) *cryptossh.Client {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := cryptossh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := &cryptossh.ServerConfig{NoClientAuth: true}
	serverConfig.AddHostKey(signer)
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tcpListener.Close() })
	go func() {
		connection, acceptErr := tcpListener.Accept()
		if acceptErr != nil {
			return
		}
		serverConnection, channels, requests, handshakeErr := cryptossh.NewServerConn(connection, serverConfig)
		if handshakeErr != nil {
			_ = connection.Close()
			return
		}
		defer serverConnection.Close()
		go cryptossh.DiscardRequests(requests)
		for channelRequest := range channels {
			if channelRequest.ChannelType() != "direct-streamlocal@openssh.com" {
				_ = channelRequest.Reject(cryptossh.UnknownChannelType, "unsupported")
				continue
			}
			var payload directStreamlocalPayload
			if err := cryptossh.Unmarshal(channelRequest.ExtraData(), &payload); err != nil || payload.SocketPath != targetSocket {
				_ = channelRequest.Reject(cryptossh.Prohibited, "invalid socket")
				continue
			}
			target, err := net.Dial("unix", targetSocket)
			if err != nil {
				_ = channelRequest.Reject(cryptossh.ConnectionFailed, err.Error())
				continue
			}
			channel, channelRequests, err := channelRequest.Accept()
			if err != nil {
				_ = target.Close()
				continue
			}
			go cryptossh.DiscardRequests(channelRequests)
			go proxyStreamlocal(channel, target)
		}
	}()
	clientConfig := &cryptossh.ClientConfig{
		User:            "test",
		HostKeyCallback: cryptossh.InsecureIgnoreHostKey(), // ephemeral in-process test server
		Timeout:         5 * time.Second,
	}
	client, err := cryptossh.Dial("tcp", tcpListener.Addr().String(), clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func proxyStreamlocal(channel cryptossh.Channel, target net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(target, channel)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(channel, target)
		done <- struct{}{}
	}()
	<-done
	_ = channel.Close()
	_ = target.Close()
}
