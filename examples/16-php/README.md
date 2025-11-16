# PHP Example

This example demonstrates deploying a vanilla PHP application with Tako CLI.

PHP is a popular general-purpose scripting language especially suited for web development.

## Features

- 🐘 PHP 8.3 with Apache
- 🚀 Simple routing
- 📦 No dependencies
- 🌍 Production-ready
- 🔄 Dynamic content generation

## Local Development

You can run this locally with PHP's built-in server:

```bash
php -S localhost:8000
```

Or use Docker:

```bash
docker build -t php-app .
docker run -p 8000:80 php-app
```

Visit http://localhost:8000

## Deploy with Tako CLI

```bash
# Deploy
tako deploy

# View logs
tako logs

# Check status
tako ps
```

## API Endpoints

- `GET /` - Home page with server info
- `GET /api/hello` - JSON API endpoint
- `GET /api/info` - PHP configuration info

## Adding Composer Dependencies

To add Composer dependencies, create a `composer.json` file and update the Dockerfile:

```dockerfile
# Install Composer
COPY --from=composer:latest /usr/bin/composer /usr/bin/composer

# Install dependencies
COPY composer.json composer.lock ./
RUN composer install --no-dev --optimize-autoloader
```

## Learn More

- [PHP Documentation](https://www.php.net/docs.php)
- [PHP The Right Way](https://phptherightway.com/)
- [Tako CLI Documentation](https://github.com/redentordev/tako-cli)
