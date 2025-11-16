<?php
// Simple PHP application with routing

$requestUri = $_SERVER['REQUEST_URI'];
$requestMethod = $_SERVER['REQUEST_METHOD'];

// Remove query string from URI
$uri = strtok($requestUri, '?');

// Simple router
switch ($uri) {
    case '/':
        renderHomePage();
        break;
    case '/api/hello':
        renderAPIResponse();
        break;
    case '/api/info':
        renderInfoResponse();
        break;
    default:
        render404();
        break;
}

function renderHomePage() {
    $serverTime = date('c');
    ?>
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>PHP on Tako CLI</title>
    <style>
        * {
            box-sizing: border-box;
            margin: 0;
            padding: 0;
        }

        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
            background: linear-gradient(135deg, #4F46E5 0%, #7C3AED 100%);
            min-height: 100vh;
            display: flex;
            justify-content: center;
            align-items: center;
            padding: 20px;
        }

        .container {
            background: white;
            border-radius: 20px;
            padding: 40px;
            max-width: 700px;
            box-shadow: 0 20px 60px rgba(0, 0, 0, 0.3);
        }

        h1 {
            color: #4F46E5;
            font-size: 2.5em;
            margin-bottom: 10px;
        }

        .badges {
            margin: 20px 0;
        }

        .badge {
            display: inline-block;
            padding: 8px 16px;
            background: #4F46E5;
            color: white;
            border-radius: 20px;
            margin: 5px;
            font-size: 0.9em;
        }

        .info-box {
            background: #f5f5f5;
            padding: 20px;
            border-radius: 15px;
            margin: 20px 0;
        }

        .info-box h3 {
            color: #4F46E5;
            margin-bottom: 10px;
        }

        .info-item {
            display: flex;
            justify-content: space-between;
            padding: 8px 0;
            border-bottom: 1px solid #ddd;
        }

        .info-item:last-child {
            border-bottom: none;
        }

        .label {
            font-weight: 600;
            color: #666;
        }

        .value {
            font-family: monospace;
            color: #333;
        }

        .features ul {
            list-style: none;
            padding: 0;
        }

        .features li {
            padding: 10px 0;
            border-bottom: 1px solid #eee;
        }

        .features li:before {
            content: "✓ ";
            color: #4F46E5;
            font-weight: bold;
            margin-right: 10px;
        }

        .links {
            margin-top: 30px;
            text-align: center;
        }

        a {
            display: inline-block;
            color: #4F46E5;
            text-decoration: none;
            margin: 0 15px;
            padding: 10px 20px;
            border: 2px solid #4F46E5;
            border-radius: 8px;
            transition: all 0.2s;
        }

        a:hover {
            background: #4F46E5;
            color: white;
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>🐘 PHP on Tako CLI</h1>
        <p>The web language that powers the internet</p>

        <div class="badges">
            <span class="badge">🐘 PHP</span>
            <span class="badge">🚀 Fast</span>
            <span class="badge">📦 Mature</span>
            <span class="badge">🌍 Ubiquitous</span>
        </div>

        <div class="info-box">
            <h3>Server Information</h3>
            <div class="info-item">
                <span class="label">PHP Version:</span>
                <span class="value"><?php echo PHP_VERSION; ?></span>
            </div>
            <div class="info-item">
                <span class="label">Server Time:</span>
                <span class="value"><?php echo $serverTime; ?></span>
            </div>
            <div class="info-item">
                <span class="label">Server Software:</span>
                <span class="value"><?php echo $_SERVER['SERVER_SOFTWARE'] ?? 'Unknown'; ?></span>
            </div>
        </div>

        <div class="features">
            <h2>Features</h2>
            <ul>
                <li>Server-side scripting</li>
                <li>Dynamic content generation</li>
                <li>Database integration</li>
                <li>Session management</li>
                <li>File handling</li>
                <li>Extensive ecosystem</li>
            </ul>
        </div>

        <div class="links">
            <a href="/api/hello">API Example</a>
            <a href="/api/info">PHP Info</a>
            <a href="https://php.net" target="_blank">PHP Docs</a>
        </div>
    </div>
</body>
</html>
    <?php
}

function renderAPIResponse() {
    header('Content-Type: application/json');
    echo json_encode([
        'message' => 'Hello from PHP!',
        'framework' => 'PHP',
        'deployed_with' => 'Tako CLI',
        'timestamp' => date('c'),
        'php_version' => PHP_VERSION,
        'features' => [
            'Server-side scripting',
            'Dynamic content',
            'Database integration',
            'Session management'
        ]
    ], JSON_PRETTY_PRINT);
}

function renderInfoResponse() {
    header('Content-Type: application/json');
    echo json_encode([
        'php_version' => PHP_VERSION,
        'server' => $_SERVER['SERVER_SOFTWARE'] ?? 'Unknown',
        'extensions' => get_loaded_extensions(),
        'memory_limit' => ini_get('memory_limit'),
        'max_execution_time' => ini_get('max_execution_time'),
        'timestamp' => date('c')
    ], JSON_PRETTY_PRINT);
}

function render404() {
    http_response_code(404);
    header('Content-Type: text/html');
    echo '<!DOCTYPE html>
<html>
<head>
    <title>404 Not Found</title>
    <style>
        body {
            font-family: sans-serif;
            text-align: center;
            padding: 50px;
            background: linear-gradient(135deg, #4F46E5 0%, #7C3AED 100%);
            color: white;
        }
        h1 { font-size: 4em; }
        a { color: white; }
    </style>
</head>
<body>
    <h1>404</h1>
    <p>Page not found</p>
    <a href="/">← Go home</a>
</body>
</html>';
}
?>
