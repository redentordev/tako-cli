package cmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var (
	imageServer  string
	imageForce   bool
	volumeServer string
	volumeForce  bool
)

var imageCmd = &cobra.Command{
	Use:   "image",
	Short: "Manage app-owned Docker images on takod nodes",
}

var imageListCmd = &cobra.Command{
	Use:   "ls",
	Short: "List app-owned images on takod nodes",
	Args:  cobra.NoArgs,
	RunE:  runImageList,
}

var imageRemoveCmd = &cobra.Command{
	Use:   "rm IMAGE [IMAGE...]",
	Short: "Remove app-owned images from takod nodes",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runImageRemove,
}

var imagePruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Prune unused app-owned images from takod nodes",
	Args:  cobra.NoArgs,
	RunE:  runImagePrune,
}

var volumeCmd = &cobra.Command{
	Use:   "volume",
	Short: "Manage app-owned Docker volumes on takod nodes",
}

var volumeListCmd = &cobra.Command{
	Use:   "ls",
	Short: "List app-owned volumes on takod nodes",
	Args:  cobra.NoArgs,
	RunE:  runVolumeList,
}

var volumeRemoveCmd = &cobra.Command{
	Use:   "rm VOLUME [VOLUME...]",
	Short: "Remove app-owned volumes from takod nodes",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runVolumeRemove,
}

func init() {
	rootCmd.AddCommand(imageCmd)
	imageCmd.AddCommand(imageListCmd)
	imageCmd.AddCommand(imageRemoveCmd)
	imageCmd.AddCommand(imagePruneCmd)
	imageListCmd.Flags().StringVarP(&imageServer, "server", "s", "", "Node to query")
	imageRemoveCmd.Flags().StringVarP(&imageServer, "server", "s", "", "Node to mutate")
	imageRemoveCmd.Flags().BoolVar(&imageForce, "force", false, "Confirm image removal")
	imagePruneCmd.Flags().StringVarP(&imageServer, "server", "s", "", "Node to mutate")
	imagePruneCmd.Flags().BoolVar(&imageForce, "force", false, "Confirm image pruning")

	rootCmd.AddCommand(volumeCmd)
	volumeCmd.AddCommand(volumeListCmd)
	volumeCmd.AddCommand(volumeRemoveCmd)
	volumeListCmd.Flags().StringVarP(&volumeServer, "server", "s", "", "Node to query")
	volumeRemoveCmd.Flags().StringVarP(&volumeServer, "server", "s", "", "Node to mutate")
	volumeRemoveCmd.Flags().BoolVar(&volumeForce, "force", false, "Confirm volume removal")
}

func runImageList(cmd *cobra.Command, args []string) error {
	cfg, envName, pool, serverNames, err := resourceCommandContext(imageServer)
	if err != nil {
		return err
	}
	defer pool.CloseAll()

	var rows []resourceImageRow
	for _, serverName := range serverNames {
		server := cfg.Servers[serverName]
		client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", serverName, err)
		}
		output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", takodclient.ImagesEndpoint(cfg.Project.Name, envName), nil)
		if err != nil {
			return fmt.Errorf("failed to list images on %s: %w", serverName, err)
		}
		var response takod.ImageListResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return fmt.Errorf("failed to parse image list from %s: %w", serverName, err)
		}
		for _, image := range response.Images {
			rows = append(rows, resourceImageRow{Node: serverName, Image: image})
		}
	}
	printImageRows(rows)
	return nil
}

func runImageRemove(cmd *cobra.Command, args []string) error {
	if !imageForce {
		return fmt.Errorf("--force is required to remove images")
	}
	cfg, envName, pool, serverNames, err := resourceCommandContext(imageServer)
	if err != nil {
		return err
	}
	defer pool.CloseAll()

	for _, serverName := range serverNames {
		server := cfg.Servers[serverName]
		client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", serverName, err)
		}
		output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "DELETE", "/v1/images", takod.ImageRemoveRequest{
			Project:     cfg.Project.Name,
			Environment: envName,
			References:  args,
			Force:       true,
		})
		if err != nil {
			return fmt.Errorf("failed to remove images on %s: %w", serverName, err)
		}
		var response takod.ImageRemoveResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return fmt.Errorf("failed to parse image removal from %s: %w", serverName, err)
		}
		fmt.Printf("%s: removed %d image(s)\n", serverName, len(response.Removed))
	}
	return nil
}

func runImagePrune(cmd *cobra.Command, args []string) error {
	if !imageForce {
		return fmt.Errorf("--force is required to prune images")
	}
	cfg, envName, pool, serverNames, err := resourceCommandContext(imageServer)
	if err != nil {
		return err
	}
	defer pool.CloseAll()

	for _, serverName := range serverNames {
		server := cfg.Servers[serverName]
		client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", serverName, err)
		}
		output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "POST", "/v1/images/prune", takod.ImagePruneRequest{
			Project:     cfg.Project.Name,
			Environment: envName,
			Force:       true,
		})
		if err != nil {
			return fmt.Errorf("failed to prune images on %s: %w", serverName, err)
		}
		var response takod.ImagePruneResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return fmt.Errorf("failed to parse image prune response from %s: %w", serverName, err)
		}
		fmt.Println(imagePruneSummary(serverName, response))
	}
	return nil
}

