FROM node:20-alpine

WORKDIR /app

# Copy package.json
COPY package.json ./

# Install dependencies
RUN npm install

# Copy source
COPY . .

# Build
RUN npm run build

# Remove dev dependencies
RUN npm prune --production

ENV NODE_ENV=production
ENV PORT=3000
EXPOSE 3000

CMD ["node", "build"]
