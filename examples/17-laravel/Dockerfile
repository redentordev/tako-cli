FROM php:8.3-fpm-alpine AS base

# Install system dependencies
RUN apk add --no-cache \
    nginx \
    supervisor \
    sqlite \
    sqlite-dev \
    git \
    unzip

# Install PHP extensions
RUN docker-php-ext-install pdo_mysql pdo_sqlite

# Install Composer
COPY --from=composer:latest /usr/bin/composer /usr/bin/composer

WORKDIR /var/www/html

# Create a fresh Laravel installation
RUN composer create-project laravel/laravel . "^11.0" --no-interaction --prefer-dist

# Create .env file from .env.example
RUN cp .env.example .env

# Generate application key properly
RUN php artisan key:generate --force

# Create SQLite database and set permissions
RUN mkdir -p storage/database storage/framework/cache storage/framework/sessions storage/framework/views storage/logs bootstrap/cache && \
    touch storage/database/database.sqlite && \
    chown -R www-data:www-data storage bootstrap/cache && \
    chmod -R 775 storage bootstrap/cache && \
    chmod 664 storage/database/database.sqlite

# Configure Nginx
COPY <<EOF /etc/nginx/nginx.conf
user www-data;
worker_processes auto;
error_log /var/log/nginx/error.log warn;
pid /var/run/nginx.pid;

events {
    worker_connections 1024;
}

http {
    include /etc/nginx/mime.types;
    default_type application/octet-stream;
    sendfile on;
    keepalive_timeout 65;

    server {
        listen 8000;
        server_name _;
        root /var/www/html/public;
        index index.php;

        location / {
            try_files \$uri \$uri/ /index.php?\$query_string;
        }

        location ~ \.php$ {
            fastcgi_pass 127.0.0.1:9000;
            fastcgi_index index.php;
            fastcgi_param SCRIPT_FILENAME \$document_root\$fastcgi_script_name;
            include fastcgi_params;
        }
    }
}
EOF

# Supervisor config
COPY <<EOF /etc/supervisor/conf.d/supervisord.conf
[supervisord]
nodaemon=true
user=root

[program:php-fpm]
command=php-fpm
autostart=true
autorestart=true
stdout_logfile=/dev/stdout
stdout_logfile_maxbytes=0
stderr_logfile=/dev/stderr
stderr_logfile_maxbytes=0

[program:nginx]
command=nginx -g 'daemon off;'
autostart=true
autorestart=true
stdout_logfile=/dev/stdout
stdout_logfile_maxbytes=0
stderr_logfile=/dev/stderr
stderr_logfile_maxbytes=0
EOF

EXPOSE 8000

CMD ["/usr/bin/supervisord", "-c", "/etc/supervisor/conf.d/supervisord.conf"]
