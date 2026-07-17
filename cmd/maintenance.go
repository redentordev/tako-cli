package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var (
	maintenanceService string
)

const maintenanceImage = "nginx:1.27-alpine"

var maintenanceCmd = &cobra.Command{
	Use:   "maintenance",
	Short: "Enable maintenance mode for a service",
	Long: `Enable maintenance mode for a public-facing service.

This command deploys a maintenance page container with proxy routing
that takes priority over the main service. The main service continues
running in the background.

Custom Maintenance Page:
  Create a 'maintenance.html' file in your project directory to use a custom page.
  If not provided, a simple default page will be used.

To restore normal operation, use 'tako live'.

Examples:
  tako maintenance --service web               # Enable on all environment nodes

  # With custom page
  echo '<h1>Custom Maintenance</h1>' > maintenance.html
  tako maintenance --service web`,
	RunE: runMaintenance,
}

func init() {
	rootCmd.AddCommand(maintenanceCmd)
	maintenanceCmd.Flags().StringVar(&maintenanceService, "service", "", "Service to put in maintenance mode (required)")
	maintenanceCmd.MarkFlagRequired("service")
}

func runMaintenance(cmd *cobra.Command, args []string) error {
	// Machine modes reserve stdout for parseable output.
	var out io.Writer = os.Stdout
	if machineOutputEnabled() {
		out = os.Stderr
	}

	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	// Get environment
	envName := getEnvironmentName(cfg)
	services, err := cfg.GetServices(envName)
	if err != nil {
		return fmt.Errorf("failed to get services: %w", err)
	}

	// Check service exists
	service, exists := services[maintenanceService]
	if !exists {
		return fmt.Errorf("service %s not found in environment %s", maintenanceService, envName)
	}

	// Check if service has proxy configuration
	if !service.IsProxied() {
		return fmt.Errorf("service %s has no proxy configuration", maintenanceService)
	}

	targetServers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return err
	}
	if len(targetServers) == 0 {
		return fmt.Errorf("no servers configured for environment %s", envName)
	}
	targetServers, err = config.ResolveSchedulableMutationTargets(cfg.Servers, targetServers, envName, true)
	if err != nil {
		return err
	}
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()
	runtimeFactory, err := nodeclient.NewFactory(cfg, sshPool, takodSocketFromConfig(cfg))
	if err != nil {
		return err
	}
	defer runtimeFactory.CloseIdleConnections()
	leaseSet, err := acquireRemoteOperationLeases(sshPool, cfg, envName, targetServers, "maintenance")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)
	if verbose {
		fmt.Fprintf(out, "→ Acquired remote maintenance leases: %s\n", leaseSet.Summary())
	}

	fmt.Fprintf(out, "Enabling maintenance mode for %s on %d node(s)...\n\n", maintenanceService, len(targetServers))

	// Check if user has a custom maintenance.html in the project directory
	customMaintenancePath := "maintenance.html"
	var maintenanceHTML string

	if _, err := os.Stat(customMaintenancePath); err == nil {
		// User provided custom maintenance page
		content, err := os.ReadFile(customMaintenancePath)
		if err != nil {
			return fmt.Errorf("failed to read custom maintenance.html: %w", err)
		}
		maintenanceHTML = string(content)
		if verbose {
			fmt.Fprintf(out, "Using custom maintenance page from ./maintenance.html\n")
		}
	} else {
		// Use default maintenance page with Tako branding and space shooter game
		maintenanceHTML = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>🐙 Tako - Under Maintenance</title>
    <style>
        * { margin: 0; padding: 0; box-sizing: border-box; }
        body {
            font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif;
            background: linear-gradient(135deg, #1a1a2e 0%, #16213e 100%);
            color: #fff;
            overflow: hidden;
            height: 100vh;
            display: flex;
            flex-direction: column;
            align-items: center;
            justify-content: center;
        }
        .container {
            text-align: center;
            z-index: 10;
            position: relative;
        }
        .logo { font-size: 72px; margin-bottom: 10px; animation: float 3s ease-in-out infinite; }
        @keyframes float { 0%, 100% { transform: translateY(0px); } 50% { transform: translateY(-10px); } }
        h1 { 
            font-size: 2.5em; 
            margin-bottom: 10px;
            background: linear-gradient(45deg, #667eea, #764ba2);
            -webkit-background-clip: text;
            -webkit-text-fill-color: transparent;
            background-clip: text;
        }
        .subtitle { font-size: 1.1em; opacity: 0.8; margin-bottom: 30px; }
        .game-container {
            margin: 30px auto;
            position: relative;
        }
        canvas {
            border: 2px solid rgba(255,255,255,0.1);
            border-radius: 10px;
            background: rgba(0,0,0,0.3);
            box-shadow: 0 8px 32px rgba(0,0,0,0.3);
        }
        .controls {
            margin-top: 15px;
            font-size: 0.9em;
            opacity: 0.7;
        }
        .score {
            position: absolute;
            top: 10px;
            right: 10px;
            font-size: 1.2em;
            font-weight: bold;
            color: #667eea;
        }
        .footer {
            margin-top: 30px;
            font-size: 0.9em;
            opacity: 0.6;
        }
        .footer a {
            color: #667eea;
            text-decoration: none;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="logo">🐙</div>
        <h1>Tako is Under Maintenance</h1>
        <p class="subtitle">We're deploying updates. Play a game while you wait!</p>
        
        <div class="game-container">
            <div class="score" id="score">Score: 0</div>
            <canvas id="gameCanvas" width="400" height="500"></canvas>
            <div class="controls">
                🎮 Arrow keys, touch, or mouse to move | 🎯 Auto-shoot | ⚡ Gets harder as you progress!
            </div>
        </div>
        
        <div class="footer">
            Powered by <a href="https://github.com/redentordev/tako-cli" target="_blank">Tako CLI</a> 🚀
        </div>
    </div>

    <script>
        const canvas = document.getElementById('gameCanvas');
        const ctx = canvas.getContext('2d');
        const scoreEl = document.getElementById('score');
        
        // Game state
        let score = 0;
        let gameOver = false;
        
        // Player (Tako octopus)
        const player = {
            x: canvas.width / 2,
            y: canvas.height - 60,
            width: 30,
            height: 30,
            speed: 3
        };
        
        // Arrays for game objects
        const bullets = [];
        const enemies = [];
        const particles = [];
        
        // Input handling (keyboard)
        const keys = {};
        window.addEventListener('keydown', (e) => keys[e.key] = true);
        window.addEventListener('keyup', (e) => keys[e.key] = false);
        
        // Touch/mouse controls for mobile
        let isTouching = false;
        
        function handlePointerMove(clientX) {
            const rect = canvas.getBoundingClientRect();
            const x = clientX - rect.left;
            const canvasX = (x / rect.width) * canvas.width;
            player.x = Math.max(0, Math.min(canvas.width - player.width, canvasX - player.width / 2));
        }
        
        canvas.addEventListener('touchstart', (e) => {
            e.preventDefault();
            isTouching = true;
            handlePointerMove(e.touches[0].clientX);
        });
        
        canvas.addEventListener('touchmove', (e) => {
            e.preventDefault();
            if (isTouching) {
                handlePointerMove(e.touches[0].clientX);
            }
        });
        
        canvas.addEventListener('touchend', () => {
            isTouching = false;
        });
        
        canvas.addEventListener('mousemove', (e) => {
            if (e.buttons === 1) { // Left mouse button held
                handlePointerMove(e.clientX);
            }
        });
        
        // Shoot bullets automatically
        setInterval(() => {
            if (!gameOver) {
                bullets.push({
                    x: player.x + player.width / 2,
                    y: player.y,
                    width: 4,
                    height: 10,
                    speed: 7
                });
            }
        }, 300);
        
        // Progressive difficulty system
        let baseEnemySpeed = 1;
        let enemySpawnInterval = 1500;
        let difficultyLevel = 1;
        
        function spawnEnemy() {
            if (!gameOver) {
                // Speed increases with score
                const speedVariation = Math.random() * 0.5;
                const currentSpeed = baseEnemySpeed + (score / 100) + speedVariation;
                
                enemies.push({
                    x: Math.random() * (canvas.width - 30),
                    y: -30,
                    width: 30,
                    height: 30,
                    speed: Math.min(currentSpeed, 4) // Cap at 4 for playability
                });
                
                // Schedule next spawn with decreasing interval
                const nextInterval = Math.max(500, enemySpawnInterval - (score / 20));
                setTimeout(spawnEnemy, nextInterval);
            }
        }
        spawnEnemy();
        
        // Increase base difficulty every 15 seconds
        setInterval(() => {
            if (!gameOver) {
                difficultyLevel++;
                baseEnemySpeed = Math.min(baseEnemySpeed + 0.15, 2.5);
                enemySpawnInterval = Math.max(enemySpawnInterval - 100, 600);
            }
        }, 15000);
        
        // Create explosion particles
        function createExplosion(x, y) {
            for (let i = 0; i < 8; i++) {
                particles.push({
                    x, y,
                    vx: (Math.random() - 0.5) * 4,
                    vy: (Math.random() - 0.5) * 4,
                    life: 30
                });
            }
        }
        
        // Game loop
        function update() {
            if (gameOver) return;
            
            // Move player
            if (keys['ArrowLeft'] && player.x > 0) player.x -= player.speed;
            if (keys['ArrowRight'] && player.x < canvas.width - player.width) player.x += player.speed;
            
            // Move bullets
            bullets.forEach((bullet, i) => {
                bullet.y -= bullet.speed;
                if (bullet.y < 0) bullets.splice(i, 1);
            });
            
            // Move enemies
            enemies.forEach((enemy, i) => {
                enemy.y += enemy.speed;
                if (enemy.y > canvas.height) {
                    enemies.splice(i, 1);
                    gameOver = true;
                }
            });
            
            // Collision detection
            bullets.forEach((bullet, bi) => {
                enemies.forEach((enemy, ei) => {
                    if (bullet.x > enemy.x && bullet.x < enemy.x + enemy.width &&
                        bullet.y > enemy.y && bullet.y < enemy.y + enemy.height) {
                        bullets.splice(bi, 1);
                        enemies.splice(ei, 1);
                        score += 10;
                        scoreEl.textContent = 'Score: ' + score + ' | Level: ' + difficultyLevel;
                        createExplosion(enemy.x + enemy.width / 2, enemy.y + enemy.height / 2);
                    }
                });
            });
            
            // Update particles
            particles.forEach((p, i) => {
                p.x += p.vx;
                p.y += p.vy;
                p.life--;
                if (p.life <= 0) particles.splice(i, 1);
            });
        }
        
        function draw() {
            // Clear canvas
            ctx.fillStyle = 'rgba(0, 0, 0, 0.1)';
            ctx.fillRect(0, 0, canvas.width, canvas.height);
            
            // Draw particles
            particles.forEach(p => {
                ctx.fillStyle = 'rgba(102, 126, 234, ' + (p.life / 30) + ')';
                ctx.fillRect(p.x, p.y, 3, 3);
            });
            
            // Draw player (Tako)
            ctx.font = '30px Arial';
            ctx.fillText('🐙', player.x - 5, player.y + 25);
            
            // Draw bullets
            ctx.fillStyle = '#667eea';
            bullets.forEach(bullet => {
                ctx.fillRect(bullet.x, bullet.y, bullet.width, bullet.height);
            });
            
            // Draw enemies
            ctx.font = '30px Arial';
            enemies.forEach(enemy => {
                ctx.fillText('👾', enemy.x, enemy.y + 25);
            });
            
            // Game over screen
            if (gameOver) {
                ctx.fillStyle = 'rgba(0, 0, 0, 0.7)';
                ctx.fillRect(0, 0, canvas.width, canvas.height);
                ctx.fillStyle = '#fff';
                ctx.font = 'bold 40px Arial';
                ctx.textAlign = 'center';
                ctx.fillText('Game Over!', canvas.width / 2, canvas.height / 2 - 20);
                ctx.font = '20px Arial';
                ctx.fillText('Final Score: ' + score, canvas.width / 2, canvas.height / 2 + 20);
                ctx.font = '16px Arial';
                ctx.fillText('Refresh to play again', canvas.width / 2, canvas.height / 2 + 50);
                ctx.textAlign = 'left';
            }
        }
        
        function gameLoop() {
            update();
            draw();
            requestAnimationFrame(gameLoop);
        }
        
        gameLoop();
    </script>
</body>
</html>`
		if verbose {
			fmt.Fprintf(out, "Using default maintenance page\n")
			fmt.Fprintf(out, "Tip: Create a maintenance.html file in your project to customize this page\n")
		}
	}

	// Prepare maintenance page content for the container.
	fmt.Fprintf(out, "→ Creating maintenance page...\n")
	encodedHTML := base64.StdEncoding.EncodeToString([]byte(maintenanceHTML))

	// Deploy maintenance container and route-manifest proxy config.
	fmt.Fprintf(out, "→ Deploying maintenance container...\n")

	containerName := maintenanceContainerName(cfg.Project.Name, envName, maintenanceService)
	containerAlias := runtimeid.ContainerAlias(cfg.Project.Name, envName, maintenanceTakodServiceName(maintenanceService), 1)
	networkName := maintenanceNetworkName(cfg.Project.Name, envName)
	socket := takodSocketFromConfig(cfg)
	command := fmt.Sprintf("printf %%s %s | base64 -d > /usr/share/nginx/html/index.html && nginx -g 'daemon off;'", encodedHTML)
	request := takod.ReconcileServiceRequest{
		Project:      cfg.Project.Name,
		Environment:  envName,
		Service:      maintenanceTakodServiceName(maintenanceService),
		Image:        maintenanceImage,
		PullImage:    true,
		Restart:      "unless-stopped",
		Network:      networkName,
		NetworkAlias: containerAlias,
		Command:      config.StringValue(command),
		Containers: []takod.ContainerSpec{
			{Name: containerName},
		},
	}

	results := runMaintenanceNodeActions(cfg.Servers, targetServers, func(serverName string, server config.ServerConfig) error {
		dynamicConfig, renderErr := renderMaintenanceProxyConfig(cfg.Project.Name, envName, maintenanceService, service.Proxy, containerAlias, server.ClusterID)
		if renderErr != nil {
			return renderErr
		}
		return enableMaintenanceOnRuntimeNode(cmd.Context(), cfg, runtimeFactory, serverName, socket, envName, maintenanceService, dynamicConfig, request)
	})
	nodeErrors := printMaintenanceNodeResults(out, "Enabling", "enabled", results)
	ack := maintenanceActionResult(cfg, envName, engine.ActionMaintenanceEnable, maintenanceService, results)
	if len(nodeErrors) > 0 {
		err := fmt.Errorf("maintenance mode failed on %d/%d node(s): %s", len(nodeErrors), len(targetServers), strings.Join(nodeErrors, "; "))
		ack.Error = err.Error()
		if emitErr := emitResultDocument(ack); emitErr != nil {
			return emitErr
		}
		return err
	}

	fmt.Fprintf(out, "✓ Maintenance mode enabled for %s\n", maintenanceService)
	fmt.Fprintf(out, "\nThe service is now showing a maintenance page to visitors.\n")
	fmt.Fprintf(out, "Service containers are still running in the background.\n")
	fmt.Fprintf(out, "\nTo restore normal operation: tako live --service %s\n", maintenanceService)

	return emitResultDocument(ack)
}

type maintenanceNodeAction func(serverName string, server config.ServerConfig) error

type maintenanceNodeResult struct {
	index      int
	serverName string
	host       string
	err        error
}

func runMaintenanceNodeActions(servers map[string]config.ServerConfig, targetServers []string, action maintenanceNodeAction) []maintenanceNodeResult {
	resultCh := make(chan maintenanceNodeResult, len(targetServers))
	var wg sync.WaitGroup

	for index, serverName := range targetServers {
		server, ok := servers[serverName]
		if !ok {
			resultCh <- maintenanceNodeResult{
				index:      index,
				serverName: serverName,
				err:        fmt.Errorf("server not found in configuration"),
			}
			continue
		}

		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			resultCh <- maintenanceNodeResult{
				index:      index,
				serverName: serverName,
				host:       server.Host,
				err:        action(serverName, server),
			}
		}(index, serverName, server)
	}

	wg.Wait()
	close(resultCh)

	results := make([]maintenanceNodeResult, len(targetServers))
	for result := range resultCh {
		results[result.index] = result
	}
	return results
}

func printMaintenanceNodeResults(out io.Writer, actionLabel string, successLabel string, results []maintenanceNodeResult) []string {
	var nodeErrors []string
	for _, result := range results {
		fmt.Fprintf(out, "→ %s on %s (%s)...\n", actionLabel, result.serverName, result.host)
		if result.err != nil {
			nodeErrors = append(nodeErrors, fmt.Sprintf("%s: %v", result.serverName, result.err))
			fmt.Fprintf(out, "  failed: %v\n", result.err)
			continue
		}
		fmt.Fprintf(out, "  %s\n", successLabel)
	}
	return nodeErrors
}

// maintenanceActionResult builds the minimal acknowledgement document for a
// maintenance/live node fanout.
func maintenanceActionResult(cfg *config.Config, envName string, action string, service string, results []maintenanceNodeResult) engine.ActionResult {
	ack := engine.ActionResult{
		APIVersion:  takoapi.APIVersionCurrent,
		Kind:        engine.KindActionResult,
		Project:     cfg.Project.Name,
		Environment: envName,
		Action:      action,
		Service:     service,
		Servers:     []engine.ActionNodeOutcome{},
	}
	failures := 0
	for _, result := range results {
		outcome := engine.ActionNodeOutcome{Server: result.serverName, Host: result.host, Done: result.err == nil}
		if result.err != nil {
			outcome.Error = result.err.Error()
			failures++
		}
		ack.Servers = append(ack.Servers, outcome)
	}
	switch {
	case failures == 0:
		ack.Outcome = engine.ActionOutcomeOK
	case failures == len(results):
		ack.Outcome = engine.ActionOutcomeFailed
	default:
		ack.Outcome = engine.ActionOutcomePartial
	}
	return ack
}

func runMaintenanceWithClient(pool sshClientProvider, server config.ServerConfig, execute func(*ssh.Client) error) error {
	if pool == nil {
		return fmt.Errorf("ssh pool is not initialized")
	}
	client, err := pool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}
	return execute(client)
}

func enableMaintenanceOnNode(cfg *config.Config, pool sshClientProvider, server config.ServerConfig, socket string, envName string, serviceName string, dynamicConfig []byte, request takod.ReconcileServiceRequest) error {
	return runMaintenanceWithClient(pool, server, func(client *ssh.Client) error {
		if err := writeMaintenanceProxyConfig(client, socket, cfg.Project.Name, envName, serviceName, dynamicConfig); err != nil {
			return fmt.Errorf("failed to write maintenance proxy config: %w", err)
		}
		if _, err := takodclient.RequestJSON(client, socket, "POST", "/v1/reconcile-service", request); err != nil {
			return fmt.Errorf("failed to reconcile maintenance container: %w", err)
		}
		return nil
	})
}

func enableMaintenanceOnRuntimeNode(ctx context.Context, cfg *config.Config, factory *nodeclient.Factory, serverName string, socket string, envName string, serviceName string, dynamicConfig []byte, request takod.ReconcileServiceRequest) error {
	client, _, err := factory.Client(ctx, serverName)
	if err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}
	if err := writeMaintenanceProxyConfig(client, socket, cfg.Project.Name, envName, serviceName, dynamicConfig); err != nil {
		return fmt.Errorf("failed to write maintenance proxy config: %w", err)
	}
	if _, err := takodclient.RequestJSONWithContext(ctx, client, socket, "POST", "/v1/reconcile-service", request); err != nil {
		return fmt.Errorf("failed to reconcile maintenance container: %w", err)
	}
	return nil
}

func renderMaintenanceProxyConfig(project string, environment string, serviceName string, proxy *config.ProxyConfig, containerAlias string, clusterID ...string) ([]byte, error) {
	hosts, err := maintenanceHosts(proxy)
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("maintenance proxy requires at least one host")
	}

	maintenanceService := maintenanceTakodServiceName(serviceName)
	upstream := "http://" + containerAlias + ":80"
	manifestClusterID := ""
	if len(clusterID) > 0 {
		manifestClusterID = strings.TrimSpace(clusterID[0])
	}
	manifest := takod.ProxyRouteManifest{
		Version:     2,
		Project:     project,
		Environment: environment,
		ClusterID:   manifestClusterID,
		Routes: []takod.ProxyRoute{
			{
				Service:    maintenanceService,
				Domains:    hosts,
				Upstreams:  []string{upstream},
				Priority:   100,
				Visibility: proxy.EffectiveVisibility(),
				Destinations: []takod.ProxyDestination{{
					Kind: takod.ProxyDestinationRuntimeAlias, URL: upstream, Project: project,
					Environment: environment, Service: maintenanceService, Slot: 1,
					ContainerPort: 80, HostPort: 80,
				}},
			},
		},
	}

	return json.MarshalIndent(manifest, "", "  ")
}

func maintenanceHosts(proxy *config.ProxyConfig) ([]string, error) {
	if proxy == nil {
		return nil, nil
	}
	rawHosts := append([]string(nil), proxy.GetAllHosts()...)
	rawHosts = append(rawHosts, proxy.GetRedirectDomains()...)
	hosts := make([]string, 0, len(rawHosts))
	for _, host := range rawHosts {
		normalized, err := config.NormalizeProxyDomain(host)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(normalized, "*.") {
			return nil, fmt.Errorf("wildcard proxy domain %q is not supported by maintenance mode", normalized)
		}
		hosts = append(hosts, normalized)
	}
	return hosts, nil
}

func writeMaintenanceProxyConfig(client any, socket string, project string, environment string, serviceName string, data []byte) error {
	_, err := takodclient.RequestJSON(client, socket, "PUT", "/v1/proxy-file", takod.ProxyFileRequest{
		Name:    maintenanceProxyConfigFileName(project, environment, serviceName),
		Content: string(data),
	})
	if err != nil {
		return err
	}
	return nil
}

func maintenanceProxyConfigFileName(project string, environment string, serviceName string) string {
	return runtimeid.MaintenanceProxyConfigFileName(project, environment, serviceName)
}

func maintenanceContainerName(project string, environment string, serviceName string) string {
	return runtimeid.ContainerName(project, environment, maintenanceTakodServiceName(serviceName), 1)
}

func maintenanceNetworkName(project string, environment string) string {
	return runtimeid.NetworkName(project, environment)
}

func maintenanceTakodServiceName(serviceName string) string {
	return serviceName + "-maintenance"
}
