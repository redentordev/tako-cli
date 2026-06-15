package cmd

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var meshRTTCount int
var meshRTTServer string

var meshCmd = &cobra.Command{
	Use:   "mesh",
	Short: "Inspect the takod mesh",
}

var meshRTTCmd = &cobra.Command{
	Use:   "rtt",
	Short: "Measure mesh RTT between nodes",
	RunE:  runMeshRTT,
}

type meshRTTRow struct {
	Source      string
	Target      string
	TargetIP    string
	Reachable   bool
	Sent        int
	Received    int
	LossPercent float64
	AvgMS       float64
	Err         error
}

func init() {
	rootCmd.AddCommand(meshCmd)
	meshCmd.AddCommand(meshRTTCmd)
	meshRTTCmd.Flags().IntVar(&meshRTTCount, "count", 3, "ICMP packets per peer")
	meshRTTCmd.Flags().StringVarP(&meshRTTServer, "server", "s", "", "Source node to measure from")
}

func runMeshRTT(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}
	if !cfg.IsMeshEnabled() {
		return fmt.Errorf("mesh is disabled")
	}
	if meshRTTCount < 1 || meshRTTCount > 10 {
		return fmt.Errorf("count must be between 1 and 10")
	}

	envName := getEnvironmentName(cfg)
	envServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return fmt.Errorf("failed to get environment servers: %w", err)
	}
	sourceServers, err := statePullServerNames(cfg, envName, meshRTTServer)
	if err != nil {
		return err
	}
	if len(envServers) <= 1 {
		fmt.Println("Mesh has one node; no peer RTT to measure.")
		return nil
	}

	addresses, err := meshNodeIPs(cfg, envServers)
	if err != nil {
		return err
	}
	pool := ssh.NewPool()
	defer pool.CloseAll()

	rows := gatherMeshRTTRows(pool, cfg, sourceServers, envServers, addresses, meshRTTCount)
	displayMeshRTTRows(rows)
	return meshRTTErr(rows)
}

func gatherMeshRTTRows(pool *ssh.Pool, cfg *config.Config, sourceServers []string, envServers []string, addresses map[string]string, count int) []meshRTTRow {
	rows := make([]meshRTTRow, 0, len(sourceServers)*(len(envServers)-1))
	for _, sourceName := range sourceServers {
		source, ok := cfg.Servers[sourceName]
		if !ok {
			rows = append(rows, meshRTTRow{Source: sourceName, Err: fmt.Errorf("source server not found")})
			continue
		}
		client, err := pool.GetOrCreateWithAuth(source.Host, source.Port, source.User, source.SSHKey, source.Password)
		if err != nil {
			rows = append(rows, meshRTTRow{Source: sourceName, Err: fmt.Errorf("connect failed: %w", err)})
			continue
		}
		for _, targetName := range envServers {
			if targetName == sourceName {
				continue
			}
			targetIP := addresses[targetName]
			row := meshRTTRow{Source: sourceName, Target: targetName, TargetIP: targetIP}
			output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", takodclient.MeshRTTEndpoint(targetIP, count), nil)
			if err != nil {
				row.Err = err
				rows = append(rows, row)
				continue
			}
			var response takod.MeshRTTResponse
			if err := json.Unmarshal([]byte(output), &response); err != nil {
				row.Err = fmt.Errorf("failed to parse RTT response: %w", err)
				rows = append(rows, row)
				continue
			}
			if response.Target != targetIP {
				row.Err = fmt.Errorf("target mismatch")
				rows = append(rows, row)
				continue
			}
			row.Reachable = response.Reachable
			row.Sent = response.Sent
			row.Received = response.Received
			row.LossPercent = response.LossPercent
			row.AvgMS = response.AvgMS
			rows = append(rows, row)
		}
	}
	return rows
}

func meshNodeIPs(cfg *config.Config, envServers []string) (map[string]string, error) {
	if cfg.Mesh == nil {
		return nil, fmt.Errorf("mesh config is required")
	}
	_, ipNet, err := net.ParseCIDR(cfg.Mesh.NetworkCIDR)
	if err != nil {
		return nil, fmt.Errorf("invalid mesh CIDR: %w", err)
	}
	baseIP := ipNet.IP.To4()
	if baseIP == nil {
		return nil, fmt.Errorf("mesh.networkCIDR must be IPv4")
	}
	base := binary.BigEndian.Uint32(baseIP)
	out := make(map[string]string, len(envServers))
	for index, serverName := range envServers {
		nodeIP := make(net.IP, 4)
		binary.BigEndian.PutUint32(nodeIP, base+uint32(index+1))
		out[serverName] = nodeIP.String()
	}
	return out, nil
}

func displayMeshRTTRows(rows []meshRTTRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Source == rows[j].Source {
			return rows[i].Target < rows[j].Target
		}
		return rows[i].Source < rows[j].Source
	})
	fmt.Println()
	fmt.Printf("%-14s %-14s %-15s %-10s %-9s %-10s\n", "SOURCE", "TARGET", "MESH_IP", "AVG_MS", "LOSS", "STATUS")
	fmt.Println(strings.Repeat("-", 78))
	for _, row := range rows {
		status := "ok"
		avg := "-"
		loss := "-"
		if row.Err != nil {
			status = "error"
		} else {
			loss = fmt.Sprintf("%.1f%%", row.LossPercent)
			if row.Reachable {
				avg = strconv.FormatFloat(row.AvgMS, 'f', 2, 64)
			} else {
				status = "lost"
			}
		}
		fmt.Printf("%-14s %-14s %-15s %-10s %-9s %-10s\n", row.Source, row.Target, row.TargetIP, avg, loss, status)
		if row.Err != nil {
			fmt.Printf("  %s -> %s: %v\n", row.Source, row.Target, row.Err)
		}
	}
	fmt.Println()
}

func meshRTTErr(rows []meshRTTRow) error {
	var failures []string
	for _, row := range rows {
		if row.Err != nil {
			failures = append(failures, fmt.Sprintf("%s->%s: %v", row.Source, row.Target, row.Err))
			continue
		}
		if !row.Reachable {
			failures = append(failures, fmt.Sprintf("%s->%s: %.1f%% packet loss", row.Source, row.Target, row.LossPercent))
		}
	}
	if len(failures) == 0 {
		return nil
	}
	sort.Strings(failures)
	return fmt.Errorf("mesh RTT check failed: %s", strings.Join(failures, "; "))
}
