FROM node:20-alpine

WORKDIR /app

# Copy package files
COPY package*.json ./

# Install dependencies (use legacy peer deps to avoid conflicts)
RUN npm install --legacy-peer-deps

# Copy all source files
COPY . .

# Build the application
RUN npm run build

ENV NODE_ENV=production
ENV PORT=3000

EXPOSE 3000

# Start the server
CMD ["npm", "start"]
