# Ruby Sinatra Example

A simple and elegant Ruby web application using the Sinatra framework, deployed with Tako CLI.

## What's Included

- **Sinatra 4.0** - Simple DSL for creating web applications
- **Puma** - High-performance web server
- **Automatic HTTPS** - Let's Encrypt SSL certificates
- **Health checks** - Built-in health endpoint

## Features

- Simple web interface showing deployment info
- JSON API endpoint
- Health check endpoint for monitoring
- Production-ready configuration

## Quick Start

1. **Set environment variables:**
   ```bash
   cp .env.example .env
   # Edit .env with your server details
   ```

2. **Deploy:**
   ```bash
   tako deploy -e production
   ```

3. **Access your app:**
   - Main page: `https://ruby.YOUR-IP.sslip.io`
   - API endpoint: `https://ruby.YOUR-IP.sslip.io/api`
   - Health check: `https://ruby.YOUR-IP.sslip.io/health`

## Deployment

The application is deployed as a containerized service with:
- Alpine Linux base image for minimal size
- Automatic health checks
- Zero-downtime deployments
- HTTPS with Let's Encrypt

✅ **Status: LIVE and Working** - https://ruby.95.216.194.236.sslip.io
