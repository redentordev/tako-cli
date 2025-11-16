# Example 2: Web + Database

This example demonstrates a web application with a PostgreSQL database, showcasing persistent data storage and service-to-service communication.

## Features Demonstrated

- **Multiple Services**: Web server + PostgreSQL database
- **Service Discovery**: Web app connects to database using service name `postgres`
- **Persistent Storage**: Database data persists across deployments
- **Database Initialization**: Automatic table creation on startup
- **Environment Variables**: Database credentials passed securely

## Files

- `tako.yaml` - Two services: web (public) and postgres (internal)
- `Dockerfile` - Node.js with PostgreSQL client
- `package.json` - Express + pg dependencies
- `index.js` - Web app with visitor tracking

## Configuration Highlights

```yaml
services:
  web:
    env:
      DATABASE_URL: postgresql://postgres:dbpassword123@postgres:5432/visitor_db

  postgres:
    image: postgres:15
    persistent: true  # Data persists across redeployments
    volumes:
      - /var/lib/postgresql/data
```

Key points:
- The web service connects to the database using the hostname `postgres` (service name)
- `persistent: true` ensures data is not lost on redeployment
- Database credentials are configured via environment variables

## How to Deploy

1. Update `tako.yaml` with your server and domain:
   ```bash
   export SERVER_HOST=your.server.ip
   ```

2. Deploy:
   ```bash
   start deploy prod
   ```

3. The application will:
   - Start PostgreSQL container
   - Start web container
   - Initialize the visitors table
   - Begin tracking page visits

## What It Does

- Tracks every page visit in PostgreSQL
- Displays total visit count
- Shows 10 most recent visits with timestamps and IP addresses
- Provides health check endpoint that verifies database connection

## Database Schema

```sql
CREATE TABLE visitors (
  id SERIAL PRIMARY KEY,
  visit_time TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  ip_address VARCHAR(50)
);
```

## Testing Locally

```bash
# Start PostgreSQL with Docker
docker run -d \
  --name postgres \
  -e POSTGRES_DB=visitor_db \
  -e POSTGRES_USER=postgres \
  -e POSTGRES_PASSWORD=dbpassword123 \
  -p 5432:5432 \
  postgres:15

# Install dependencies
npm install

# Set database URL
export DATABASE_URL=postgresql://postgres:dbpassword123@localhost:5432/visitor_db

# Run the server
npm start

# Visit http://localhost:3000
```

## Service Communication

The web service communicates with PostgreSQL using Docker's internal networking:

```javascript
const pool = new Pool({
  connectionString: 'postgresql://postgres:dbpassword123@postgres:5432/visitor_db'
});
```

The hostname `postgres` is automatically resolved to the PostgreSQL container's IP address within the Docker network.

## Persistent Data

With `persistent: true`, the database volume is preserved even when:
- The service is redeployed
- The container is recreated
- The application is updated

This ensures visitor data is never lost.