func runVolumeList(cmd *cobra.Command, args []string) error {
	cfg, envName, pool, serverNames, err := resourceCommandContext(volumeServer)
	if err != nil {
		return err
	}
	defer pool.CloseAll()

	var rows []resourceVolumeRow
	for _, serverName := range serverNames {
		server := cfg.Servers[serverName]
		client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", serverName, err)
		}
		output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "GET", takodclient.VolumesEndpoint(cfg.Project.Name, envName), nil)
		if err != nil {
			return fmt.Errorf("failed to list volumes on %s: %w", serverName, err)
		}
		var response takod.VolumeListResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return fmt.Errorf("failed to parse volume list from %s: %w", serverName, err)
		}
		for _, volume := range response.Volumes {
			rows = append(rows, resourceVolumeRow{Node: serverName, Volume: volume})
		}
	}
	printVolumeRows(rows)
	return nil
}

func runVolumeRemove(cmd *cobra.Command, args []string) error {
	if !volumeForce {
		return fmt.Errorf("--force is required to remove volumes")
	}
	cfg, envName, pool, serverNames, err := resourceCommandContext(volumeServer)
	if err != nil {
		return err
	}
	defer pool.CloseAll()

	for _, serverName := range serverNames {
		server := cfg.Servers[serverName]
		client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", serverName, err)
		}
		output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "DELETE", "/v1/volumes", takod.VolumeRemoveRequest{
			Project:     cfg.Project.Name,
			Environment: envName,
			Names:       args,
			Force:       true,
		})
		if err != nil {
			return fmt.Errorf("failed to remove volumes on %s: %w", serverName, err)
		}
		var response takod.VolumeRemoveResponse
		if err := json.Unmarshal([]byte(output), &response); err != nil {
			return fmt.Errorf("failed to parse volume removal from %s: %w", serverName, err)
		}
		summary := fmt.Sprintf("%s: removed %d volume(s)", serverName, len(response.Removed))
		if len(response.Skipped) > 0 {
			summary += fmt.Sprintf(", skipped in-use: %s", strings.Join(response.Skipped, ", "))
		}
		fmt.Println(summary)
	}
	return nil
}

func imagePruneSummary(serverName string, response takod.ImagePruneResponse) string {
	summary := fmt.Sprintf("%s: pruned %d image(s)", serverName, len(response.Removed))
	if len(response.Skipped) > 0 {
		summary += fmt.Sprintf(", skipped in-use: %s", strings.Join(response.Skipped, ", "))
	}
	return summary
}

func resourceCommandContext(serverFlag string) (*config.Config, string, *ssh.Pool, []string, error) {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return nil, "", nil, nil, fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return nil, "", nil, nil, err
	}
	envName := getEnvironmentName(cfg)
	serverNames, err := statePullServerNames(cfg, envName, serverFlag)
	if err != nil {
		return nil, "", nil, nil, err
	}
	return cfg, envName, ssh.NewPool(), serverNames, nil
}

type resourceImageRow struct {
	Node  string
	Image takod.ImageSummary
}

type resourceVolumeRow struct {
	Node   string
	Volume takod.VolumeSummary
}

func printImageRows(rows []resourceImageRow) {
	if len(rows) == 0 {
		fmt.Println("No app-owned images found")
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Node == rows[j].Node {
			return rows[i].Image.Reference < rows[j].Image.Reference
		}
		return rows[i].Node < rows[j].Node
	})
	fmt.Printf("%-14s %-44s %-14s %-10s %s\n", "NODE", "IMAGE", "ID", "SIZE", "CREATED")
	fmt.Println(strings.Repeat("-", 96))
	for _, row := range rows {
		fmt.Printf("%-14s %-44s %-14s %-10s %s\n", row.Node, truncateResource(row.Image.Reference, 44), row.Image.ID, row.Image.Size, row.Image.CreatedSince)
	}
}

func printVolumeRows(rows []resourceVolumeRow) {
	if len(rows) == 0 {
		fmt.Println("No app-owned volumes found")
		return
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Node == rows[j].Node {
			return rows[i].Volume.Name < rows[j].Volume.Name
		}
		return rows[i].Node < rows[j].Node
	})
	fmt.Printf("%-14s %-44s %-12s %s\n", "NODE", "VOLUME", "DRIVER", "SCOPE")
	fmt.Println(strings.Repeat("-", 82))
	for _, row := range rows {
		fmt.Printf("%-14s %-44s %-12s %s\n", row.Node, truncateResource(row.Volume.Name, 44), row.Volume.Driver, row.Volume.Scope)
	}
}

func truncateResource(value string, width int) string {
	if len(value) <= width {
		return value
	}
	if width <= 3 {
		return value[:width]
	}
	return value[:width-3] + "..."
}
