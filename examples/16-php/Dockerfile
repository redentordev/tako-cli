FROM php:8.3-apache

# Enable Apache mod_rewrite
RUN a2enmod rewrite

# Copy application files
COPY . /var/www/html/

# Set proper permissions
RUN chown -R www-data:www-data /var/www/html \
    && chmod -R 755 /var/www/html

# Configure Apache and PHP handler
RUN echo '<Directory /var/www/html>\n\
    Options Indexes FollowSymLinks\n\
    AllowOverride All\n\
    Require all granted\n\
    DirectoryIndex index.php index.html\n\
</Directory>\n\
<FilesMatch \.php$>\n\
    SetHandler application/x-httpd-php\n\
</FilesMatch>' > /etc/apache2/conf-available/docker-php.conf \
    && a2enconf docker-php

EXPOSE 80

CMD ["apache2-foreground"]
