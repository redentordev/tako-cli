FROM ruby:3.3-alpine

# Install dependencies
RUN apk add --no-cache \
    build-base \
    tzdata

WORKDIR /app

# Copy Gemfile and install gems
COPY Gemfile* ./
RUN bundle install --without development test

# Copy application
COPY . .

# Create necessary directories
RUN mkdir -p tmp/pids && chmod -R 755 tmp

# Expose port
EXPOSE 3000

# Start the application
CMD ["bundle", "exec", "rackup", "-o", "0.0.0.0", "-p", "3000"]
