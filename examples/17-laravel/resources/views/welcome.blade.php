<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Laravel on Tako CLI</title>
    <style>
        * {
            box-sizing: border-box;
            margin: 0;
            padding: 0;
        }

        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
            background: linear-gradient(135deg, #FF2D20 0%, #FF6B6B 100%);
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
            color: #FF2D20;
            font-size: 2.5em;
            margin-bottom: 10px;
        }

        .version {
            color: #666;
            font-size: 0.9em;
            margin-bottom: 20px;
        }

        .badges {
            margin: 20px 0;
        }

        .badge {
            display: inline-block;
            padding: 8px 16px;
            background: #FF2D20;
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
            color: #FF2D20;
            margin-bottom: 10px;
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
            color: #FF2D20;
            font-weight: bold;
            margin-right: 10px;
        }

        .links {
            margin-top: 30px;
            text-align: center;
        }

        a {
            display: inline-block;
            color: #FF2D20;
            text-decoration: none;
            margin: 0 15px;
            padding: 10px 20px;
            border: 2px solid #FF2D20;
            border-radius: 8px;
            transition: all 0.2s;
        }

        a:hover {
            background: #FF2D20;
            color: white;
        }
    </style>
</head>
<body>
    <div class="container">
        <h1>🔺 Laravel on Tako CLI</h1>
        <p class="version">Version {{ app()->version() }}</p>
        <p>The PHP Framework for Web Artisans</p>

        <div class="badges">
            <span class="badge">🎨 Elegant</span>
            <span class="badge">⚡ Fast</span>
            <span class="badge">🛠️ Full-stack</span>
            <span class="badge">📦 Batteries-included</span>
        </div>

        <div class="info-box">
            <h3>Environment Info</h3>
            <p><strong>Environment:</strong> {{ app()->environment() }}</p>
            <p><strong>PHP Version:</strong> {{ PHP_VERSION }}</p>
            <p><strong>Server Time:</strong> {{ now()->toDateTimeString() }}</p>
        </div>

        <div class="features">
            <h2>Features</h2>
            <ul>
                <li>Eloquent ORM for database operations</li>
                <li>Blade templating engine</li>
                <li>Powerful routing system</li>
                <li>Built-in authentication</li>
                <li>Queue system for background jobs</li>
                <li>Task scheduling</li>
                <li>Real-time broadcasting</li>
                <li>API resources</li>
            </ul>
        </div>

        <div class="links">
            <a href="/api/hello">API Example</a>
            <a href="/api/status">Status Check</a>
            <a href="https://laravel.com/docs" target="_blank">Laravel Docs</a>
        </div>
    </div>
</body>
</html>
