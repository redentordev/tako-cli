package cmd

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	maintenanceServer  string
	maintenanceService string
)

const maintenanceImage = "nginx:1.27-alpine"

type maintenanceProxyConfig struct {
	HTTP maintenanceHTTPConfig `yaml:"http"`
}

type maintenanceHTTPConfig struct {
	Routers  map[string]maintenanceRouter         `yaml:"routers"`
	Services map[string]maintenanceTraefikService `yaml:"services"`
}

type maintenanceRouter struct {
	Rule        string          `yaml:"rule"`
	EntryPoints []string        `yaml:"entryPoints"`
	Service     string          `yaml:"service"`
	Priority    int             `yaml:"priority"`
	TLS         *maintenanceTLS `yaml:"tls,omitempty"`
}

type maintenanceTLS struct {
	CertResolver string `yaml:"certResolver"`
}

type maintenanceTraefikService struct {
	LoadBalancer maintenanceLoadBalancer `yaml:"loadBalancer"`
}

type maintenanceLoadBalancer struct {
	Servers        []maintenanceServerURL `yaml:"servers"`
	PassHostHeader bool                   `yaml:"passHostHeader"`
}

type maintenanceServerURL struct {
	URL string `yaml:"url"`
}

var maintenanceCmd = &cobra.Command{
	Use:   "maintenance",
	Short: "Enable maintenance mode for a service",
	Long: `Enable maintenance mode for a public-facing service.

This command deploys a maintenance page container with proxy routing
that takes priority over the main service. The main service continues
running in the background.

If --server is not specified, defaults to the primary environment node.

Custom Maintenance Page:
  Create a 'maintenance.html' file in your project directory to use a custom page.
  If not provided, a simple default page will be used.

To restore normal operation, use 'tako live'.

Examples:
  tako maintenance --service web              # Enable on default server
  tako maintenance --service web --server prod # Enable on specific server

  # With custom page
  echo '<h1>Custom Maintenance</h1>' > maintenance.html
  tako maintenance --service web`,
	RunE: runMaintenance,
}

func init() {
	rootCmd.AddCommand(maintenanceCmd)
	maintenanceCmd.Flags().StringVarP(&maintenanceServer, "server", "s", "", "Node to enable maintenance on (default: primary node)")
	maintenanceCmd.Flags().StringVar(&maintenanceService, "service", "", "Service to put in maintenance mode (required)")
	maintenanceCmd.MarkFlagRequired("service")
}

func runMaintenance(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
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
	if !service.IsPublic() {
		return fmt.Errorf("service %s is not public-facing (no proxy configuration)", maintenanceService)
	}

	// Determine which server to use
	var serverName string
	var server config.ServerConfig

	if maintenanceServer != "" {
		// Use specified server
		var exists bool
		server, exists = cfg.Servers[maintenanceServer]
		if !exists {
			return fmt.Errorf("server %s not found in configuration", maintenanceServer)
		}
		serverName = maintenanceServer
	} else {
		primaryName, err := cfg.GetPrimaryServer(envName)
		if err != nil {
			return fmt.Errorf("failed to get primary node: %w", err)
		}
		serverName = primaryName
		server = cfg.Servers[primaryName]

		if verbose {
			fmt.Printf("Using node: %s (%s)\n", serverName, server.Host)
		}
	}

	// Create SSH client (supports both key and password auth)
	client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
		Host:     server.Host,
		Port:     server.Port,
		User:     server.User,
		SSHKey:   server.SSHKey,
		Password: server.Password,
	})
	if err != nil {
		return fmt.Errorf("failed to create SSH client: %w", err)
	}
	defer client.Close()

	if err := client.Connect(); err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}

	fmt.Printf("🔧 Enabling maintenance mode for %s on %s...\n\n", maintenanceService, serverName)

	// Create maintenance directory
	maintenanceDir := fmt.Sprintf("/opt/%s/maintenance", cfg.Project.Name)
	createDirCmd := fmt.Sprintf("sudo mkdir -p %s", maintenanceDir)
	if _, err := client.Execute(createDirCmd); err != nil {
		return fmt.Errorf("failed to create maintenance directory: %w", err)
	}

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
			fmt.Printf("Using custom maintenance page from ./maintenance.html\n")
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
			fmt.Printf("Using default maintenance page\n")
			fmt.Printf("Tip: Create a maintenance.html file in your project to customize this page\n")
		}
	}

	// Write maintenance page to server
	fmt.Printf("→ Creating maintenance page...\n")

	// Use base64 encoding to safely transfer HTML with special characters
	encodedHTML := base64.StdEncoding.EncodeToString([]byte(maintenanceHTML))

	// Write base64 to temp file first, then decode (handles large files)
	tmpFile := fmt.Sprintf("/tmp/tako_maintenance_%d.b64", time.Now().Unix())
	writeTempCmd := fmt.Sprintf("cat > %s <<'B64EOF'\n%s\nB64EOF", tmpFile, encodedHTML)
	if _, err := client.Execute(writeTempCmd); err != nil {
		return fmt.Errorf("failed to write temp file: %w", err)
	}

	// Decode and move to final location
	decodeCmd := fmt.Sprintf("base64 -d < %s > %s/index.html && rm %s", tmpFile, maintenanceDir, tmpFile)
	if _, err := client.Execute(decodeCmd); err != nil {
		return fmt.Errorf("failed to decode maintenance page: %w", err)
	}

	// Deploy maintenance container and file-provider proxy config.
	// Use priority 100 to ensure it takes precedence over normal service (priority 10)
	fmt.Printf("→ Deploying maintenance container...\n")

	containerName := fmt.Sprintf("%s_%s_maintenance", cfg.Project.Name, maintenanceService)
	networkName := fmt.Sprintf("tako_%s_%s", cfg.Project.Name, envName)

	dynamicConfig, err := renderMaintenanceProxyConfig(cfg.Project.Name, envName, maintenanceService, service.Proxy, containerName)
	if err != nil {
		return err
	}
	if err := writeMaintenanceProxyConfig(client, cfg.Project.Name, envName, maintenanceService, dynamicConfig); err != nil {
		return fmt.Errorf("failed to write maintenance proxy config: %w", err)
	}

	// Create docker run command
	dockerCmd := fmt.Sprintf(
		"docker run -d --name %s --network %s -v %s:/usr/share/nginx/html:ro %s",
		maintenanceShellQuote(containerName),
		maintenanceShellQuote(networkName),
		maintenanceShellQuote(maintenanceDir),
		maintenanceImage,
	)

	// Remove existing maintenance container if any
	client.Execute(fmt.Sprintf("docker rm -f %s 2>/dev/null", containerName))

	// Start maintenance container
	output, err := client.Execute(dockerCmd)
	if err != nil {
		return fmt.Errorf("failed to start maintenance container: %w, output: %s", err, output)
	}

	// Verify container is running
	checkCmd := fmt.Sprintf("docker ps --filter name=%s --format '{{.Status}}'", containerName)
	if output, err := client.Execute(checkCmd); err != nil || output == "" {
		return fmt.Errorf("maintenance container failed to start")
	}

	fmt.Printf("✓ Maintenance mode enabled for %s\n", maintenanceService)
	fmt.Printf("\nThe service is now showing a maintenance page to visitors.\n")
	fmt.Printf("Service containers are still running in the background.\n")
	fmt.Printf("\nTo restore normal operation: tako live --service %s\n", maintenanceService)

	return nil
}

