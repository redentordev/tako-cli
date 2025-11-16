# Laravel Example

This example demonstrates deploying a [Laravel](https://laravel.com) application with Tako CLI.

Laravel is a web application framework with expressive, elegant syntax.

## Features

- 🎨 Elegant syntax and structure
- ⚡ Fast development
- 🛠️ Full-stack framework
- 📦 Batteries-included
- 🗃️ Eloquent ORM
- 🎭 Blade templating
- 🔐 Built-in authentication

## Initial Setup

To create a fresh Laravel application:

```bash
composer create-project laravel/laravel .
```

Or use this minimal example structure provided here.

## Local Development

```bash
composer install
php artisan key:generate
php artisan serve
```

Visit http://localhost:8000

## Deploy with Tako CLI

```bash
# Set up environment variables
export SERVER_HOST=your.server.ip

# Deploy
tako deploy

# View logs
tako logs

# Check status
tako ps
```

## Project Structure

```
app/               # Application code
├── Http/          # Controllers, middleware
├── Models/        # Eloquent models
└── ...

routes/
├── web.php        # Web routes
└── api.php        # API routes

resources/
├── views/         # Blade templates
└── ...

database/
├── migrations/    # Database migrations
└── ...
```

## Important Notes

1. **Application Key**: In production, set `APP_KEY` via environment variable instead of generating it in Dockerfile
2. **Database**: This example uses SQLite. For MySQL/PostgreSQL, add database service
3. **Storage**: The example uses a volume for persistent storage
4. **Cache**: Configure Redis for better performance in production

## Adding a Database

To add MySQL/PostgreSQL, update `tako.yaml`:

```yaml
services:
  web:
    # ... existing config
    depends_on:
      - db

  db:
    image: mysql:8.0
    env:
      MYSQL_ROOT_PASSWORD: secret
      MYSQL_DATABASE: laravel
    volumes:
      - db-data:/var/lib/mysql

volumes:
  db-data:
```

## Learn More

- [Laravel Documentation](https://laravel.com/docs)
- [Laracasts](https://laracasts.com)
- [Laravel News](https://laravel-news.com)
- [Tako CLI Documentation](https://github.com/redentordev/tako-cli)