func renderMaintenanceProxyConfig(project string, environment string, serviceName string, proxy *config.ProxyConfig, containerName string) ([]byte, error) {
	domains := maintenanceDomains(proxy)
	if len(domains) == 0 {
		return nil, fmt.Errorf("maintenance proxy requires at least one domain")
	}

	routerBase := sanitizeMaintenanceName(project + "-" + environment + "-" + serviceName + "-maintenance")
	serviceID := routerBase + "-svc"
	cfg := maintenanceProxyConfig{
		HTTP: maintenanceHTTPConfig{
			Routers:  make(map[string]maintenanceRouter),
			Services: make(map[string]maintenanceTraefikService),
		},
	}

	rule := hostRuleForDomains(domains)
	cfg.HTTP.Routers[routerBase+"-https"] = maintenanceRouter{
		Rule:        rule,
		EntryPoints: []string{"websecure"},
		Service:     serviceID,
		Priority:    100,
		TLS:         &maintenanceTLS{CertResolver: "letsencrypt"},
	}
	cfg.HTTP.Routers[routerBase+"-http"] = maintenanceRouter{
		Rule:        rule,
		EntryPoints: []string{"web"},
		Service:     serviceID,
		Priority:    100,
	}
	cfg.HTTP.Services[serviceID] = maintenanceTraefikService{
		LoadBalancer: maintenanceLoadBalancer{
			Servers:        []maintenanceServerURL{{URL: "http://" + containerName + ":80"}},
			PassHostHeader: true,
		},
	}

	return yaml.Marshal(cfg)
}

func maintenanceDomains(proxy *config.ProxyConfig) []string {
	if proxy == nil {
		return nil
	}
	domains := append([]string(nil), proxy.GetAllDomains()...)
	domains = append(domains, proxy.GetRedirectDomains()...)
	return domains
}

func hostRuleForDomains(domains []string) string {
	parts := make([]string, 0, len(domains))
	for _, domain := range domains {
		parts = append(parts, "Host(`"+strings.ReplaceAll(domain, "`", "")+"`)")
	}
	return strings.Join(parts, " || ")
}

func writeMaintenanceProxyConfig(client *ssh.Client, project string, environment string, serviceName string, data []byte) error {
	path := maintenanceProxyConfigPath(project, environment, serviceName)
	tmpPath := fmt.Sprintf("/tmp/%s", maintenanceProxyConfigFileName(project, environment, serviceName))
	if err := client.UploadReader(strings.NewReader(string(data)), tmpPath, 0644); err != nil {
		return err
	}
	cmd := fmt.Sprintf(
		"sudo mkdir -p /etc/tako/proxy/dynamic && sudo mv %s %s && sudo chmod 644 %s",
		maintenanceShellQuote(tmpPath),
		maintenanceShellQuote(path),
		maintenanceShellQuote(path),
	)
	_, err := client.Execute(cmd)
	return err
}

func maintenanceProxyConfigPath(project string, environment string, serviceName string) string {
	return "/etc/tako/proxy/dynamic/" + maintenanceProxyConfigFileName(project, environment, serviceName)
}

func maintenanceProxyConfigFileName(project string, environment string, serviceName string) string {
	return sanitizeMaintenanceName(project+"-"+environment+"-"+serviceName+"-maintenance") + ".yml"
}

func sanitizeMaintenanceName(value string) string {
	replacer := strings.NewReplacer("_", "-", ".", "-", "/", "-", " ", "-")
	return replacer.Replace(strings.ToLower(value))
}

func maintenanceShellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
